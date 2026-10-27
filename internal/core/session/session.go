// Package session owns the mutable runtime of one hosted thread.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Wire is the provider edge. Existing checked provider adapters implement it.
type Wire interface{ providers.Adapter }

// OutboxPublisher shows each committed prompt record change. Saved rows come
// from the provider's file, so a Session publishes nothing else.
type OutboxPublisher interface {
	PublishOutbox(prompts.OutboxEntry)
}

// PromptStore is the durable prompt transaction edge. C1 renames and narrows
// the concrete store; keeping the interface here avoids teaching Session SQL.
type PromptStore interface {
	Submit(context.Context, prompts.Submission) (prompts.Record, bool, error)
	Claim(context.Context, prompts.ClaimRequest) (prompts.Record, bool, error)
	Accepted(context.Context, prompts.Acceptance) (prompts.Record, error)
	Uncertain(context.Context, prompts.Uncertainty) (prompts.Record, error)
	Refused(context.Context, prompts.Refusal) (prompts.Record, error)
	Requeue(context.Context, prompts.Requeue) (prompts.Record, error)
	Cancel(context.Context, string) (prompts.Record, error)
	Restore(context.Context, string, uint64) (prompts.Restored, error)
}

type TurnStore interface {
	Started(context.Context, coreturns.Start) (coreturns.Record, bool, error)
	Finished(context.Context, coreturns.Finish) (coreturns.Record, bool, error)
}

type Config struct {
	HostedThreadID string
	Wire           Wire
	Prompts        PromptStore
	Turns          TurnStore
	Outbox         OutboxPublisher
	Lifecycle      lifecycle.Snapshot
	// AsyncError receives failures from ObserveAsync after the caller has
	// returned. It must be nonblocking and must not retry provider actions.
	AsyncError func(error)
	// ExternalDrain is the C1 compatibility seam for root operation wrappers.
	// Core still owns delivery; root creates its public operation before calling it.
	ExternalDrain bool
	// InterruptHandoff overrides InterruptHandoffTimeout. Tests use it to wedge
	// the loop without waiting out the production deadline.
	InterruptHandoff time.Duration
}

type Claim struct {
	ActionID string
	Token    string
	Epoch    uint64
	// RunningTurn delivers into the turn already running, not a new one.
	RunningTurn bool
}

type Snapshot struct {
	Lifecycle       lifecycle.Snapshot
	AttachmentEpoch uint64
	InFlightAction  string
}

type AdapterEvent struct {
	Lifecycle      *lifecycle.Observation
	ProviderTurnID string
	TurnEvents     []TurnEvent
	// c2: root still translates raw wire frames. Run that translation on the
	// session writer.
	LegacyApply func() error
	// AsyncBatchKey, AsyncBatchEpoch and AsyncBatchBytes let the mailbox combine
	// only adjacent, compatible ObserveAsync events. AsyncBatchMerge updates the
	// first event's captured apply state with the next event's opaque payload.
	AsyncBatchKey     string
	AsyncBatchEpoch   uint64
	AsyncBatchBytes   int
	AsyncBatchPayload any
	AsyncBatchMerge   func(any)
}

type TurnEvent struct {
	Started  *coreturns.Start
	Finished *coreturns.Finish
}

type SubmitRequest struct {
	Admission        prompts.Submission
	Permission       lifecycle.Submission
	Scope            model.Scope
	Settings         providers.Settings
	PreviouslyQueued bool
}

type SubmitResult struct {
	Record   prompts.Record
	Provider providers.SubmissionResult
	Created  bool
	Refused  bool
	Claimed  bool
}

type eventKind uint8

const (
	eventAdapter eventKind = iota
	eventStart
	eventResume
	eventSubmit
	eventInterrupt
	eventRespond
	// c1: remove the four split-delivery events when root callers use Submit.
	eventAdmit
	eventClaim
	eventDeliver
	eventRestore
)

type event struct {
	kind             eventKind
	ctx              context.Context
	adapter          AdapterEvent
	start            providers.StartRequest
	resume           providers.ResumeRequest
	submit           SubmitRequest
	interrupt        providers.InterruptRequest
	respond          providers.InteractionResponse
	admission        prompts.Submission
	permission       lifecycle.Submission
	epoch            uint64
	runningTurn      bool
	claimed          prompts.Record
	scope            model.Scope
	settings         providers.Settings
	previouslyQueued bool
	// c2: root attachment handshakes are asynchronous and do not yet return a
	// checked provider Attachment. The event loop still serializes their start.
	legacyAttachment func(context.Context) error
	reply            chan result
	async            bool
	// dispatched, when set, is closed by loop the moment it takes this event
	// off the mailbox. It separates "still queued" from "running", which is
	// what lets Interrupt bound only the queue wait.
	dispatched chan struct{}
}

type result struct {
	value any
	err   error
}

// mailbox is an unbounded FIFO with a coalesced wake-up. Producers never wait
// for the session loop: provider callbacks can enqueue while that same loop is
// inside a legacy transport call.
//
// priority is a second FIFO that loop drains first. Only eventInterrupt uses
// it: stopping a turn is the operator's emergency brake, and it is needed
// exactly when a session is backlogged with slow wire events, so it must not
// queue behind them. Everything else keeps strict arrival order.
type mailbox struct {
	mu       sync.Mutex
	queue    []event
	priority []event
	wake     chan struct{}
	closed   bool
}

// Session has one writer: loop. The mutex protects coherent read snapshots;
// every owned-field write happens while loop holds it.
type Session struct {
	mu              sync.RWMutex
	wire            Wire
	lifecycle       lifecycle.Snapshot
	attachmentEpoch uint64
	// attachmentEpochView mirrors attachmentEpoch for callback ingress. A
	// provider callback must never wait for the writer lock held across a
	// provider request that may itself be waiting for that callback to return.
	attachmentEpochView atomic.Uint64
	inFlight            *Claim
	outbox              OutboxPublisher
	prompts             PromptStore
	turns               TurnStore
	hostedThreadID      string
	scope               model.Scope
	mailbox             *mailbox
	done                chan struct{}
	externalDrain       bool
	asyncError          func(error)
	interruptHandoff    time.Duration
}

const (
	maxAsyncAdapterBatchCount = 64
	maxAsyncAdapterBatchBytes = 64 << 10
)

func New(config Config) (*Session, error) {
	if config.HostedThreadID == "" || config.Wire == nil || config.Prompts == nil || config.Turns == nil || config.Outbox == nil {
		return nil, errors.New("core/session: hosted thread, wire, prompts, and outbox are required")
	}
	if config.Lifecycle.Epoch == 0 {
		return nil, errors.New("core/session: lifecycle attachment epoch is required")
	}
	s := &Session{
		wire: config.Wire, lifecycle: config.Lifecycle, attachmentEpoch: config.Lifecycle.Epoch,
		outbox: config.Outbox, prompts: config.Prompts, turns: config.Turns, hostedThreadID: config.HostedThreadID,
		mailbox: &mailbox{wake: make(chan struct{}, 1)}, done: make(chan struct{}), externalDrain: config.ExternalDrain,
		asyncError: config.AsyncError, interruptHandoff: config.InterruptHandoff,
	}
	if s.interruptHandoff <= 0 {
		s.interruptHandoff = InterruptHandoffTimeout
	}
	s.attachmentEpochView.Store(config.Lifecycle.Epoch)
	go s.loop()
	return s, nil
}

func (s *Session) Close() {
	s.mailbox.mu.Lock()
	if !s.mailbox.closed {
		s.mailbox.closed = true
		select {
		case s.mailbox.wake <- struct{}{}:
		default:
		}
	}
	s.mailbox.mu.Unlock()
	<-s.done
}

func (s *Session) loop() {
	defer close(s.done)
	for {
		next, ok := s.mailbox.next()
		if !ok {
			return
		}
		if next.dispatched != nil {
			close(next.dispatched)
		}
		// LegacyApply bridges raw root wire semantics until c2 moves that
		// translation into provider adapters. The loop still orders the call,
		// but it must not hold the core snapshot mutex across root callbacks:
		// root may hold one of its compatibility locks while reading Snapshot.
		if next.kind == eventAdapter && next.adapter.LegacyApply != nil {
			apply := next.adapter.LegacyApply
			next.adapter.LegacyApply = nil
			if err := apply(); err != nil {
				s.complete(next, result{err: err})
				continue
			}
		}
		s.mu.Lock()
		out := s.apply(next)
		s.mu.Unlock()
		s.complete(next, out)
	}
}

func (s *Session) complete(next event, out result) {
	if next.async {
		if out.err != nil && s.asyncError != nil {
			s.asyncError(out.err)
		}
		return
	}
	next.reply <- out
	close(next.reply)
}

func (s *Session) enqueue(next event) <-chan result {
	next.reply = make(chan result, 1)
	s.mailbox.mu.Lock()
	if s.mailbox.closed {
		next.reply <- result{err: errors.New("core/session: session is closed")}
		close(next.reply)
		s.mailbox.mu.Unlock()
		return next.reply
	}
	if next.kind == eventInterrupt {
		s.mailbox.priority = append(s.mailbox.priority, next)
	} else {
		s.mailbox.queue = append(s.mailbox.queue, next)
	}
	select {
	case s.mailbox.wake <- struct{}{}:
	default:
	}
	s.mailbox.mu.Unlock()
	return next.reply
}

func (m *mailbox) next() (event, bool) {
	for {
		m.mu.Lock()
		if len(m.priority) > 0 {
			next := m.priority[0]
			m.priority[0] = event{}
			m.priority = m.priority[1:]
			if len(m.priority) == 0 {
				m.priority = nil
			}
			m.mu.Unlock()
			return next, true
		}
		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue[0] = event{}
			m.queue = m.queue[1:]
			if len(m.queue) == 0 {
				m.queue = nil
			}
			if next.async && next.kind == eventAdapter && next.adapter.AsyncBatchKey != "" && next.adapter.AsyncBatchMerge != nil {
				count := 1
				bytes := next.adapter.AsyncBatchBytes
				for len(m.queue) > 0 && count < maxAsyncAdapterBatchCount {
					candidate := m.queue[0]
					if !candidate.async || candidate.kind != eventAdapter ||
						candidate.adapter.AsyncBatchKey != next.adapter.AsyncBatchKey ||
						candidate.adapter.AsyncBatchEpoch != next.adapter.AsyncBatchEpoch ||
						bytes+candidate.adapter.AsyncBatchBytes > maxAsyncAdapterBatchBytes {
						break
					}
					next.adapter.AsyncBatchMerge(candidate.adapter.AsyncBatchPayload)
					bytes += candidate.adapter.AsyncBatchBytes
					count++
					m.queue[0] = event{}
					m.queue = m.queue[1:]
				}
				if len(m.queue) == 0 {
					m.queue = nil
				}
			}
			m.mu.Unlock()
			return next, true
		}
		if m.closed {
			m.mu.Unlock()
			return event{}, false
		}
		m.mu.Unlock()
		<-m.wake
	}
}

func (s *Session) request(ctx context.Context, next event) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	next.ctx = ctx
	future := s.enqueue(next)
	select {
	case out := <-future:
		return out.value, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) Start(ctx context.Context, request providers.StartRequest) (providers.Attachment, error) {
	value, err := s.request(ctx, event{kind: eventStart, start: request})
	if err != nil {
		return providers.Attachment{}, err
	}
	return value.(providers.Attachment), nil
}

func (s *Session) Resume(ctx context.Context, request providers.ResumeRequest) (providers.Attachment, error) {
	value, err := s.request(ctx, event{kind: eventResume, resume: request})
	if err != nil {
		return providers.Attachment{}, err
	}
	return value.(providers.Attachment), nil
}

// StartLegacy and ResumeLegacy are the c2: root attachment compatibility
// seam. C2 replaces them with Start and Resume once adapters return evidence.
func (s *Session) StartLegacy(ctx context.Context, operation func(context.Context) error) error {
	_, err := s.request(ctx, event{kind: eventStart, legacyAttachment: operation})
	return err
}

func (s *Session) ResumeLegacy(ctx context.Context, operation func(context.Context) error) error {
	_, err := s.request(ctx, event{kind: eventResume, legacyAttachment: operation})
	return err
}

func (s *Session) Submit(ctx context.Context, request SubmitRequest) (SubmitResult, error) {
	value, err := s.request(ctx, event{kind: eventSubmit, submit: request})
	if err != nil {
		return SubmitResult{}, err
	}
	return value.(SubmitResult), nil
}

// InterruptHandoffTimeout bounds only the wait for loop to pick the interrupt
// up, not the provider call that follows — every adapter bounds its own
// interrupt (codex 5s, claude 10s, grok is a fire-and-forget notify). An
// interrupt jumps the mailbox queue, so the sole remaining wait is the one
// event already running, and a wait this long means that event is wedged.
const InterruptHandoffTimeout = 10 * time.Second

// ErrInterruptNotAccepted reports that the session loop was still busy with an
// earlier event when the handoff deadline passed. The interrupt stays queued at
// the front and is still delivered if the loop recovers; the caller must not
// report the turn as interrupted.
var ErrInterruptNotAccepted = errors.New("core/session: the session loop did not accept the interrupt before the handoff deadline")

func (s *Session) Interrupt(ctx context.Context, request providers.InterruptRequest) (providers.InterruptionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dispatched := make(chan struct{})
	future := s.enqueue(event{kind: eventInterrupt, ctx: ctx, interrupt: request, dispatched: dispatched})
	handoff := time.NewTimer(s.interruptHandoff)
	defer handoff.Stop()
	select {
	case <-dispatched:
	case out := <-future:
		// A closed session answers without ever reaching loop.
		return interruptionOf(out)
	case <-ctx.Done():
		return providers.InterruptionResult{}, ctx.Err()
	case <-handoff.C:
		return providers.InterruptionResult{}, ErrInterruptNotAccepted
	}
	select {
	case out := <-future:
		return interruptionOf(out)
	case <-ctx.Done():
		return providers.InterruptionResult{}, ctx.Err()
	}
}

func interruptionOf(out result) (providers.InterruptionResult, error) {
	value, ok := out.value.(providers.InterruptionResult)
	if !ok {
		return providers.InterruptionResult{}, out.err
	}
	return value, out.err
}

func (s *Session) RespondInteraction(ctx context.Context, request providers.InteractionResponse) (model.Batch, error) {
	value, err := s.request(ctx, event{kind: eventRespond, respond: request})
	if err != nil {
		return model.Batch{}, err
	}
	return value.(model.Batch), nil
}

func (s *Session) Observe(ctx context.Context, observed AdapterEvent) error {
	_, err := s.request(ctx, event{kind: eventAdapter, adapter: observed})
	return err
}

// ObserveAsync is the ordered wire-callback ingress. It must not wait for a
// provider operation already running on the loop, because that operation may
// be waiting for the callback's transport response.
func (s *Session) ObserveAsync(observed AdapterEvent) {
	s.mailbox.mu.Lock()
	if !s.mailbox.closed {
		s.mailbox.queue = append(s.mailbox.queue, event{kind: eventAdapter, ctx: context.Background(), adapter: observed, async: true})
		select {
		case s.mailbox.wake <- struct{}{}:
		default:
		}
	}
	s.mailbox.mu.Unlock()
}

type AdmissionResult struct {
	Record           prompts.Record
	Created, Refused bool
}
type DeliveryResult struct {
	Record   prompts.Record
	Provider providers.SubmissionResult
	Claimed  bool
}

// The following four methods are the C1 compatibility seam for root callers.
func (s *Session) Admit(ctx context.Context, admission prompts.Submission, permission lifecycle.Submission) (AdmissionResult, error) {
	value, err := s.request(ctx, event{kind: eventAdmit, admission: admission, permission: permission})
	if err != nil {
		return AdmissionResult{}, err
	}
	return value.(AdmissionResult), nil
}
func (s *Session) Claim(ctx context.Context, epoch uint64) (prompts.Record, bool, error) {
	return s.requestClaim(ctx, event{kind: eventClaim, epoch: epoch})
}

// ClaimRunningTurn claims the first ordinary queued prompt for delivery into
// the turn that is running now. It claims nothing unless that turn is working
// on a provider that accepts running-turn input.
func (s *Session) ClaimRunningTurn(ctx context.Context, epoch uint64) (prompts.Record, bool, error) {
	return s.requestClaim(ctx, event{kind: eventClaim, epoch: epoch, runningTurn: true})
}

func (s *Session) requestClaim(ctx context.Context, next event) (prompts.Record, bool, error) {
	value, err := s.request(ctx, next)
	if err != nil {
		return prompts.Record{}, false, err
	}
	answer := value.(claimResult)
	return answer.Record, answer.Claimed, nil
}

// Deliver waits for the delivery's recorded outcome, not for ctx: ctx bounds
// the provider call inside it, and the caller must see the claim released
// before it drains the queue or answers the person.
func (s *Session) Deliver(ctx context.Context, claimed prompts.Record, scope model.Scope, settings providers.Settings, previouslyQueued bool) (DeliveryResult, error) {
	out := <-s.enqueue(event{kind: eventDeliver, ctx: ctx, claimed: claimed, scope: scope, settings: settings, previouslyQueued: previouslyQueued})
	value, _ := out.value.(DeliveryResult)
	return value, out.err
}
func (s *Session) Restore(ctx context.Context, epoch uint64) (prompts.Restored, error) {
	value, err := s.request(ctx, event{kind: eventRestore, epoch: epoch})
	if err != nil {
		return prompts.Restored{}, err
	}
	return value.(prompts.Restored), nil
}

type claimResult struct {
	Record  prompts.Record
	Claimed bool
}

func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	view := Snapshot{Lifecycle: s.lifecycle, AttachmentEpoch: s.attachmentEpoch}
	if s.inFlight != nil {
		view.InFlightAction = s.inFlight.ActionID
	}
	return view
}

// AttachmentEpoch is the lock-free callback-ingress view of the current
// attachment. The session loop remains the sole writer and publishes each
// accepted lifecycle transition to this atomic mirror.
func (s *Session) AttachmentEpoch() uint64 { return s.attachmentEpochView.Load() }

func (s *Session) TurnCapabilities() providers.Capabilities { return s.wire.TurnCapabilities() }

func (s *Session) apply(next event) result {
	switch next.kind {
	case eventAdapter:
		return result{err: s.applyAdapter(next.ctx, next.adapter)}
	case eventStart:
		if next.legacyAttachment != nil {
			return result{value: providers.Attachment{}, err: next.legacyAttachment(next.ctx)}
		}
		attachment, err := s.wire.Start(next.ctx, next.start)
		if err == nil {
			err = s.applyAttachment(next.ctx, attachment)
		}
		return result{value: attachment, err: err}
	case eventResume:
		if next.legacyAttachment != nil {
			return result{value: providers.Attachment{}, err: next.legacyAttachment(next.ctx)}
		}
		restored, err := s.prompts.Restore(next.ctx, s.hostedThreadID, s.attachmentEpoch)
		if err != nil {
			return result{err: err}
		}
		for _, entry := range restored.Outbox {
			s.outbox.PublishOutbox(entry)
		}
		attachment, err := s.wire.Resume(next.ctx, next.resume)
		if err == nil {
			err = s.applyAttachment(next.ctx, attachment)
		}
		return result{value: attachment, err: err}
	case eventSubmit:
		value, err := s.applySubmit(next.ctx, next.submit)
		return result{value: value, err: err}
	case eventInterrupt:
		value, err := s.wire.Interrupt(next.ctx, next.interrupt)
		return result{value: value, err: err}
	case eventRespond:
		value, err := s.wire.RespondInteraction(next.ctx, next.respond)
		return result{value: value, err: err}
	case eventAdmit:
		value, err := s.admit(next.ctx, next.admission, next.permission)
		return result{value: value, err: err}
	case eventClaim:
		record, claimed, err := s.claim(next.ctx, next.epoch, next.runningTurn)
		return result{value: claimResult{Record: record, Claimed: claimed}, err: err}
	case eventDeliver:
		value, err := s.deliver(next.ctx, next.claimed, next.scope, next.settings, next.previouslyQueued)
		return result{value: value, err: err}
	case eventRestore:
		value, err := s.restore(next.ctx, next.epoch)
		return result{value: value, err: err}
	default:
		return result{err: errors.New("core/session: unknown event")}
	}
}

func (s *Session) applyAttachment(ctx context.Context, attachment providers.Attachment) error {
	if attachment.Scope.Provider != "" {
		s.scope = attachment.Scope
	}
	if attachment.Lifecycle.Epoch != 0 {
		observation := attachment.Lifecycle
		if err := s.applyLifecycle(&observation); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) applyAdapter(ctx context.Context, observed AdapterEvent) error {
	drain := false
	if observed.Lifecycle != nil {
		var err error
		drain, err = s.applyLifecycleOutcome(observed.Lifecycle)
		if err != nil {
			return err
		}
	}
	for _, turnEvent := range observed.TurnEvents {
		if turnEvent.Started != nil {
			started := *turnEvent.Started
			if started.TurnID == "" {
				started.TurnID = observed.ProviderTurnID
			}
			if _, _, err := s.turns.Started(ctx, started); err != nil {
				return err
			}
		}
		if turnEvent.Finished != nil {
			finished := *turnEvent.Finished
			if finished.TurnID == "" {
				finished.TurnID = observed.ProviderTurnID
			}
			if finished.TurnID == "" {
				continue // its adapter could name no turn, so it finishes none
			}
			if _, _, err := s.turns.Finished(ctx, finished); err != nil {
				return err
			}
		}
	}
	if drain && !s.externalDrain {
		_, err := s.drain(ctx, SubmitRequest{Scope: s.scope})
		return err
	}
	return nil
}

func (s *Session) applyLifecycle(observation *lifecycle.Observation) error {
	_, err := s.applyLifecycleOutcome(observation)
	return err
}

func (s *Session) applyLifecycleOutcome(observation *lifecycle.Observation) (bool, error) {
	if observation == nil {
		return false, nil
	}
	out, err := lifecycle.Transition(s.lifecycle, *observation)
	if err != nil {
		return false, err
	}
	s.lifecycle = out.Snapshot
	s.attachmentEpoch = out.Snapshot.Epoch
	s.attachmentEpochView.Store(out.Snapshot.Epoch)
	return out.Drain, nil
}

func (s *Session) applySubmit(ctx context.Context, request SubmitRequest) (SubmitResult, error) {
	admitted, err := s.admit(ctx, request.Admission, request.Permission)
	answer := SubmitResult{Record: admitted.Record, Created: admitted.Created, Refused: admitted.Refused}
	if err != nil {
		return answer, err
	}
	if request.Scope.Provider != "" {
		s.scope = request.Scope
	}
	if admitted.Refused || !s.readyToSend() {
		return answer, nil
	}
	delivered, err := s.drain(ctx, request)
	if delivered.Record.Outbox.ActionID == "" {
		delivered.Record = admitted.Record
	}
	delivered.Created = admitted.Created
	return delivered, err
}

func (s *Session) admit(ctx context.Context, admission prompts.Submission, permission lifecycle.Submission) (AdmissionResult, error) {
	if admission.HostedThreadID != s.hostedThreadID {
		return AdmissionResult{}, errors.New("core/session: submission owner does not match session")
	}
	record, created, err := s.prompts.Submit(ctx, admission)
	if err != nil {
		return AdmissionResult{}, err
	}
	s.outbox.PublishOutbox(record.Outbox)
	answer := AdmissionResult{Record: record, Created: created}
	if permission != lifecycle.RefuseKeepText {
		return answer, nil
	}
	record, err = s.prompts.Cancel(ctx, admission.ActionID)
	if err != nil {
		return AdmissionResult{}, err
	}
	answer.Record, answer.Refused = record, true
	s.outbox.PublishOutbox(record.Outbox)
	return answer, nil
}

func (s *Session) claim(ctx context.Context, epoch uint64, runningTurn bool) (prompts.Record, bool, error) {
	if s.inFlight != nil || runningTurn && !s.readyForRunningTurn() {
		return prompts.Record{}, false, nil
	}
	token, err := newClaimToken()
	if err != nil {
		return prompts.Record{}, false, err
	}
	record, claimed, err := s.prompts.Claim(ctx, prompts.ClaimRequest{HostedThreadID: s.hostedThreadID, Token: token, AttachmentEpoch: epoch, Ready: true, IntoRunningTurn: runningTurn})
	if errors.Is(err, prompts.ErrClaimHeld) {
		return record, false, nil
	}
	if err != nil || !claimed {
		return record, claimed, err
	}
	s.outbox.PublishOutbox(record.Outbox)
	s.inFlight = &Claim{ActionID: record.Outbox.ActionID, Token: record.ClaimToken, Epoch: record.ClaimEpoch, RunningTurn: runningTurn}
	return record, true, nil
}

func (s *Session) deliver(ctx context.Context, claimed prompts.Record, scope model.Scope, settings providers.Settings, previouslyQueued bool) (DeliveryResult, error) {
	if s.inFlight == nil || s.inFlight.ActionID != claimed.Outbox.ActionID || s.inFlight.Token != claimed.ClaimToken || s.inFlight.Epoch != claimed.ClaimEpoch {
		return DeliveryResult{}, errors.New("core/session: delivery does not match the in-flight claim")
	}
	if scope.Provider != "" {
		s.scope = scope
	}
	request := providers.SubmissionRequest{
		Scope: scope, Outbox: claimed.Outbox, Text: claimed.ProviderText,
		Settings: settings, PreviouslyQueued: previouslyQueued,
	}
	if s.inFlight.RunningTurn {
		if scope.NativeTurnID == nil || *scope.NativeTurnID == "" {
			// Nothing was sent; release the claim rather than start a turn.
			s.inFlight = nil
			record, err := s.prompts.Requeue(context.WithoutCancel(ctx), prompts.Requeue{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch})
			if err == nil {
				s.outbox.PublishOutbox(record.Outbox)
			}
			return DeliveryResult{Record: record, Claimed: true}, err
		}
		request.RunningTurnID = *scope.NativeTurnID
	}
	record, providerResult, err := s.sendClaimed(ctx, claimed, request)
	return DeliveryResult{Record: record, Provider: providerResult, Claimed: true}, err
}

func (s *Session) restore(ctx context.Context, epoch uint64) (prompts.Restored, error) {
	restored, err := s.prompts.Restore(ctx, s.hostedThreadID, epoch)
	if err != nil {
		return prompts.Restored{}, err
	}
	for _, entry := range restored.Outbox {
		s.outbox.PublishOutbox(entry)
	}
	return restored, nil
}

func (s *Session) readyToSend() bool {
	return s.inFlight == nil && s.lifecycle.State.Connection == lifecycle.Ready && s.lifecycle.State.Work == lifecycle.Idle
}

// readyForRunningTurn admits running-turn input only while the turn works.
// An accepted turn may not be steerable yet, and a turn waiting on an
// approval keeps ordinary prompts queued: a prompt never answers it.
func (s *Session) readyForRunningTurn() bool {
	return s.lifecycle.State.Connection == lifecycle.Ready && s.lifecycle.State.Work == lifecycle.Working &&
		s.wire.TurnCapabilities().RunningTurnInput
}

func (s *Session) drain(ctx context.Context, request SubmitRequest) (SubmitResult, error) {
	if !s.readyToSend() {
		return SubmitResult{}, nil
	}
	claimed, ok, err := s.claim(ctx, s.attachmentEpoch, false)
	if err != nil || !ok {
		return SubmitResult{Record: claimed}, err
	}
	record, providerResult, err := s.sendClaimed(ctx, claimed, providers.SubmissionRequest{
		Scope: request.Scope, Outbox: claimed.Outbox, Text: claimed.ProviderText,
		Settings: request.Settings, PreviouslyQueued: request.PreviouslyQueued,
	})
	return SubmitResult{Record: record, Provider: providerResult, Claimed: true}, err
}

func (s *Session) sendClaimed(ctx context.Context, claimed prompts.Record, request providers.SubmissionRequest) (prompts.Record, providers.SubmissionResult, error) {
	providerResult, providerErr := s.wire.Submit(ctx, request)
	s.inFlight = nil
	// The delivery deadline bounds only the provider call. Its outcome must
	// still be recorded once the deadline has passed: a failed write here
	// keeps the claim, which blocks the thread's queue until a restart.
	ctx = context.WithoutCancel(ctx)
	runningTurn := request.RunningTurnID != ""
	if providerErr != nil {
		return claimed, providerResult, s.makeUncertain(ctx, claimed, providerErr.Error(), providerErr)
	}
	record, err := s.persistDelivery(ctx, claimed, providerResult)
	if err != nil {
		if providerResult.State != prompts.Accepted {
			return record, providerResult, err
		}
		uncertain, uncertaintyErr := s.prompts.Uncertain(ctx, prompts.Uncertainty{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch, Reason: "accepted delivery binding could not be persisted: " + err.Error()})
		if uncertaintyErr == nil {
			s.outbox.PublishOutbox(uncertain.Outbox)
		}
		return uncertain, providerResult, errors.Join(err, uncertaintyErr)
	}
	s.outbox.PublishOutbox(record.Outbox)
	if providerResult.Lifecycle != nil {
		if err := s.applyLifecycle(providerResult.Lifecycle); err != nil {
			return record, providerResult, err
		}
	} else if record.Outbox.State == prompts.Accepted && !runningTurn {
		observation := lifecycle.Observation{Event: lifecycle.EventAccepted, Epoch: s.lifecycle.Epoch, Sequence: s.lifecycle.Sequence + 1}
		if err := s.applyLifecycle(&observation); err != nil {
			return record, providerResult, err
		}
	}
	return record, providerResult, nil
}

func (s *Session) persistDelivery(ctx context.Context, claimed prompts.Record, providerResult providers.SubmissionResult) (prompts.Record, error) {
	switch providerResult.State {
	case prompts.Accepted:
		if providerResult.Binding == nil {
			return s.prompts.Uncertain(ctx, prompts.Uncertainty{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch, Reason: "accepted_without_binding"})
		}
		return s.prompts.Accepted(ctx, prompts.Acceptance{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch, Binding: *providerResult.Binding})
	case prompts.Uncertain:
		return s.prompts.Uncertain(ctx, prompts.Uncertainty{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch, Reason: providerResult.Reason})
	case prompts.Refused, prompts.Parked:
		if providerResult.FailureCode == providers.FailureTurnEnded {
			return s.prompts.Requeue(ctx, prompts.Requeue{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch})
		}
		reason := providerResult.Reason
		if providerResult.FailureCode != "" {
			reason = providerResult.FailureCode
		}
		return s.prompts.Refused(ctx, prompts.Refusal{
			ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch,
			State: providerResult.State, Reason: reason,
		})
	default:
		return prompts.Record{}, fmt.Errorf("core/session: provider returned unsupported submission state %q", providerResult.State)
	}
}

func (s *Session) makeUncertain(ctx context.Context, claimed prompts.Record, reason string, cause error) error {
	record, err := s.prompts.Uncertain(ctx, prompts.Uncertainty{ActionID: claimed.Outbox.ActionID, ClaimToken: claimed.ClaimToken, AttachmentEpoch: claimed.ClaimEpoch, Reason: reason})
	if err == nil {
		s.outbox.PublishOutbox(record.Outbox)
	}
	return errors.Join(cause, err)
}

func newClaimToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
