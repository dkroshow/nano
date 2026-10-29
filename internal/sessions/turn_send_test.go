package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func observeTestTurnCompletion(t *testing.T, sess *Session, turnID string) {
	t.Helper()
	finish := coreturns.Finish{HostedThreadID: sess.HostedThreadID(), Provider: sess.Provider(), NativeThreadID: sess.stateOwner.ThreadID(), TurnID: turnID, Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}
	if err := sess.runtime.core.Observe(context.Background(), coresession.AdapterEvent{ProviderTurnID: turnID, TurnEvents: []coresession.TurnEvent{{Finished: &finish}}}); err != nil {
		t.Fatal(err)
	}
}

func waitTurnSendDone(t *testing.T, sess *Session, actionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sess.delivery.sendMu.Lock()
		op := sess.delivery.sendOps[actionID]
		sess.delivery.sendMu.Unlock()
		if op != nil {
			select {
			case <-op.done:
				return
			default:
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("send operation %s did not finish", actionID)
}

func readySendTestSession(t *testing.T, store *turns.LegacyJournal, sender func(context.Context, string) (json.RawMessage, error)) *Session {
	t.Helper()
	turnStore, err := turns.OpenStore(context.Background(), store.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(store.Database(), store.Path(), testPromptStore(t, store.Database()))
	if err != nil {
		t.Fatal(err)
	}
	store.BindTransactionStore(turnStore)
	queue.BindTransactionStore(turnStore)
	projection := newDeliveryTestProjection("codex")
	return sessionWithOwners(&Session{id: "nano-session",
		provider: "codex", hostedThreadID: "hosted-thread",
		stateOwner: State{threadID: "native-thread",
			lifecycleState: testLifecycleSnapshot("ready", false)},
		view: View{
			projection: projection},
		delivery: Delivery{turns: turnStore, sendAttempts: store, queuedTurns: queue,
			sendOps: map[string]*turnSendOperation{}, turnSender: sender}})
}

func readyTransactionalSendTestSession(t *testing.T, sender func(context.Context, string) (json.RawMessage, error)) (*Session, *turns.Store) {
	t.Helper()
	db, path, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := testPromptStore(t, db)
	queue, err := newQueuedTurnStore(db, path, store)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newSendAttemptStore(db, path, "transactional-test", store)
	if err != nil {
		t.Fatal(err)
	}
	projection := newDeliveryTestProjection("codex")

	sess := sessionWithOwners(&Session{
		id: "nano-session", provider: "codex", hostedThreadID: "hosted-thread",
		stateOwner: State{threadID: "native-thread",
			lifecycleState: testLifecycleSnapshot("ready", false)},
		view: View{
			projection: projection},
		delivery: Delivery{turns: store, sendAttempts: attempts, queuedTurns: queue,
			sendOps: map[string]*turnSendOperation{}, turnSender: sender},
	})
	return sess, store
}

func TestTransactionalTurnCommitsClaimBeforeProviderAndBindsExactResult(t *testing.T) {
	var store *turns.Store
	var calls atomic.Int32
	providerObserved := make(chan error, 1)
	sess, opened := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		record, attempt, err := store.Get(context.Background(), "transactional-action")
		if err != nil || record.Outbox.State != turns.Delivering || record.ClaimToken == "" || attempt.State != turns.JournalSending || attempt.ClaimToken != record.ClaimToken {
			providerObserved <- fmt.Errorf("provider crossed an uncommitted claim: record=%+v attempt=%+v err=%v", record, attempt, err)
		} else {
			providerObserved <- nil
		}
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	store = opened
	response := postTurn(t, sess.Delivery(), context.Background(), "transactional-action", "deliver once")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if err := <-providerObserved; err != nil {
		t.Fatal(err)
	}
	record, attempt, err := store.Get(context.Background(), "transactional-action")
	if err != nil || record.Outbox.State != turns.Accepted || attempt.State != turns.JournalAccepted || record.ProviderTurnID != "provider-turn" || record.Outbox.Binding == nil {
		t.Fatalf("accepted transaction: record=%+v attempt=%+v err=%v", record, attempt, err)
	}
	retry := postTurn(t, sess.Delivery(), context.Background(), "transactional-action", "deliver once")
	if retry.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("retry status=%d calls=%d body=%s", retry.Code, calls.Load(), retry.Body.String())
	}
}

func TestTransactionalRestoreMarksStaleClaimUncertainBeforeReplay(t *testing.T) {
	var calls atomic.Int32
	sess, store := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("unexpected provider call")
	})
	request := turns.Submission{
		HostedThreadID: "hosted-thread", Provider: model.ProviderCodex, NativeThreadID: "native-thread", ActionID: "stale-action",
		ProviderText: "provider text", PresentationText: "visible text", TextHash: "provider-hash", RequestHash: "request-hash", Origin: turns.HumanOrigin,
	}
	if _, _, err := store.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.Claim(context.Background(), turns.ClaimRequest{HostedThreadID: "hosted-thread", Token: "old-claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	sess.stateOwner.lifecycleState.Epoch = 2
	sess.delivery.restoreSubmissionOutbox()
	record, attempt, err := store.Get(context.Background(), "stale-action")
	if err != nil || record.Outbox.State != turns.Uncertain || attempt.State != turns.JournalUnknown {
		t.Fatalf("restored record=%+v attempt=%+v err=%v", record, attempt, err)
	}
	if calls.Load() != 0 {
		t.Fatal("restore retried an uncertain provider write")
	}
}

func TestTransactionalTransportLossStaysUncertainAndNeverAutoRetries(t *testing.T) {
	var calls atomic.Int32
	sess, store := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("connection lost after provider write")
	})
	response := postTurn(t, sess.Delivery(), context.Background(), "uncertain-action", "do not duplicate")
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"timeout"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	record, attempt, err := store.Get(context.Background(), "uncertain-action")
	if err != nil || record.Outbox.State != turns.Uncertain || attempt.State != turns.JournalUnknown || record.ClaimToken != "" || attempt.ClaimToken == "" {
		t.Fatalf("uncertain record=%+v attempt=%+v err=%v", record, attempt, err)
	}
	retry := postTurn(t, sess.Delivery(), context.Background(), "uncertain-action", "do not duplicate")
	if retry.Code != http.StatusAccepted || calls.Load() != 1 {
		t.Fatalf("retry status=%d calls=%d body=%s", retry.Code, calls.Load(), retry.Body.String())
	}
}

func TestTurnSendWhileActiveIsPersistedBeforeAcknowledgement(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	sess.stateOwner.lifecycleState = testLifecycleSnapshot(sess.stateOwner.connectionStatusLocked(), true)

	rec := postTurn(t, sess.Delivery(), context.Background(), "queued-action", "send this next")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	queued, ok := sess.delivery.queuedTurns.Get("queued-action")
	if !ok || queued.Text != "send this next" {
		t.Fatalf("queued = %#v, found = %v", queued, ok)
	}
	journal, ok := attempts.Get("queued-action")
	if !ok || journal.Status != sendAttemptQueued {
		t.Fatalf("journal = %#v, found = %v", journal, ok)
	}
}

func TestAcceptedSubmissionTextSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	if rec := postTurn(t, sess.Delivery(), context.Background(), "restart-action", "survive the restart"); rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, body = %s", rec.Code, rec.Body.String())
	}

	reloadedAttempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-two")
	if err != nil {
		t.Fatal(err)
	}
	restarted := readySendTestSession(t, reloadedAttempts, nil)
	restarted.delivery.restoreSubmissionOutbox()
	accepted, ok := restarted.delivery.queuedTurns.Get("restart-action")
	if !ok || accepted.State != submissionAccepted || accepted.Text != "survive the restart" {
		t.Fatalf("accepted retained record = %#v, present=%v", accepted, ok)
	}
}

func TestClaudeGenericConfirmationAndScrubFailClosed(t *testing.T) {
	dir := t.TempDir()
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := "synthetic Claude submission"
	if _, _, err := QueuePutSubmission(queue, turns.QueuedTurn{
		ClientActionID: "claude-guard", Provider: "claude", HostedThreadID: "hosted-guard",
		NativeThreadID: "native-guard", ProviderTurnID: "claude:3", State: submissionAccepted,
		Text: text, TextHash: sendTextHash(text),
	}); err != nil {
		t.Fatal(err)
	}
	if err := JournalPut(attempts, turns.SendAttempt{
		ClientActionID: "claude-guard", Provider: "claude", NativeThreadID: "native-guard",
		ProviderTurnID: "claude:3", Status: sendAttemptAccepted, TextHash: sendTextHash(text),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := JournalConfirmOnce(attempts, "claude-guard"); err == nil {
		t.Fatal("generic confirmation accepted a Claude submission")
	}
	if attempt, _ := attempts.Get("claude-guard"); attempt.Confirmed {
		t.Fatal("generic confirmation changed Claude journal")
	}
	if _, ok := queue.Get("claude-guard"); !ok {
		t.Fatal("generic scrub removed Claude outbox row")
	}
	if _, err := JournalConfirmClaudeExactCompleted(attempts, "claude-guard", "native-guard", "claude:4"); err == nil {
		t.Fatal("mismatched exact confirmation accepted Claude submission")
	}
}

func TestAcceptedSubmissionRetryDoesNotMoveOutboxStateBackward(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	for attempt := 0; attempt < 2; attempt++ {
		if rec := postTurn(t, sess.Delivery(), context.Background(), "stable-action", "send once"); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, body = %s", attempt+1, rec.Code, rec.Body.String())
		}
	}
	submission, ok := sess.delivery.queuedTurns.Get("stable-action")
	if !ok || submission.State != submissionAccepted {
		t.Fatalf("retried accepted submission = %#v, found = %v", submission, ok)
	}
}

func TestQueuedTurnSurvivesStoreReloadAndDrainsOnCompletion(t *testing.T) {
	dir := t.TempDir()
	attemptPath := filepath.Join(dir, "send-attempts.json")
	attempts, err := newTestSendAttemptStore(t, attemptPath, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	first := readySendTestSession(t, attempts, nil)
	first.stateOwner.lifecycleState = testLifecycleSnapshot(first.stateOwner.connectionStatusLocked(), true)
	if rec := postTurn(t, first.Delivery(), context.Background(), "queued-restart", "after restart"); rec.Code != http.StatusAccepted {
		t.Fatalf("queue status = %d, body = %s", rec.Code, rec.Body.String())
	}

	reloadedAttempts, err := newTestSendAttemptStore(t, attemptPath, "run-two")
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 1)
	second := readySendTestSession(t, reloadedAttempts, func(_ context.Context, text string) (json.RawMessage, error) {
		delivered <- text
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	second.stateOwner.lifecycleState = testLifecycleSnapshot(second.stateOwner.connectionStatusLocked(), true)
	second.stateOwner.setTurnActive(false, "", 0, true)

	select {
	case text := <-delivered:
		if text != "after restart" {
			t.Fatalf("delivered %q", text)
		}
	case <-time.After(time.Second):
		t.Fatal("queued turn did not drain")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if submission, ok := second.delivery.queuedTurns.Get("queued-restart"); ok && submission.State == submissionAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivered turn remained in durable queue")
		}
		time.Sleep(time.Millisecond)
	}
	waitTurnSendDone(t, second, "queued-restart")
}

func TestDuplicateQueuedActionIsStoredOnce(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, attempts, nil)
	sess.stateOwner.lifecycleState = testLifecycleSnapshot(sess.stateOwner.connectionStatusLocked(), true)
	for i := 0; i < 2; i++ {
		if rec := postTurn(t, sess.Delivery(), context.Background(), "same-queued-action", "only once"); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	if records := QueueSnapshot(sess.delivery.queuedTurns.(*turns.LegacyQueue)); len(records) != 1 || records[0].ClientActionID != "same-queued-action" {
		t.Fatalf("queue records = %#v, want one action", records)
	}
}

func TestQueuedTurnsDrainInSubmissionOrder(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 2)
	var providerCalls atomic.Int32
	sess := readySendTestSession(t, attempts, func(_ context.Context, text string) (json.RawMessage, error) {
		delivered <- text
		turnID := fmt.Sprintf("provider-turn-%d", providerCalls.Add(1))
		return json.RawMessage(`{"turn":{"id":"` + turnID + `"}}`), nil
	})
	sess.stateOwner.lifecycleState = testLifecycleSnapshot(sess.stateOwner.connectionStatusLocked(), true)
	for i, text := range []string{"first next", "second next"} {
		if rec := postTurn(t, sess.Delivery(), context.Background(), fmt.Sprintf("ordered-%d", i), text); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %d status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}

	sess.stateOwner.setTurnActive(false, "", 0, true)
	if got := <-delivered; got != "first next" {
		t.Fatalf("first delivery = %q", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if submission, ok := sess.delivery.queuedTurns.Get("ordered-0"); ok && submission.State == submissionAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first queued delivery did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	waitTurnSendDone(t, sess, "ordered-0")
	// Model the provider lifecycle around the first queued turn. The second is
	// released only by its completion edge.
	observeTestTurnCompletion(t, sess, "provider-turn-1")
	sess.stateOwner.setTurnActive(true, "provider-turn-1", time.Now().UnixMilli(), true)
	sess.stateOwner.setTurnActive(false, "provider-turn-1", 0, true)
	select {
	case got := <-delivered:
		if got != "second next" {
			t.Fatalf("second delivery = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("second queued turn did not drain")
	}
	deadline = time.Now().Add(time.Second)
	for {
		if submission, ok := sess.delivery.queuedTurns.Get("ordered-1"); ok && submission.State == submissionAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second queued delivery did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	waitTurnSendDone(t, sess, "ordered-1")
}

func TestQueuedTurnCanBeCanceledBeforeDelivery(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	sess.stateOwner.lifecycleState = testLifecycleSnapshot(sess.stateOwner.connectionStatusLocked(), true)
	sess.agentExecutions = &unlinkedPromptExecutions{} // a Discard ends any operation the prompt carries
	if rec := postTurn(t, sess.Delivery(), context.Background(), "cancel-queued", "never send"); rec.Code != http.StatusAccepted {
		t.Fatalf("queue status = %d, body = %s", rec.Code, rec.Body.String())
	}

	cancel := httptest.NewRecorder()
	sess.delivery.HandleTurnDeliveryCancel(cancel, httptest.NewRequest(http.MethodDelete, "/api/sessions/nano-session/turn-delivery/cancel-queued", nil), "cancel-queued")
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancel.Code, cancel.Body.String())
	}
	sess.stateOwner.setTurnActive(false, "", 0, true)
	time.Sleep(10 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	if canceled, ok := sess.delivery.queuedTurns.Get("cancel-queued"); !ok || canceled.State != string(turns.Refused) || canceled.Text != "never send" {
		t.Fatalf("retained canceled turn = %#v, present=%v", canceled, ok)
	}
}

// The server decides Resend and Discard from the prompt's stored state, and
// the row offers exactly the actions the server then allows: a refusal
// answers with the reason the browser shows, and changes nothing. Discard
// ends a prompt for good, keeping its text in the store, and Discard of one
// already ended is not an error. Resend retires the prompt and sends its exact
// text once as a new prompt; asked again, it answers with that same prompt.
// Boundary: the HTTP delete, resend and delivery-list routes.
func TestPromptActionsAreDecidedFromTheStoredState(t *testing.T) {
	const text = "no, budget requests go to me"
	ctx := context.Background()
	submit := func(t *testing.T, sess *Session) {
		if _, _, err := sess.delivery.PromptStore().Submit(ctx, turns.Submission{HostedThreadID: "hosted-thread", Provider: model.ProviderCodex, NativeThreadID: "native-thread", ActionID: "prompt",
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
	}
	deliver := func(t *testing.T, sess *Session) {
		if rec := postTurn(t, sess.Delivery(), ctx, "prompt", text); rec.Code != http.StatusOK {
			t.Fatalf("send status = %d, body = %s", rec.Code, rec.Body.String())
		}
		waitTurnSendDone(t, sess, "prompt")
	}
	claim := func(t *testing.T, sess *Session) turns.Record {
		if err := sess.runtime.ensureDeliveryCoordinator(); err != nil {
			t.Fatal(err)
		}
		record, claimed, err := sess.runtime.core.Claim(ctx, sess.stateOwner.lifecycleState.Epoch)
		if err != nil || !claimed {
			t.Fatalf("claim = %v, %v", claimed, err)
		}
		return record
	}
	act := func(t *testing.T, sess *Session, action string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		if action == promptDiscard {
			sess.delivery.HandleTurnDeliveryCancel(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/nano-session/turn-delivery/prompt", nil), "prompt")
		} else {
			sess.delivery.HandleTurnDeliveryResend(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn-delivery/prompt/resend", nil), "prompt")
		}
		return rec
	}
	// Outcomes: allowed, ended (Discard of an ended prompt: done already),
	// refused (409 with a reason), absent (Resend of a prompt Nano never had).
	for _, test := range []struct {
		name    string
		setup   func(t *testing.T, sess *Session) // leaves action "prompt" in its state
		state   turns.DeliveryState               // its state before the action; "" for none
		discard string
		resend  string
		ended   turns.DeliveryState // its state after an allowed or ended Discard
	}{
		{name: "queued", state: turns.Queued, discard: "allowed", resend: "refused", ended: turns.Refused, setup: submit},
		{name: "delivering", state: turns.Delivering, discard: "refused", resend: "refused", setup: func(t *testing.T, sess *Session) {
			submit(t, sess)
			claim(t, sess)
		}},
		{name: "accepted, not yet saved", state: turns.Accepted, discard: "refused", resend: "refused", setup: deliver},
		{name: "saved", state: turns.Confirmed, discard: "refused", resend: "refused", setup: func(t *testing.T, sess *Session) {
			deliver(t, sess)
			if _, settled, err := sess.delivery.PromptStore().Settle(ctx, "prompt"); err != nil || !settled {
				t.Fatalf("settle = %v, %v", settled, err)
			}
		}},
		{name: "uncertain after the delivery deadline", state: turns.Uncertain, discard: "allowed", resend: "allowed", ended: turns.Expired, setup: func(t *testing.T, sess *Session) {
			submit(t, sess)
			record := claim(t, sess)
			answer := sess.delivery.turnSender
			defer func() { sess.delivery.turnSender = answer }()
			sess.delivery.turnSender = func(ctx context.Context, _ string) (json.RawMessage, error) { <-ctx.Done(); return nil, ctx.Err() }
			deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
			defer cancel()
			_, _ = sess.runtime.core.Deliver(deadline, record, sess.runtime.deliveryScope(), sess.runtime.deliverySettings(), false)
		}},
		{name: "held after a Stop", state: turns.Uncertain, discard: "allowed", resend: "allowed", ended: turns.Expired, setup: func(t *testing.T, sess *Session) {
			deliver(t, sess)
			record, _, err := sess.delivery.PromptStore().Get(ctx, "prompt")
			if err != nil {
				t.Fatal(err)
			}
			if _, held, err := sess.delivery.PromptStore().HoldUntaken(ctx, "prompt", record.Revision, turns.ReasonStopped); err != nil || !held {
				t.Fatalf("hold = %v, %v", held, err)
			}
			sess.stateOwner.setTurnActive(false, "", 0, true) // the Stop ended the turn
		}},
		{name: "parked", state: turns.Parked, discard: "allowed", resend: "allowed", ended: turns.Expired, setup: func(t *testing.T, sess *Session) {
			submit(t, sess)
			if _, claimed, err := sess.delivery.PromptStore().Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted-thread", Token: "park", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
			if _, err := sess.delivery.PromptStore().Refused(ctx, turns.Refusal{ActionID: "prompt", ClaimToken: "park", AttachmentEpoch: 1, State: turns.Parked}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "discarded before", state: turns.Expired, discard: "ended", resend: "refused", ended: turns.Expired, setup: func(t *testing.T, sess *Session) {
			deliver(t, sess)
			record, _, err := sess.delivery.PromptStore().Get(ctx, "prompt")
			if err != nil {
				t.Fatal(err)
			}
			if _, held, err := sess.delivery.PromptStore().HoldUntaken(ctx, "prompt", record.Revision, turns.ReasonStopped); err != nil || !held {
				t.Fatalf("hold = %v, %v", held, err)
			}
			if rec := act(t, sess, promptDiscard); rec.Code != http.StatusOK {
				t.Fatalf("first discard = %d %s", rec.Code, rec.Body.String())
			}
		}},
		{name: "not yet reached Nano", discard: "ended", resend: "absent", setup: func(*testing.T, *Session) {}},
	} {
		for _, action := range []string{promptDiscard, promptResend} {
			t.Run(test.name+"/"+action, func(t *testing.T) {
				attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
				if err != nil {
					t.Fatal(err)
				}
				sess := readySendTestSession(t, attempts, nil)
				sess.agentExecutions = &unlinkedPromptExecutions{} // a Discard ends any operation the prompt carries
				var sent atomic.Int32
				var sentText atomic.Value
				sess.delivery.turnSender = func(_ context.Context, delivered string) (json.RawMessage, error) {
					sentText.Store(delivered)
					return json.RawMessage(fmt.Sprintf(`{"turn":{"id":"provider-turn-%d"}}`, sent.Add(1))), nil
				}
				test.setup(t, sess)
				before, _, _ := sess.delivery.PromptStore().Get(ctx, "prompt")
				if before.Outbox.State != test.state {
					t.Fatalf("setup left the prompt %q, want %q", before.Outbox.State, test.state)
				}
				// The row offers exactly the actions the server allows.
				var offered []string
				if test.state != "" {
					offered, _ = promptPresentation(sess.Provider(), before.Outbox)
				}
				outcome := test.discard
				if action == promptResend {
					outcome = test.resend
				}
				if got, want := slices.Contains(offered, action), outcome == "allowed"; got != want {
					t.Fatalf("row offers %v; %s is %s", offered, action, outcome)
				}
				sentBefore := sent.Load()

				rec := act(t, sess, action)
				after, _, _ := sess.delivery.PromptStore().Get(ctx, "prompt")
				switch outcome {
				case "refused":
					var refusal struct {
						Error struct {
							Code    string `json:"code"`
							Message string `json:"message"`
							Details struct {
								ClientActionID string `json:"clientActionId"`
								State          string `json:"state"`
							} `json:"details"`
						} `json:"error"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &refusal); rec.Code != http.StatusConflict || err != nil ||
						refusal.Error.Code != "prompt_action_refused" || refusal.Error.Details.ClientActionID != "prompt" || refusal.Error.Details.State != string(test.state) {
						t.Fatalf("%s = %d %s; want 409 prompt_action_refused for prompt in %s", action, rec.Code, rec.Body.String(), test.state)
					}
					if want := promptActionRefusal(sess.Provider(), before.Outbox, action); want == "" || refusal.Error.Message != want {
						t.Fatalf("refusal reason = %q, want the server's %q", refusal.Error.Message, want)
					}
					if after.Outbox.State != before.Outbox.State || after.Revision != before.Revision {
						t.Fatalf("a refused %s changed the prompt: %s r%d -> %s r%d", action, before.Outbox.State, before.Revision, after.Outbox.State, after.Revision)
					}
				case "absent":
					if rec.Code != http.StatusNotFound {
						t.Fatalf("%s = %d %s; want 404", action, rec.Code, rec.Body.String())
					}
				case "allowed", "ended":
					if action == promptDiscard {
						var answer struct {
							Canceled bool   `json:"canceled"`
							State    string `json:"state"`
						}
						if err := json.Unmarshal(rec.Body.Bytes(), &answer); rec.Code != http.StatusOK || err != nil || !answer.Canceled || answer.State != string(test.ended) {
							t.Fatalf("discard = %d %s; want 200 with state %q", rec.Code, rec.Body.String(), test.ended)
						}
						if test.ended == "" {
							// The lost attempt arrives after all.
							if late := postTurn(t, sess.Delivery(), ctx, "prompt", text); late.Code == http.StatusOK || late.Code == http.StatusAccepted {
								t.Fatalf("a discarded attempt was admitted late: %d %s", late.Code, late.Body.String())
							}
						} else if after.Outbox.State != test.ended || after.ProviderText != text {
							t.Fatalf("discarded prompt = %s %q; want %s with its text kept", after.Outbox.State, after.ProviderText, test.ended)
						}
						break
					}
					var resent struct {
						ClientActionID string `json:"clientActionId"`
						State          string `json:"state"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &resent); rec.Code != http.StatusOK || err != nil || resent.ClientActionID == "" || resent.ClientActionID == "prompt" {
						t.Fatalf("resend = %d %s; want 200 naming the new prompt", rec.Code, rec.Body.String())
					}
					if after.Outbox.State != turns.Expired || after.ProviderText != text {
						t.Fatalf("resent prompt = %s %q; want expired with its text kept", after.Outbox.State, after.ProviderText)
					}
					waitTurnSendDone(t, sess, resent.ClientActionID)
					if got := sent.Load() - sentBefore; got != 1 || sentText.Load() != text {
						t.Fatalf("resend delivered %d times, last %q; want once, the same text", got, sentText.Load())
					}
					// Asked again (a second click, another tab), it answers with
					// the same new prompt and sends nothing more.
					again := act(t, sess, action)
					if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), `"clientActionId":"`+resent.ClientActionID+`"`) {
						t.Fatalf("second resend = %d %s; want the same new prompt %s", again.Code, again.Body.String(), resent.ClientActionID)
					}
					if got := sent.Load() - sentBefore; got != 1 {
						t.Fatalf("a repeated resend delivered %d times", got)
					}
					return
				}
				if got := sent.Load(); got != sentBefore {
					t.Fatalf("%s sent the prompt %d times", action, got-sentBefore)
				}
			})
		}
	}
}

func TestQueuedTurnCompletionStillReleasesNext(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan string, 2)
	var providerCalls atomic.Int32
	sess := readySendTestSession(t, attempts, func(_ context.Context, text string) (json.RawMessage, error) {
		delivered <- text
		turnID := fmt.Sprintf("instant-turn-%d", providerCalls.Add(1))
		return json.RawMessage(`{"turn":{"id":"` + turnID + `"}}`), nil
	})
	sess.stateOwner.lifecycleState = testLifecycleSnapshot(sess.stateOwner.connectionStatusLocked(), true)
	for i, text := range []string{"instant first", "still released"} {
		if rec := postTurn(t, sess.Delivery(), context.Background(), fmt.Sprintf("instant-%d", i), text); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %d status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	sess.stateOwner.setTurnActive(false, "", 0, true)
	select {
	case got := <-delivered:
		if got != "instant first" {
			t.Fatalf("delivery = %q, want instant first", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for instant first")
	}
	waitTurnSendDone(t, sess, "instant-0")
	observeTestTurnCompletion(t, sess, "instant-turn-1")
	sess.stateOwner.setTurnActive(true, "instant-turn-1", time.Now().UnixMilli(), true)
	sess.stateOwner.setTurnActive(false, "instant-turn-1", 0, true)
	select {
	case got := <-delivered:
		if got != "still released" {
			t.Fatalf("delivery = %q, want still released", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for still released")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if submission, ok := sess.delivery.queuedTurns.Get("instant-1"); ok && submission.State == submissionAccepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second instant turn did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	waitTurnSendDone(t, sess, "instant-1")
}

func TestFailedQueuedDeliveryBecomesParkedAndAllowsFollowingTurn(t *testing.T) {
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("provider refused queued write")
		}
		return json.RawMessage(`{"turn":{"id":"healthy-following-turn"}}`), nil
	})
	sess.stateOwner.lifecycleState = testLifecycleSnapshot(sess.stateOwner.connectionStatusLocked(), true)
	for i, text := range []string{"keep failed text", "must not overtake"} {
		if rec := postTurn(t, sess.Delivery(), context.Background(), fmt.Sprintf("failed-queued-%d", i), text); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %d status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	sess.stateOwner.setTurnActive(false, "", 0, true)
	deadline := time.Now().Add(time.Second)
	for {
		attempt, ok := attempts.Get("failed-queued-0")
		if ok && attempt.Status == "parked" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed queued turn did not become parked")
		}
		time.Sleep(time.Millisecond)
	}
	waitTurnSendDone(t, sess, "failed-queued-0")
	waitTurnSendDone(t, sess, "failed-queued-1")
	if got := calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	queued, ok := sess.delivery.queuedTurns.Get("failed-queued-0")
	if !ok || queued.State != submissionParked || queued.Text != "keep failed text" {
		t.Fatalf("failed queue record = %#v, found = %v", queued, ok)
	}
	if view, code := readPrompt(t, sess.Delivery(), "failed-queued-0"); code != http.StatusOK || view.State != string(turns.Parked) || !slices.Equal(view.AllowedActions, []string{promptResend, promptDiscard}) {
		t.Fatalf("parked read = %d %+v", code, view)
	}
}

func TestTurnSendDuplicateActionRunsProviderOnce(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return json.RawMessage(fmt.Sprintf(`{"turn":{"id":"provider-turn-%d"},"providerAccepted":true}`, call)), nil
	})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postTurn(t, sess.Delivery(), context.Background(), "action-1", "hello") }()
	<-started
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- postTurn(t, sess.Delivery(), context.Background(), "action-1", "hello") }()
	close(release)

	for _, rec := range []*httptest.ResponseRecorder{<-firstDone, <-secondDone} {
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}

	// Holding for the next turn is part of the request: the same action asking
	// differently is a different message, even when its text was chosen to
	// hash like the other choice's.
	nextTurn := func(actionID, text string) *httptest.ResponseRecorder {
		body := `{"text":` + string(mustJSON(t, text)) + `,"clientActionId":` + string(mustJSON(t, actionID)) + `,"nextTurn":true}`
		rec := httptest.NewRecorder()
		sess.delivery.HandleTurn(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn", strings.NewReader(body)))
		return rec
	}
	if rec := nextTurn("action-1", "hello"); rec.Code != http.StatusConflict {
		t.Fatalf("changed next-turn choice: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := postTurn(t, sess.Delivery(), context.Background(), "action-2", "hello\x00nextTurn"); rec.Code >= http.StatusBadRequest {
		t.Fatalf("second action: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := nextTurn("action-2", "hello"); rec.Code != http.StatusConflict {
		t.Fatalf("colliding next-turn retry: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestTurnSendOutlivesCanceledBrowserRequest(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	sess := readySendTestSession(t, store, func(ctx context.Context, text string) (json.RawMessage, error) {
		calls.Add(1)
		close(started)
		<-release
		if ctx.Err() != nil {
			t.Fatalf("provider context canceled with browser: %v", ctx.Err())
		}
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postTurn(t, sess.Delivery(), ctx, "action-2", "survive reload") }()
	<-started
	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("canceled browser handler did not return")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- postTurn(t, sess.Delivery(), context.Background(), "action-2", "survive reload") }()
	close(release)
	if rec := <-secondDone; rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestTurnDeliveryReportsBackgroundCompletionAfterBrowserTimeout(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		close(started)
		<-release
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- postTurn(t, sess.Delivery(), ctx, "action-status", "wait for it") }()
	<-started
	cancel()
	<-firstDone

	if view, code := readPrompt(t, sess.Delivery(), "action-status"); code != http.StatusOK || view.State != string(turns.Delivering) {
		t.Fatalf("pending read = %d %+v; want delivering", code, view)
	}

	close(release)
	deadline := time.After(time.Second)
	for {
		view, _ := readPrompt(t, sess.Delivery(), "action-status")
		if view.State == string(turns.Accepted) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("delivery never became accepted: %+v", view)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestTurnSendAcceptedAttemptSurvivesNanoRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send-attempts.json")
	firstStore, err := newTestSendAttemptStore(t, path, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sender := func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	}
	if rec := postTurn(t, readySendTestSession(t, firstStore, sender).Delivery(), context.Background(), "action-3", "once only"); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", rec.Code, rec.Body.String())
	}

	secondStore, err := newTestSendAttemptStore(t, path, "run-two")
	if err != nil {
		t.Fatal(err)
	}
	if rec := postTurn(t, readySendTestSession(t, secondStore, sender).Delivery(), context.Background(), "action-3", "once only"); rec.Code != http.StatusOK {
		t.Fatalf("restart status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestTurnSendReportsPriorRunInFlightAsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send-attempts.json")
	firstStore, err := newTestSendAttemptStore(t, path, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalPut(firstStore, turns.SendAttempt{
		ClientActionID: "action-4", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("uncertain"), Status: sendAttemptSending,
	}); err != nil {
		t.Fatal(err)
	}

	secondStore, err := newTestSendAttemptStore(t, path, "run-two")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := readySendTestSession(t, secondStore, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return nil, nil
	})
	rec := postTurn(t, sess.Delivery(), context.Background(), "action-4", "uncertain")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown_after_restart") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

func TestTurnSendAcceptedActionBypassesReadinessGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send-attempts.json")
	store, err := newTestSendAttemptStore(t, path, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalPut(store, turns.SendAttempt{
		ClientActionID: "accepted-while-starting", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("already accepted"), Status: sendAttemptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := sessionWithOwners(&Session{id: "nano-session",
		provider: "codex",
		stateOwner: State{
			threadID:       "native-thread",
			lifecycleState: testLifecycleSnapshot("starting", false)},
		delivery: Delivery{
			sendAttempts: store,
			sendOps:      map[string]*turnSendOperation{},
			turnSender: func(context.Context, string) (json.RawMessage, error) {
				calls.Add(1)
				return nil, nil
			}}})
	rec := postTurn(t, sess.Delivery(), context.Background(), "accepted-while-starting", "already accepted")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
}

// A read of a prompt Nano does not have says so (404), and the thread's
// journal lists the prompts the store has, by state; a journal row with no
// prompt record (from before the prompt store) has no state and is not listed.
func TestTurnDeliveryUnknownIsExplicitAndThreadListIsAvailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send-attempts.json")
	store, err := newTestSendAttemptStore(t, path, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalPut(store, turns.SendAttempt{
		ClientActionID: "legacy-action", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("legacy"), Status: sendAttemptSending,
	}); err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	if rec := postTurn(t, sess.Delivery(), context.Background(), "listed-action", "listed"); rec.Code != http.StatusOK {
		t.Fatalf("send = %d %s", rec.Code, rec.Body.String())
	}
	waitTurnSendDone(t, sess, "listed-action")
	if _, code := readPrompt(t, sess.Delivery(), "missing"); code != http.StatusNotFound {
		t.Fatalf("unknown read = %d; want 404", code)
	}

	list := httptest.NewRecorder()
	sess.delivery.HandleTurnDeliveryList(list, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery?thread=native-thread", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"clientActionId":"listed-action"`) || !strings.Contains(list.Body.String(), `"state":"accepted"`) ||
		strings.Contains(list.Body.String(), "legacy-action") {
		t.Fatalf("thread list = %d, body = %s", list.Code, list.Body.String())
	}
}

// A provider that refuses a person's prompt does not end it: Nano keeps it
// parked with its text and answers that it holds it, so a browser that lost
// that answer, or any later reader, finds it on the thread with Resend and
// Discard rather than nowhere.
func TestTurnDeliveryUsesHonestRefusalAndTimeoutStatuses(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}

	refused := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		return nil, &protocol.RPCError{Code: -32000, Message: "provider is not accepting input"}
	})
	refusal := postTurn(t, refused.Delivery(), context.Background(), "action-refused", "not now")
	if refusal.Code != http.StatusAccepted || !strings.Contains(refusal.Body.String(), `"reason":"queued_delivery_failed"`) {
		t.Fatalf("refusal = %d %s", refusal.Code, refusal.Body.String())
	}
	if record, _, err := refused.delivery.PromptStore().Get(context.Background(), "action-refused"); err != nil || record.Outbox.State != turns.Parked || record.ProviderText != "not now" {
		t.Fatalf("refused prompt = %s %q, %v; want parked with its text", record.Outbox.State, record.ProviderText, err)
	}

	if view, code := readPrompt(t, refused.Delivery(), "action-refused"); code != http.StatusOK || view.State != string(turns.Parked) ||
		view.Label != "Not delivered: rpc error -32000: provider is not accepting input" {
		t.Fatalf("refused prompt read = %d %+v; want parked, saying why", code, view)
	}
}

func TestTurnDeliveryExposesAcceptedPromptAndStillDedupes(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	if rec := postTurn(t, sess.Delivery(), context.Background(), "action-journal", "confirm me"); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	list := httptest.NewRecorder()
	sess.delivery.HandleTurnDeliveryList(list, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery?thread=native-thread", nil))
	var listed struct {
		Operations []turns.View `json:"operations"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("thread list body = %s: %v", list.Body.String(), err)
	}
	if len(listed.Operations) != 1 || listed.Operations[0].State != string(turns.Accepted) || listed.Operations[0].AllowedActions != nil {
		t.Fatalf("thread list did not present the accepted prompt: %s", list.Body.String())
	}
	if view, code := readPrompt(t, sess.Delivery(), "action-journal"); code != http.StatusOK || view.State != string(turns.Accepted) {
		t.Fatalf("read = %d %+v", code, view)
	}

	// Same action id, after confirmation, still answers idempotently without a
	// second provider turn.
	if rec := postTurn(t, sess.Delivery(), context.Background(), "action-journal", "confirm me"); rec.Code != http.StatusOK {
		t.Fatalf("dedup submit = %d %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// A later status write must not erase provider evidence the matcher already
// recorded; confirmation is monotonic.
func TestSendAttemptConfirmationSurvivesLaterStatusWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send-attempts.json")
	store, err := newTestSendAttemptStore(t, path, "run-one")
	if err != nil {
		t.Fatal(err)
	}
	rec := turns.SendAttempt{
		ClientActionID: "action-race", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("racing"), Status: sendAttemptSending,
	}
	if err := JournalPut(store, rec); err != nil {
		t.Fatal(err)
	}
	if err := JournalConfirm(store, "action-race"); err != nil {
		t.Fatal(err)
	}
	rec.Status = sendAttemptTimeout
	rec.Reason = "late transport timeout"
	if err := JournalPut(store, rec); err != nil {
		t.Fatal(err)
	}

	restarted, err := newTestSendAttemptStore(t, path, "run-two")
	if err != nil {
		t.Fatal(err)
	}
	reloaded, ok := restarted.Get("action-race")
	if !ok || !reloaded.Confirmed || reloaded.ConfirmedAt == "" || reloaded.Status != sendAttemptAccepted || reloaded.Reason != "" {
		t.Fatalf("confirmation did not survive: %#v found=%v", reloaded, ok)
	}
}

// A prompt sent while a Claude turn works goes to the CLI at once, which
// reads it between the turn's steps, instead of waiting for the turn to end:
// Claude's mid-turn delivery. The saved transcript later proves which turn
// absorbed it. Boundary: the HTTP send through the core session, with
// Claude's declared capabilities, to the provider sender.
func TestClaudePromptSentWhileATurnWorksIsDeliveredAtOnce(t *testing.T) {
	ctx := context.Background()
	attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "claude-running")
	if err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	sess := readySendTestSession(t, attempts, func(context.Context, string) (json.RawMessage, error) {
		sent.Add(1)
		return json.RawMessage(`{"turn":{"id":"claude:hosted-thread:2"}}`), nil
	})
	sess.provider = "claude"
	sess.view.projection = newDeliveryTestProjection("claude")
	sess.view.projection.setNativeThreadID("native-thread")
	if err := sess.runtime.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.runtime.coreSession().Close)
	sess.stateOwner.setTurnActive(true, "claude:hosted-thread:1", 0, false)

	if rec := postTurn(t, sess.Delivery(), ctx, "correction", "Use the other file."); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	waitTurnSendDone(t, sess, "correction")
	record, _, err := sess.delivery.PromptStore().Get(ctx, "correction")
	if err != nil || sent.Load() != 1 || record.Outbox.State != turns.Accepted {
		t.Fatalf("sent %d times, state %s, err %v; want delivered once while the turn works", sent.Load(), record.Outbox.State, err)
	}
}

// A prompt sent while a Codex turn works joins that turn through turn/steer,
// naming the turn it saw so Codex refuses rather than join another, and
// carrying its submission id so its saved row can be told from its turn's
// other prompts. A definite "no active turn" answer means Codex never saw it:
// the same prompt waits again, for the next turn. Any other refusal is final.
// A lost connection may follow delivery, so a prompt queued first and then
// steered stays uncertain rather than failed. None starts a turn or resends.
// Boundary: the HTTP send and queue-drain paths through the real Codex sender
// to an app-server.
func TestCodexPromptJoinsTheWorkingTurnBySteer(t *testing.T) {
	for _, test := range []struct {
		name     string
		steerErr string
		dropped  bool // the connection closes before Codex answers
		queued   bool // queued before the turn worked, then drained into it
		state    turns.DeliveryState
		reason   string
	}{
		{name: "joins", state: turns.Accepted},
		{name: "transport lost after queue", dropped: true, queued: true, state: turns.Uncertain},
		{name: "turn ended", steerErr: "no active turn to steer", state: turns.Queued, reason: turns.ReasonNextTurn},
		// Refused, but kept for the person: parked with its text, not resent.
		{name: "other turn", steerErr: "expected active turn id `turn-running` but found `turn-other`", state: turns.Parked},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "steer-test")
			if err != nil {
				t.Fatal(err)
			}
			sess := readySendTestSession(t, attempts, nil)
			fake := newFakeAppServer(t)
			fake.SetSteerError(test.steerErr)
			fake.SetDropSteer(test.dropped)
			sess.runtime.codex = fake.daemon()
			if err := sess.runtime.ensureCoreSession(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.runtime.coreSession().Close)
			if test.queued {
				text := "Use the other file."
				if _, _, err := sess.delivery.PromptStore().Submit(ctx, turns.Submission{HostedThreadID: sess.HostedThreadID(), Provider: model.ProviderCodex, NativeThreadID: sess.stateOwner.ThreadID(),
					ActionID: "correction", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
			}
			sess.stateOwner.setTurnActive(true, "turn-running", 0, false)

			if !test.queued {
				rec := postTurn(t, sess.Delivery(), ctx, "correction", "Use the other file.")
				if test.state == turns.Accepted && rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
				}
			}
			waitTurnSendDone(t, sess, "correction")
			sess.delivery.kickQueuedTurnDrain() // a settled attempt is never sent again
			time.Sleep(50 * time.Millisecond)
			steers := fake.SteerParams()
			if len(steers) != 1 {
				t.Fatalf("turn/steer calls = %d, want 1", len(steers))
			}
			var params struct {
				ThreadID            string                  `json:"threadId"`
				ExpectedTurnID      string                  `json:"expectedTurnId"`
				ClientUserMessageID string                  `json:"clientUserMessageId"`
				Input               []struct{ Text string } `json:"input"`
			}
			if err := json.Unmarshal(steers[0], &params); err != nil {
				t.Fatal(err)
			}
			if params.ThreadID != "native-thread" || params.ExpectedTurnID != "turn-running" ||
				params.ClientUserMessageID != submissionEventID(sess.HostedThreadID(), "correction") ||
				len(params.Input) != 1 || !strings.HasPrefix(params.Input[0].Text, "Use the other file.") {
				t.Fatalf("turn/steer params = %s", steers[0])
			}
			record, attempt, err := sess.delivery.PromptStore().Get(ctx, "correction")
			if err != nil || record.Outbox.State != test.state || test.reason != "" && attempt.Reason != test.reason {
				t.Fatalf("record state=%s reason=%q err=%v; want %s %q", record.Outbox.State, attempt.Reason, err, test.state, test.reason)
			}
			if test.state == turns.Accepted && (record.Outbox.Binding == nil || record.Outbox.Binding.EventID != params.ClientUserMessageID || record.ProviderTurnID != "turn-running") {
				t.Fatalf("binding = %+v turn=%q; want the running turn and the client id", record.Outbox.Binding, record.ProviderTurnID)
			}
			fake.Mu.Lock()
			started := fake.TurnStarts
			fake.Mu.Unlock()
			if started != 0 {
				t.Fatalf("turn/start calls = %d, want 0 while the turn works", started)
			}
		})
	}
}

// A never-sent ordinary prompt queued while a Codex thread could not take it
// must join the turn as soon as the thread is working again, whether it
// reconnects into that turn or an approval it waited on is answered. Nothing
// else would send it until the turn ends.
// Boundary: the session's connection and interaction transitions through the
// real queue drain and Codex sender.
func TestQueuedPromptJoinsTheTurnItsThreadReturnsTo(t *testing.T) {
	for _, path := range []string{"reconnected", "approval answered"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "return-test")
			if err != nil {
				t.Fatal(err)
			}
			sess := readySendTestSession(t, attempts, nil)
			fake := newFakeAppServer(t)
			sess.runtime.codex = fake.daemon()
			// The thread is already off working before the core exists, so no
			// kick from reaching that state can send the prompt queued next:
			// only the return under test can.
			sess.stateOwner.setTurnActiveState(true, "turn-running", 0, false, false)
			if path == "reconnected" {
				sess.setStatus("reconnecting")
			} else {
				sess.delivery.addPendingInteraction(interact.PendingInteraction{ID: "approval-1", Method: "item/commandExecution/requestApproval", TurnID: "turn-running", ThreadID: sess.stateOwner.ThreadID()})
			}
			if err := sess.runtime.ensureCoreSession(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.runtime.coreSession().Close)
			text := "Use the other file."
			if _, _, err := sess.delivery.PromptStore().Submit(ctx, turns.Submission{HostedThreadID: sess.HostedThreadID(), Provider: model.ProviderCodex, NativeThreadID: sess.stateOwner.ThreadID(),
				ActionID: "queued", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
				t.Fatal(err)
			}

			if path == "reconnected" {
				sess.stateOwner.observeConnection(lifecycle.EventReadyWorking)
			} else if _, ok := sess.delivery.claimPendingInteraction("approval-1"); !ok {
				t.Fatal("approval not pending")
			}
			waitTurnSendDone(t, sess, "queued")
			steers := fake.SteerParams()
			var params struct {
				ExpectedTurnID string `json:"expectedTurnId"`
			}
			if len(steers) != 1 || json.Unmarshal(steers[0], &params) != nil || params.ExpectedTurnID != "turn-running" {
				t.Fatalf("turn/steer = %s; want one steer into turn-running", steers)
			}
			if record, _, err := sess.delivery.PromptStore().Get(ctx, "queued"); err != nil || record.Outbox.State != turns.Accepted {
				t.Fatalf("queued prompt state=%s err=%v; want accepted", record.Outbox.State, err)
			}
		})
	}
}
