package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestObserveAsyncBatchesOnlyBoundedAdjacentCompatibleEvents(t *testing.T) {
	t.Parallel()
	s := newTestSession(t, &fakeWire{}, &fakePromptStore{}, newBlockingTranscriptOpen())
	entered := make(chan struct{})
	release := make(chan struct{})
	s.ObserveAsync(AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		return nil
	}})
	<-entered

	applied := make(chan int, 4)
	newBatch := func(key string, epoch uint64) AdapterEvent {
		count := 1
		return AdapterEvent{
			LegacyApply:       func() error { applied <- count; return nil },
			AsyncBatchKey:     key,
			AsyncBatchEpoch:   epoch,
			AsyncBatchBytes:   1,
			AsyncBatchPayload: 1,
			AsyncBatchMerge:   func(payload any) { count += payload.(int) },
		}
	}
	for i := 0; i < maxAsyncAdapterBatchCount+1; i++ {
		s.ObserveAsync(newBatch("same-message", 1))
	}
	// The key remains identical across the attachment boundary so the explicit
	// epoch check, not an incidental identity change, must split the batches.
	s.ObserveAsync(newBatch("same-message", 2))
	s.ObserveAsync(newBatch("same-message", 2))
	s.ObserveAsync(newBatch("next-message", 2))
	close(release)
	if err := s.Observe(context.Background(), AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	got := []int{<-applied, <-applied, <-applied, <-applied}
	want := []int{maxAsyncAdapterBatchCount, 1, 2, 1}
	if !equalInts(got, want) {
		t.Fatalf("batched apply counts = %v, want %v", got, want)
	}
}

func TestObserveAsyncBatchStopsAtByteLimit(t *testing.T) {
	t.Parallel()
	s := newTestSession(t, &fakeWire{}, &fakePromptStore{}, newBlockingTranscriptOpen())
	entered := make(chan struct{})
	release := make(chan struct{})
	s.ObserveAsync(AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		return nil
	}})
	<-entered

	applied := make(chan int, 2)
	newBatch := func() AdapterEvent {
		count := 1
		return AdapterEvent{
			LegacyApply:       func() error { applied <- count; return nil },
			AsyncBatchKey:     "same-item",
			AsyncBatchEpoch:   1,
			AsyncBatchBytes:   maxAsyncAdapterBatchBytes/2 + 1,
			AsyncBatchPayload: 1,
			AsyncBatchMerge:   func(payload any) { count += payload.(int) },
		}
	}
	s.ObserveAsync(newBatch())
	s.ObserveAsync(newBatch())
	close(release)
	if err := s.Observe(context.Background(), AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	if first, second := <-applied, <-applied; first != 1 || second != 1 {
		t.Fatalf("oversized adjacent events were combined: %d, %d", first, second)
	}
}

func TestObserveAsyncReportsStoreFailure(t *testing.T) {
	t.Parallel()
	turnStore := &fakeTurnStore{err: errors.New("synthetic store failure")}
	reported := make(chan error, 1)
	s := newUnmanagedTestSessionWithConfig(t, &fakeWire{}, &fakePromptStore{}, turnStore, newBlockingTranscriptOpen(), func(err error) {
		reported <- err
	})
	t.Cleanup(s.Close)
	finished := coreturns.Finish{HostedThreadID: "host-1", Provider: "codex", TurnID: "turn-1", Outcome: "completed"}
	s.ObserveAsync(AdapterEvent{TurnEvents: []TurnEvent{{Finished: &finished}}})
	if err := s.Observe(context.Background(), AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if err == nil || err.Error() != "synthetic store failure" {
			t.Fatalf("reported error = %v", err)
		}
	default:
		t.Fatal("async store failure was not reported")
	}
}

func TestConcurrentEventsApplyInArrivalOrderWithoutInterleaving(t *testing.T) {
	t.Parallel()
	sink := newBlockingTranscript()
	s := newTestSession(t, &fakeWire{}, &fakePromptStore{}, sink)

	first := s.enqueue(event{kind: eventAdapter, ctx: context.Background(), adapter: sink.adapterEvent("first")})
	<-sink.entered
	second := s.enqueue(event{kind: eventSubmit, ctx: context.Background(), submit: submitRequest("second")})
	third := s.enqueue(event{kind: eventAdapter, ctx: context.Background(), adapter: sink.adapterEvent("third")})
	close(sink.release)

	for _, future := range []<-chan result{first, second, third} {
		if got := <-future; got.err != nil {
			t.Fatal(got.err)
		}
	}
	if got, want := sink.log(), []string{"first:start", "first:end", "second", "third:start", "third:end"}; !equalStrings(got, want) {
		t.Fatalf("apply order = %v, want %v", got, want)
	}
}

// The operator's stop button is needed exactly when a session is backlogged, so
// an interrupt must overtake the queue instead of waiting out every event in it.
func TestInterruptOvertakesQueuedEvents(t *testing.T) {
	t.Parallel()
	const backlog = 8
	order := make(chan string, backlog+2)
	// The marker is recorded on the loop itself, so the assertion is about the
	// loop's own order rather than about when a waiting caller wakes up.
	wire := &recordingWire{order: order}
	s := newTestSession(t, wire, &fakePromptStore{}, newBlockingTranscriptOpen())

	entered := make(chan struct{})
	release := make(chan struct{})
	s.ObserveAsync(AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		order <- "in-flight"
		return nil
	}})
	<-entered
	for i := 0; i < backlog; i++ {
		s.ObserveAsync(AdapterEvent{LegacyApply: func() error { order <- "backlog"; return nil }})
	}

	interrupted := make(chan error, 1)
	go func() {
		result, err := s.Interrupt(context.Background(), providers.InterruptRequest{Scope: model.Scope{Provider: model.ProviderCodex}})
		if err == nil && !result.Interrupted {
			err = errors.New("interrupt reported no interruption")
		}
		interrupted <- err
	}()
	// The interrupt must be waiting before the in-flight event is released,
	// otherwise the backlog can drain first for uninteresting reasons.
	waitForMailboxDepth(t, s, backlog+1)
	close(release)

	select {
	case err := <-interrupted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt never completed")
	}
	if err := s.Observe(context.Background(), AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
	got := []string{<-order, <-order}
	if !equalStrings(got, []string{"in-flight", "interrupt"}) {
		t.Fatalf("event order = %v, want the interrupt right after the in-flight event", got)
	}
}

// waitForMailboxDepth counts both lanes, so the test synchronizes on the
// interrupt being enqueued without assuming which lane holds it.
func waitForMailboxDepth(t *testing.T, s *Session, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mailbox.mu.Lock()
		depth := len(s.mailbox.queue) + len(s.mailbox.priority)
		s.mailbox.mu.Unlock()
		if depth >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mailbox depth = %d, want %d", depth, want)
		}
		time.Sleep(time.Millisecond)
	}
}

type recordingWire struct {
	fakeWire
	order chan string
}

func (w *recordingWire) Interrupt(context.Context, providers.InterruptRequest) (providers.InterruptionResult, error) {
	w.order <- "interrupt"
	return providers.InterruptionResult{Interrupted: true}, nil
}

// A loop wedged on one event must not freeze the stop path forever.
func TestInterruptReportsDeadlineWhenLoopIsWedged(t *testing.T) {
	t.Parallel()
	s := newTestSessionWithHandoff(t, 20*time.Millisecond)

	entered := make(chan struct{})
	wedged := make(chan struct{})
	s.ObserveAsync(AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-wedged
		return nil
	}})
	<-entered

	done := make(chan error, 1)
	go func() {
		_, err := s.Interrupt(context.Background(), providers.InterruptRequest{Scope: model.Scope{Provider: model.ProviderCodex}})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInterruptNotAccepted) {
			t.Fatalf("interrupt error = %v, want ErrInterruptNotAccepted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt hung on a wedged session loop")
	}
	close(wedged)
	s.Close()
}

func newTestSessionWithHandoff(t *testing.T, handoff time.Duration) *Session {
	t.Helper()
	initial, err := lifecycle.New(1)
	if err != nil {
		t.Fatal(err)
	}
	initial, err = transition(initial, lifecycle.EventReadyIdle, 1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		HostedThreadID: "host-1", Wire: &fakeWire{}, Prompts: &fakePromptStore{}, Turns: &fakeTurnStore{},
		Outbox: newBlockingTranscriptOpen(), Lifecycle: initial, InterruptHandoff: handoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCodexNotificationsDuringSubmitDoNotDelayTurnStartReply(t *testing.T) {
	t.Parallel()
	store := &fakePromptStore{records: []turns.Record{claimedRecord("first")}}
	wire := &notifyingWire{}
	s := newUnmanagedTestSession(t, wire, store, newBlockingTranscriptOpen())
	wire.session = s

	done := make(chan error, 1)
	go func() {
		_, err := s.Submit(context.Background(), submitRequest("first"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("40 notifications delayed the turn/start reply")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not drain the queued notifications")
	}
}

func TestSessionFieldsHaveOneWriter(t *testing.T) {
	t.Parallel()
	s := newTestSession(t, &fakeWire{}, &fakePromptStore{}, newBlockingTranscriptOpen())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = s.Observe(context.Background(), AdapterEvent{Lifecycle: observation(uint64(i + 1))})
		}(i)
		go func() { defer wg.Done(); _ = s.Snapshot() }()
	}
	wg.Wait()
	if got := s.Snapshot().Lifecycle.Epoch; got != 1 {
		t.Fatalf("lifecycle epoch = %d, want 1", got)
	}
}

func TestAttachmentEpochViewAdvancesWithLifecycle(t *testing.T) {
	t.Parallel()
	s := newTestSession(t, &fakeWire{}, &fakePromptStore{}, newBlockingTranscriptOpen())
	if got := s.AttachmentEpoch(); got != 1 {
		t.Fatalf("initial attachment epoch = %d, want 1", got)
	}
	if err := s.Observe(context.Background(), AdapterEvent{Lifecycle: &lifecycle.Observation{
		Event: lifecycle.EventBoot, Epoch: 2,
	}}); err != nil {
		t.Fatal(err)
	}
	if got := s.AttachmentEpoch(); got != 2 {
		t.Fatalf("advanced attachment epoch = %d, want 2", got)
	}
}

func TestCompletionQueuedBeforeAcceptanceStillDrainsSecondPrompt(t *testing.T) {
	t.Parallel()
	wire := &fakeWire{holdFirst: make(chan struct{}), firstEntered: make(chan struct{})}
	store := &fakePromptStore{records: []turns.Record{claimedRecord("first"), claimedRecord("second")}}
	s := newTestSession(t, wire, store, newBlockingTranscriptOpen())

	firstDone := make(chan error, 1)
	go func() { _, err := s.Submit(context.Background(), submitRequest("first")); firstDone <- err }()
	<-wire.firstEntered
	secondDone := make(chan error, 1)
	go func() { _, err := s.Submit(context.Background(), submitRequest("second")); secondDone <- err }()
	completion := s.enqueue(event{kind: eventAdapter, ctx: context.Background(), adapter: AdapterEvent{Lifecycle: observationWith(lifecycle.EventFinished, 3)}})
	close(wire.holdFirst)

	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := <-completion; got.err != nil {
		t.Fatal(got.err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second prompt did not send after completion")
	}
	if got := wire.submissions(); !equalStrings(got, []string{"first", "second"}) {
		t.Fatalf("submissions = %v", got)
	}
	if got := wire.submissionScopes(); len(got) != 2 || got[1].Provider != model.ProviderCodex {
		t.Fatalf("drained submission scopes = %#v", got)
	}
}

func TestReattachRestoresAcceptedPromptWithoutResend(t *testing.T) {
	t.Parallel()
	wire := &fakeWire{}
	store := &fakePromptStore{restored: turns.Restored{Attempts: []turns.Attempt{{ActionID: "accepted", State: turns.JournalAccepted, ProviderTurnID: "turn-1"}}}}
	s := newTestSession(t, wire, store, newBlockingTranscriptOpen())
	native := "native-1"
	if _, err := s.Resume(context.Background(), providers.ResumeRequest{StartRequest: providers.StartRequest{HostedThreadID: "host-1"}, NativeThreadID: native}); err != nil {
		t.Fatal(err)
	}
	if wire.resumeCount != 1 || store.restoreCount != 1 {
		t.Fatalf("resume=%d restore=%d, want 1/1", wire.resumeCount, store.restoreCount)
	}
	if got := wire.submissions(); len(got) != 0 {
		t.Fatalf("accepted prompt resent: %v", got)
	}
}

// A provider that answers after the delivery deadline must not strand the
// prompt's claim, which would block every later prompt on the thread and make
// Discard refuse it. The deadline bounds the provider call only; the outcome
// is still recorded, and the caller sees it. Boundary: Deliver through the
// session loop to a prompt store that, like database/sql, refuses a done
// context.
func TestDeliveryPastItsDeadlineStillRecordsItsOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &deadlineStore{fakePromptStore: &fakePromptStore{records: []turns.Record{claimedRecord("slow"), claimedRecord("next")}}}
	s := newTestSession(t, &slowWire{}, store, newBlockingTranscriptOpen())
	record, claimed, err := s.Claim(ctx, 1)
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	delivery, err := s.Deliver(deadline, record, model.Scope{Provider: model.ProviderCodex}, providers.Settings{}, false)
	if err != nil || delivery.Record.Outbox.State != turns.Uncertain {
		t.Fatalf("delivery=%+v err=%v; want the recorded uncertain outcome", delivery.Record.Outbox, err)
	}
	if got := store.recorded(); !equalStrings(got, []string{"slow"}) {
		t.Fatalf("uncertain records=%v; want [slow]", got)
	}
	if next, claimed, err := s.Claim(ctx, 1); err != nil || !claimed || next.Outbox.ActionID != "next" {
		t.Fatalf("next claim=%v %s err=%v; want the following prompt claimed", claimed, next.Outbox.ActionID, err)
	}
}

// slowWire answers only once the delivery deadline has passed, as a provider
// adapter reports a call it stopped waiting for.
type slowWire struct{ fakeWire }

func (*slowWire) Submit(ctx context.Context, _ providers.SubmissionRequest) (providers.SubmissionResult, error) {
	<-ctx.Done()
	return providers.SubmissionResult{State: turns.Uncertain, Reason: ctx.Err().Error()}, nil
}

type deadlineStore struct {
	*fakePromptStore
	uncertain []string
}

func (s *deadlineStore) Uncertain(ctx context.Context, u turns.Uncertainty) (turns.Record, error) {
	if err := ctx.Err(); err != nil {
		return turns.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uncertain = append(s.uncertain, u.ActionID)
	return turns.Record{Outbox: turns.OutboxEntry{HostedThreadID: "host-1", ActionID: u.ActionID, State: turns.Uncertain}}, nil
}

func (s *deadlineStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.uncertain...)
}

func newTestSession(t *testing.T, wire Wire, store PromptStore, outbox OutboxPublisher) *Session {
	return newTestSessionWithTurns(t, wire, store, &fakeTurnStore{}, outbox)
}

func newTestSessionWithTurns(t *testing.T, wire Wire, store PromptStore, turnStore TurnStore, outbox OutboxPublisher) *Session {
	t.Helper()
	s := newUnmanagedTestSessionWithTurns(t, wire, store, turnStore, outbox)
	t.Cleanup(s.Close)
	return s
}

func newUnmanagedTestSession(t *testing.T, wire Wire, store PromptStore, outbox OutboxPublisher) *Session {
	return newUnmanagedTestSessionWithTurns(t, wire, store, &fakeTurnStore{}, outbox)
}

func newUnmanagedTestSessionWithTurns(t *testing.T, wire Wire, store PromptStore, turnStore TurnStore, outbox OutboxPublisher) *Session {
	return newUnmanagedTestSessionWithConfig(t, wire, store, turnStore, outbox, nil)
}

func newUnmanagedTestSessionWithConfig(t *testing.T, wire Wire, store PromptStore, turnStore TurnStore, outbox OutboxPublisher, asyncError func(error)) *Session {
	t.Helper()
	initial, err := lifecycle.New(1)
	if err != nil {
		t.Fatal(err)
	}
	initial, err = transition(initial, lifecycle.EventReadyIdle, 1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{HostedThreadID: "host-1", Wire: wire, Prompts: store, Turns: turnStore, Outbox: outbox, Lifecycle: initial, AsyncError: asyncError})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func transition(current lifecycle.Snapshot, event lifecycle.Event, sequence uint64) (lifecycle.Snapshot, error) {
	out, err := lifecycle.Transition(current, lifecycle.Observation{Event: event, Epoch: current.Epoch, Sequence: sequence})
	return out.Snapshot, err
}

func observation(sequence uint64) *lifecycle.Observation {
	return observationWith(lifecycle.EventStarted, sequence)
}
func observationWith(kind lifecycle.Event, sequence uint64) *lifecycle.Observation {
	return &lifecycle.Observation{Event: kind, Epoch: 1, Sequence: sequence}
}

func submitRequest(id string) SubmitRequest {
	return SubmitRequest{Admission: turns.Submission{HostedThreadID: "host-1", Provider: model.ProviderCodex, ActionID: id, ProviderText: id, PresentationText: id}, Scope: model.Scope{Provider: model.ProviderCodex}}
}

func claimedRecord(id string) turns.Record {
	return turns.Record{Outbox: turns.OutboxEntry{HostedThreadID: "host-1", ActionID: id, PresentationText: id, State: turns.Delivering}, Provider: model.ProviderCodex, ProviderText: id, ClaimToken: "claim-" + id, ClaimEpoch: 1}
}

type fakeWire struct {
	mu           sync.Mutex
	submitted    []string
	scopes       []model.Scope
	firstEntered chan struct{}
	holdFirst    chan struct{}
	resumeCount  int
}

type notifyingWire struct {
	fakeWire
	session *Session
}

func (w *notifyingWire) Submit(ctx context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
	for i := 0; i < 40; i++ {
		w.session.ObserveAsync(AdapterEvent{LegacyApply: func() error { return nil }})
	}
	return w.fakeWire.Submit(ctx, request)
}

func (*fakeWire) Capabilities() lifecycle.Capabilities     { return lifecycle.Capabilities{} }
func (*fakeWire) Current() (lifecycle.Observation, bool)   { return lifecycle.Observation{}, false }
func (*fakeWire) TurnCapabilities() providers.Capabilities { return providers.Capabilities{} }
func (*fakeWire) Start(context.Context, providers.StartRequest) (providers.Attachment, error) {
	return providers.Attachment{}, nil
}
func (w *fakeWire) Resume(context.Context, providers.ResumeRequest) (providers.Attachment, error) {
	w.resumeCount++
	return providers.Attachment{}, nil
}
func (w *fakeWire) Submit(_ context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
	w.mu.Lock()
	w.submitted = append(w.submitted, request.Outbox.ActionID)
	w.scopes = append(w.scopes, request.Scope)
	first := len(w.submitted) == 1
	w.mu.Unlock()
	if first && w.firstEntered != nil {
		close(w.firstEntered)
		<-w.holdFirst
	}
	turnID, native := "turn-"+request.Outbox.ActionID, "native-1"
	return providers.SubmissionResult{State: turns.Accepted, Binding: &turns.Binding{Scope: model.Scope{Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turnID}}}, nil
}
func (*fakeWire) Interrupt(context.Context, providers.InterruptRequest) (providers.InterruptionResult, error) {
	return providers.InterruptionResult{Interrupted: true}, nil
}
func (*fakeWire) RespondInteraction(context.Context, providers.InteractionResponse) (model.Batch, error) {
	return model.Batch{}, nil
}
func (w *fakeWire) submissions() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.submitted...)
}
func (w *fakeWire) submissionScopes() []model.Scope {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]model.Scope(nil), w.scopes...)
}

type fakePromptStore struct {
	mu           sync.Mutex
	records      []turns.Record
	restored     turns.Restored
	restoreCount int
	writes       int
	requeued     []string
}

func (s *fakePromptStore) Submit(_ context.Context, a turns.Submission) (turns.Record, bool, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return turns.Record{Outbox: turns.OutboxEntry{HostedThreadID: a.HostedThreadID, ActionID: a.ActionID, PresentationText: a.PresentationText, State: turns.Queued}, Provider: a.Provider, ProviderText: a.ProviderText}, true, nil
}
func (s *fakePromptStore) Claim(context.Context, turns.ClaimRequest) (turns.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if len(s.records) == 0 {
		return turns.Record{}, false, nil
	}
	r := s.records[0]
	s.records = s.records[1:]
	return r, true, nil
}
func (s *fakePromptStore) Accepted(_ context.Context, a turns.Acceptance) (turns.Record, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return turns.Record{Outbox: turns.OutboxEntry{HostedThreadID: "host-1", ActionID: a.ActionID, State: turns.Accepted, Binding: &a.Binding}}, nil
}
func (s *fakePromptStore) Uncertain(context.Context, turns.Uncertainty) (turns.Record, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return turns.Record{}, nil
}
func (s *fakePromptStore) Refused(context.Context, turns.Refusal) (turns.Record, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return turns.Record{}, nil
}
func (s *fakePromptStore) Requeue(_ context.Context, r turns.Requeue) (turns.Record, error) {
	s.mu.Lock()
	s.writes++
	s.requeued = append(s.requeued, r.ActionID)
	s.mu.Unlock()
	return turns.Record{Outbox: turns.OutboxEntry{HostedThreadID: "host-1", ActionID: r.ActionID, State: turns.Queued}}, nil
}
func (s *fakePromptStore) Cancel(context.Context, string) (turns.Record, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return turns.Record{}, nil
}
func (s *fakePromptStore) Restore(context.Context, string, uint64) (turns.Restored, error) {
	s.restoreCount++
	return s.restored, nil
}

func (s *fakePromptStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

type fakeTurnStore struct {
	mu     sync.Mutex
	writes int
	err    error
}

func (s *fakeTurnStore) Started(context.Context, coreturns.Start) (coreturns.Record, bool, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return coreturns.Record{}, true, s.err
}

func (s *fakeTurnStore) Finished(context.Context, coreturns.Finish) (coreturns.Record, bool, error) {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return coreturns.Record{}, true, s.err
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type blockingTranscript struct {
	mu      sync.Mutex
	entries []string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingTranscript() *blockingTranscript {
	return &blockingTranscript{entered: make(chan struct{}), release: make(chan struct{})}
}
func newBlockingTranscriptOpen() *blockingTranscript {
	s := newBlockingTranscript()
	close(s.release)
	return s
}

// adapterEvent is an adapter event whose apply logs its start and end and
// waits for release, so a test can hold the session loop inside it.
func (s *blockingTranscript) adapterEvent(id string) AdapterEvent {
	return AdapterEvent{LegacyApply: func() error {
		s.mu.Lock()
		s.entries = append(s.entries, id+":start")
		s.mu.Unlock()
		s.once.Do(func() { close(s.entered) })
		<-s.release
		s.mu.Lock()
		s.entries = append(s.entries, id+":end")
		s.mu.Unlock()
		return nil
	}}
}
func (s *blockingTranscript) PublishOutbox(entry turns.OutboxEntry) {
	s.mu.Lock()
	s.entries = append(s.entries, entry.ActionID)
	s.mu.Unlock()
}
func (s *blockingTranscript) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.entries...)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// steeringWire accepts running-turn input and answers each submission with
// the scripted result, recording the turn each one targeted.
type steeringWire struct {
	fakeWire
	result  providers.SubmissionResult
	err     error
	targets []string
}

func (*steeringWire) TurnCapabilities() providers.Capabilities {
	return providers.Capabilities{RunningTurnInput: true}
}
func (w *steeringWire) Submit(_ context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.submitted = append(w.submitted, request.Outbox.ActionID)
	w.targets = append(w.targets, request.RunningTurnID)
	return w.result, w.err
}

func workingSession(t *testing.T, wire Wire, store PromptStore) *Session {
	t.Helper()
	s := newTestSession(t, wire, store, newBlockingTranscriptOpen())
	if err := s.Observe(context.Background(), AdapterEvent{Lifecycle: observationWith(lifecycle.EventStarted, 2)}); err != nil {
		t.Fatal(err)
	}
	if got := s.Snapshot().Lifecycle.State.Work; got != lifecycle.Working {
		t.Fatalf("work=%s; want working", got)
	}
	return s
}

func runningScope(turn string) model.Scope {
	native := "native-1"
	return model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}
}

// A running-turn claim exists only while a turn works on a provider that
// accepts running-turn input. Its delivery names that exact turn and leaves
// the turn working: it is not a new turn being accepted.
func TestRunningTurnDeliveryTargetsTheWorkingTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	idle := newTestSession(t, &steeringWire{}, &fakePromptStore{records: []turns.Record{claimedRecord("early")}}, newBlockingTranscriptOpen())
	if _, claimed, err := idle.ClaimRunningTurn(ctx, 1); err != nil || claimed {
		t.Fatalf("idle thread: claimed=%v err=%v; want no running-turn claim", claimed, err)
	}
	unsupported := workingSession(t, &fakeWire{}, &fakePromptStore{records: []turns.Record{claimedRecord("plain")}})
	if _, claimed, err := unsupported.ClaimRunningTurn(ctx, 1); err != nil || claimed {
		t.Fatalf("provider without running-turn input: claimed=%v err=%v", claimed, err)
	}
	waiting := workingSession(t, &steeringWire{}, &fakePromptStore{records: []turns.Record{claimedRecord("waiting")}})
	if err := waiting.Observe(ctx, AdapterEvent{Lifecycle: observationWith(lifecycle.EventWaiting, 3)}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := waiting.ClaimRunningTurn(ctx, 1); err != nil || claimed {
		t.Fatalf("turn waiting on an approval: claimed=%v err=%v; want queued", claimed, err)
	}

	native, turn := "native-1", "turn-running"
	wire := &steeringWire{result: providers.SubmissionResult{State: turns.Accepted, Binding: &turns.Binding{Scope: model.Scope{Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: "event-steer"}}}
	s := workingSession(t, wire, &fakePromptStore{records: []turns.Record{claimedRecord("steer")}})
	record, claimed, err := s.ClaimRunningTurn(ctx, 1)
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	delivery, err := s.Deliver(ctx, record, runningScope(turn), providers.Settings{}, false)
	if err != nil || delivery.Record.Outbox.State != turns.Accepted {
		t.Fatalf("delivery=%+v err=%v", delivery.Record.Outbox, err)
	}
	if !equalStrings(wire.targets, []string{turn}) {
		t.Fatalf("targets=%v; want [%s]", wire.targets, turn)
	}
	if got := s.Snapshot().Lifecycle.State.Work; got != lifecycle.Working {
		t.Fatalf("work after running-turn delivery=%s; want working", got)
	}
}

// Only a definite "turn already ended" refusal returns the prompt to the
// queue. Any other refusal is explicit, and a transport failure is uncertain;
// none of them is sent a second time.
func TestRunningTurnRefusalsNeverResend(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		result  providers.SubmissionResult
		err     error
		requeue bool
		state   turns.DeliveryState
	}{
		{name: "turn ended", result: providers.SubmissionResult{State: turns.Refused, FailureCode: providers.FailureTurnEnded, Reason: "no active turn to steer"}, requeue: true, state: turns.Queued},
		{name: "turn mismatch", result: providers.SubmissionResult{State: turns.Refused, Reason: "expected active turn id"}},
		{name: "transport lost", err: errors.New("connection reset")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			wire := &steeringWire{result: test.result, err: test.err}
			store := &fakePromptStore{records: []turns.Record{claimedRecord("steer")}}
			s := workingSession(t, wire, store)
			record, claimed, err := s.ClaimRunningTurn(ctx, 1)
			if err != nil || !claimed {
				t.Fatalf("claimed=%v err=%v", claimed, err)
			}
			delivery, _ := s.Deliver(ctx, record, runningScope("turn-running"), providers.Settings{}, false)
			if requeued := len(store.requeued) == 1; requeued != test.requeue {
				t.Fatalf("requeued=%v; want %v", store.requeued, test.requeue)
			}
			if test.requeue && delivery.Record.Outbox.State != test.state {
				t.Fatalf("state=%s; want %s", delivery.Record.Outbox.State, test.state)
			}
			if got := wire.submissions(); len(got) != 1 {
				t.Fatalf("submissions=%v; want exactly one", got)
			}
			if s.Snapshot().InFlightAction != "" {
				t.Fatal("claim still held after the provider answered")
			}
		})
	}
}
