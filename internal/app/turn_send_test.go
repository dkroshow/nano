package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func observeTestTurnCompletion(t *testing.T, sess *session, turnID string) {
	t.Helper()
	finish := coreturns.Finish{HostedThreadID: sess.hostedThreadID, Provider: sess.provider, NativeThreadID: sess.currentThreadID(), TurnID: turnID, Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}
	if err := sess.core.Observe(context.Background(), coresession.AdapterEvent{ProviderTurnID: turnID, TurnEvents: []coresession.TurnEvent{{Finished: &finish}}}); err != nil {
		t.Fatal(err)
	}
}

func mustQueuedTurnStore(t *testing.T) *queuedTurnStore {
	t.Helper()
	store, err := newTestQueuedTurnStore(t, filepath.Join(t.TempDir(), "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustBindTransactionalTurnStore(t *testing.T, queue sessionQueue, attempts sessionJournal) *turns.Store {
	t.Helper()
	if queue == nil || attempts == nil {
		t.Fatal("transactional turn fixture requires queue and journal views")
	}
	store, err := turns.OpenStore(context.Background(), queue.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue.BindTransactionStore(store)
	attempts.BindTransactionStore(store)
	return store
}

func waitTurnSendDone(t *testing.T, sess *session, actionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		sess.sendMu.Lock()
		op := sess.sendOps[actionID]
		sess.sendMu.Unlock()
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

func readySendTestSession(t *testing.T, store *sendAttemptStore, sender func(context.Context, string) (json.RawMessage, error)) *session {
	t.Helper()
	turnStore, err := turns.OpenStore(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(store.db, store.path, testPromptStore(t, store.db))
	if err != nil {
		t.Fatal(err)
	}
	store.turns, queue.turns = turnStore, turnStore
	projection := newLiveProjection("codex", "hosted-thread")
	projection.SetNativeThreadID("native-thread")
	return &session{id: "nano-session",
		provider: "codex", hostedThreadID: "hosted-thread", threadID: "native-thread",
		projection: projection, turns: turnStore, sendAttempts: store, queuedTurns: queue,
		sendOps: map[string]*turnSendOperation{}, turnSender: sender,
		lifecycleState: testLifecycleSnapshot("ready", false)}
}

func readyTransactionalSendTestSession(t *testing.T, sender func(context.Context, string) (json.RawMessage, error)) (*session, *turns.Store) {
	t.Helper()
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "transactional-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.control.Close() })
	projection := newLiveProjection("codex", "hosted-thread")
	projection.SetNativeThreadID("native-thread")
	sess := &session{
		id: "nano-session", provider: "codex", hostedThreadID: "hosted-thread", threadID: "native-thread",
		projection: projection, turns: stores.turns, sendAttempts: stores.attempts, queuedTurns: stores.queue,
		sendOps: map[string]*turnSendOperation{}, turnSender: sender,
		lifecycleState: testLifecycleSnapshot("ready", false),
	}
	return sess, stores.turns
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
	response := postTurn(t, sess, context.Background(), "transactional-action", "deliver once")
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
	retry := postTurn(t, sess, context.Background(), "transactional-action", "deliver once")
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
	sess.lifecycleState.Epoch = 2
	sess.restoreSubmissionOutbox()
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
	response := postTurn(t, sess, context.Background(), "uncertain-action", "do not duplicate")
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"status":"timeout"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	record, attempt, err := store.Get(context.Background(), "uncertain-action")
	if err != nil || record.Outbox.State != turns.Uncertain || attempt.State != turns.JournalUnknown || record.ClaimToken != "" || attempt.ClaimToken == "" {
		t.Fatalf("uncertain record=%+v attempt=%+v err=%v", record, attempt, err)
	}
	retry := postTurn(t, sess, context.Background(), "uncertain-action", "do not duplicate")
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
	sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)

	rec := postTurn(t, sess, context.Background(), "queued-action", "send this next")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	queued, ok := sess.queuedTurns.Get("queued-action")
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
	if rec := postTurn(t, sess, context.Background(), "restart-action", "survive the restart"); rec.Code != http.StatusOK {
		t.Fatalf("send status = %d, body = %s", rec.Code, rec.Body.String())
	}

	reloadedAttempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-two")
	if err != nil {
		t.Fatal(err)
	}
	restarted := readySendTestSession(t, reloadedAttempts, nil)
	restarted.restoreSubmissionOutbox()
	accepted, ok := restarted.queuedTurns.Get("restart-action")
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
	if _, _, err := queue.putSubmission(queuedTurnRecord{
		ClientActionID: "claude-guard", Provider: "claude", HostedThreadID: "hosted-guard",
		NativeThreadID: "native-guard", ProviderTurnID: "claude:3", State: submissionAccepted,
		Text: text, TextHash: sendTextHash(text),
	}); err != nil {
		t.Fatal(err)
	}
	if err := attempts.put(sendAttemptRecord{
		ClientActionID: "claude-guard", Provider: "claude", NativeThreadID: "native-guard",
		ProviderTurnID: "claude:3", Status: sendAttemptAccepted, TextHash: sendTextHash(text),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.confirmOnce("claude-guard"); err == nil {
		t.Fatal("generic confirmation accepted a Claude submission")
	}
	if attempt, _ := attempts.Get("claude-guard"); attempt.Confirmed {
		t.Fatal("generic confirmation changed Claude journal")
	}
	if _, ok := queue.Get("claude-guard"); !ok {
		t.Fatal("generic scrub removed Claude outbox row")
	}
	if _, err := attempts.confirmClaudeExactCompleted("claude-guard", "native-guard", "claude:4"); err == nil {
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
		if rec := postTurn(t, sess, context.Background(), "stable-action", "send once"); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, body = %s", attempt+1, rec.Code, rec.Body.String())
		}
	}
	submission, ok := sess.queuedTurns.Get("stable-action")
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
	first.lifecycleState = testLifecycleSnapshot(first.connectionStatusLocked(), true)
	if rec := postTurn(t, first, context.Background(), "queued-restart", "after restart"); rec.Code != http.StatusAccepted {
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
	second.lifecycleState = testLifecycleSnapshot(second.connectionStatusLocked(), true)
	second.setTurnActive(false, "", 0, true)

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
		if submission, ok := second.queuedTurns.Get("queued-restart"); ok && submission.State == submissionAccepted {
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
	sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)
	for i := 0; i < 2; i++ {
		if rec := postTurn(t, sess, context.Background(), "same-queued-action", "only once"); rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	if records := sess.queuedTurns.(*queuedTurnStore).snapshot(); len(records) != 1 || records[0].ClientActionID != "same-queued-action" {
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
	sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)
	for i, text := range []string{"first next", "second next"} {
		if rec := postTurn(t, sess, context.Background(), fmt.Sprintf("ordered-%d", i), text); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %d status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}

	sess.setTurnActive(false, "", 0, true)
	if got := <-delivered; got != "first next" {
		t.Fatalf("first delivery = %q", got)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if submission, ok := sess.queuedTurns.Get("ordered-0"); ok && submission.State == submissionAccepted {
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
	sess.setTurnActive(true, "provider-turn-1", time.Now().UnixMilli(), true)
	sess.setTurnActive(false, "provider-turn-1", 0, true)
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
		if submission, ok := sess.queuedTurns.Get("ordered-1"); ok && submission.State == submissionAccepted {
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
	sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)
	sess.agentExecutions = newTestAgentExecutionStore(t, "run-one") // a Discard ends any operation the prompt carries
	if rec := postTurn(t, sess, context.Background(), "cancel-queued", "never send"); rec.Code != http.StatusAccepted {
		t.Fatalf("queue status = %d, body = %s", rec.Code, rec.Body.String())
	}

	cancel := httptest.NewRecorder()
	sess.handleTurnDeliveryCancel(cancel, httptest.NewRequest(http.MethodDelete, "/api/sessions/nano-session/turn-delivery/cancel-queued", nil), "cancel-queued")
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body = %s", cancel.Code, cancel.Body.String())
	}
	sess.setTurnActive(false, "", 0, true)
	time.Sleep(10 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0", got)
	}
	if canceled, ok := sess.queuedTurns.Get("cancel-queued"); !ok || canceled.State != string(turns.Refused) || canceled.Text != "never send" {
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
	submit := func(t *testing.T, sess *session) {
		if _, _, err := sess.turns.Submit(ctx, turns.Submission{HostedThreadID: "hosted-thread", Provider: model.ProviderCodex, NativeThreadID: "native-thread", ActionID: "prompt",
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
	}
	deliver := func(t *testing.T, sess *session) {
		if rec := postTurn(t, sess, ctx, "prompt", text); rec.Code != http.StatusOK {
			t.Fatalf("send status = %d, body = %s", rec.Code, rec.Body.String())
		}
		waitTurnSendDone(t, sess, "prompt")
	}
	claim := func(t *testing.T, sess *session) turns.Record {
		if err := sess.ensureDeliveryCoordinator(); err != nil {
			t.Fatal(err)
		}
		record, claimed, err := sess.core.Claim(ctx, sess.lifecycleState.Epoch)
		if err != nil || !claimed {
			t.Fatalf("claim = %v, %v", claimed, err)
		}
		return record
	}
	act := func(t *testing.T, sess *session, action string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		if action == promptDiscard {
			sess.handleTurnDeliveryCancel(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/nano-session/turn-delivery/prompt", nil), "prompt")
		} else {
			sess.handleTurnDeliveryResend(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn-delivery/prompt/resend", nil), "prompt")
		}
		return rec
	}
	// Outcomes: allowed, ended (Discard of an ended prompt: done already),
	// refused (409 with a reason), absent (Resend of a prompt Nano never had).
	for _, test := range []struct {
		name    string
		setup   func(t *testing.T, sess *session) // leaves action "prompt" in its state
		state   turns.DeliveryState               // its state before the action; "" for none
		discard string
		resend  string
		ended   turns.DeliveryState // its state after an allowed or ended Discard
	}{
		{name: "queued", state: turns.Queued, discard: "allowed", resend: "refused", ended: turns.Refused, setup: submit},
		{name: "delivering", state: turns.Delivering, discard: "refused", resend: "refused", setup: func(t *testing.T, sess *session) {
			submit(t, sess)
			claim(t, sess)
		}},
		{name: "accepted, not yet saved", state: turns.Accepted, discard: "refused", resend: "refused", setup: deliver},
		{name: "saved", state: turns.Confirmed, discard: "refused", resend: "refused", setup: func(t *testing.T, sess *session) {
			deliver(t, sess)
			if _, settled, err := sess.turns.Settle(ctx, "prompt"); err != nil || !settled {
				t.Fatalf("settle = %v, %v", settled, err)
			}
		}},
		{name: "uncertain after the delivery deadline", state: turns.Uncertain, discard: "allowed", resend: "allowed", ended: turns.Expired, setup: func(t *testing.T, sess *session) {
			submit(t, sess)
			record := claim(t, sess)
			answer := sess.turnSender
			defer func() { sess.turnSender = answer }()
			sess.turnSender = func(ctx context.Context, _ string) (json.RawMessage, error) { <-ctx.Done(); return nil, ctx.Err() }
			deadline, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
			defer cancel()
			_, _ = sess.core.Deliver(deadline, record, sess.deliveryScope(), sess.deliverySettings(), false)
		}},
		{name: "held after a Stop", state: turns.Uncertain, discard: "allowed", resend: "allowed", ended: turns.Expired, setup: func(t *testing.T, sess *session) {
			deliver(t, sess)
			record, _, err := sess.turns.Get(ctx, "prompt")
			if err != nil {
				t.Fatal(err)
			}
			if _, held, err := sess.turns.HoldUntaken(ctx, "prompt", record.Revision, turns.ReasonStopped); err != nil || !held {
				t.Fatalf("hold = %v, %v", held, err)
			}
			sess.setTurnActive(false, "", 0, true) // the Stop ended the turn
		}},
		{name: "parked", state: turns.Parked, discard: "allowed", resend: "allowed", ended: turns.Expired, setup: func(t *testing.T, sess *session) {
			submit(t, sess)
			if _, claimed, err := sess.turns.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted-thread", Token: "park", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
			if _, err := sess.turns.Refused(ctx, turns.Refusal{ActionID: "prompt", ClaimToken: "park", AttachmentEpoch: 1, State: turns.Parked}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "discarded before", state: turns.Expired, discard: "ended", resend: "refused", ended: turns.Expired, setup: func(t *testing.T, sess *session) {
			deliver(t, sess)
			record, _, err := sess.turns.Get(ctx, "prompt")
			if err != nil {
				t.Fatal(err)
			}
			if _, held, err := sess.turns.HoldUntaken(ctx, "prompt", record.Revision, turns.ReasonStopped); err != nil || !held {
				t.Fatalf("hold = %v, %v", held, err)
			}
			if rec := act(t, sess, promptDiscard); rec.Code != http.StatusOK {
				t.Fatalf("first discard = %d %s", rec.Code, rec.Body.String())
			}
		}},
		{name: "not yet reached Nano", discard: "ended", resend: "absent", setup: func(*testing.T, *session) {}},
	} {
		for _, action := range []string{promptDiscard, promptResend} {
			t.Run(test.name+"/"+action, func(t *testing.T) {
				attempts, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
				if err != nil {
					t.Fatal(err)
				}
				sess := readySendTestSession(t, attempts, nil)
				sess.agentExecutions = newTestAgentExecutionStore(t, "run-one") // a Discard ends any operation the prompt carries
				var sent atomic.Int32
				var sentText atomic.Value
				sess.turnSender = func(_ context.Context, delivered string) (json.RawMessage, error) {
					sentText.Store(delivered)
					return json.RawMessage(fmt.Sprintf(`{"turn":{"id":"provider-turn-%d"}}`, sent.Add(1))), nil
				}
				test.setup(t, sess)
				before, _, _ := sess.turns.Get(ctx, "prompt")
				if before.Outbox.State != test.state {
					t.Fatalf("setup left the prompt %q, want %q", before.Outbox.State, test.state)
				}
				// The row offers exactly the actions the server allows.
				var offered []string
				if test.state != "" {
					offered, _ = promptPresentation(sess.provider, before.Outbox)
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
				after, _, _ := sess.turns.Get(ctx, "prompt")
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
					if want := promptActionRefusal(sess.provider, before.Outbox, action); want == "" || refusal.Error.Message != want {
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
							if late := postTurn(t, sess, ctx, "prompt", text); late.Code == http.StatusOK || late.Code == http.StatusAccepted {
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
	sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)
	for i, text := range []string{"instant first", "still released"} {
		if rec := postTurn(t, sess, context.Background(), fmt.Sprintf("instant-%d", i), text); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %d status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	sess.setTurnActive(false, "", 0, true)
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
	sess.setTurnActive(true, "instant-turn-1", time.Now().UnixMilli(), true)
	sess.setTurnActive(false, "instant-turn-1", 0, true)
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
		if submission, ok := sess.queuedTurns.Get("instant-1"); ok && submission.State == submissionAccepted {
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
	sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)
	for i, text := range []string{"keep failed text", "must not overtake"} {
		if rec := postTurn(t, sess, context.Background(), fmt.Sprintf("failed-queued-%d", i), text); rec.Code != http.StatusAccepted {
			t.Fatalf("queue %d status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	sess.setTurnActive(false, "", 0, true)
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
	queued, ok := sess.queuedTurns.Get("failed-queued-0")
	if !ok || queued.State != submissionParked || queued.Text != "keep failed text" {
		t.Fatalf("failed queue record = %#v, found = %v", queued, ok)
	}
	if view, code := readPrompt(t, sess, "failed-queued-0"); code != http.StatusOK || view.State != string(turns.Parked) || !slices.Equal(view.AllowedActions, []string{promptResend, promptDiscard}) {
		t.Fatalf("parked read = %d %+v", code, view)
	}
}

func postTurn(t *testing.T, sess *session, ctx context.Context, actionID, text string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"text":` + string(mustJSON(t, text)) + `,"clientActionId":` + string(mustJSON(t, actionID)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	sess.handleTurn(rec, req)
	return rec
}

// readPrompt reads one prompt through the delivery read route.
func readPrompt(t *testing.T, sess *session, actionID string) (promptView, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	sess.handleTurnDeliveryRead(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery/"+actionID, nil), actionID)
	var view promptView
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
	}
	return view, rec.Code
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
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
	go func() { firstDone <- postTurn(t, sess, context.Background(), "action-1", "hello") }()
	<-started
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- postTurn(t, sess, context.Background(), "action-1", "hello") }()
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
		sess.handleTurn(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn", strings.NewReader(body)))
		return rec
	}
	if rec := nextTurn("action-1", "hello"); rec.Code != http.StatusConflict {
		t.Fatalf("changed next-turn choice: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := postTurn(t, sess, context.Background(), "action-2", "hello\x00nextTurn"); rec.Code >= http.StatusBadRequest {
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
	go func() { firstDone <- postTurn(t, sess, ctx, "action-2", "survive reload") }()
	<-started
	cancel()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("canceled browser handler did not return")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- postTurn(t, sess, context.Background(), "action-2", "survive reload") }()
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
	go func() { firstDone <- postTurn(t, sess, ctx, "action-status", "wait for it") }()
	<-started
	cancel()
	<-firstDone

	if view, code := readPrompt(t, sess, "action-status"); code != http.StatusOK || view.State != string(turns.Delivering) {
		t.Fatalf("pending read = %d %+v; want delivering", code, view)
	}

	close(release)
	deadline := time.After(time.Second)
	for {
		view, _ := readPrompt(t, sess, "action-status")
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
	if rec := postTurn(t, readySendTestSession(t, firstStore, sender), context.Background(), "action-3", "once only"); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", rec.Code, rec.Body.String())
	}

	secondStore, err := newTestSendAttemptStore(t, path, "run-two")
	if err != nil {
		t.Fatal(err)
	}
	if rec := postTurn(t, readySendTestSession(t, secondStore, sender), context.Background(), "action-3", "once only"); rec.Code != http.StatusOK {
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
	if err := firstStore.put(sendAttemptRecord{
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
	rec := postTurn(t, sess, context.Background(), "action-4", "uncertain")
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
	if err := store.put(sendAttemptRecord{
		ClientActionID: "accepted-while-starting", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("already accepted"), Status: sendAttemptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	sess := &session{id: "nano-session",
		provider:     "codex",
		threadID:     "native-thread",
		sendAttempts: store,
		sendOps:      map[string]*turnSendOperation{},
		turnSender: func(context.Context, string) (json.RawMessage, error) {
			calls.Add(1)
			return nil, nil
		},
		lifecycleState: testLifecycleSnapshot("starting", false)}
	rec := postTurn(t, sess, context.Background(), "accepted-while-starting", "already accepted")
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
	if err := store.put(sendAttemptRecord{
		ClientActionID: "legacy-action", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("legacy"), Status: sendAttemptSending,
	}); err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	if rec := postTurn(t, sess, context.Background(), "listed-action", "listed"); rec.Code != http.StatusOK {
		t.Fatalf("send = %d %s", rec.Code, rec.Body.String())
	}
	waitTurnSendDone(t, sess, "listed-action")
	if _, code := readPrompt(t, sess, "missing"); code != http.StatusNotFound {
		t.Fatalf("unknown read = %d; want 404", code)
	}

	list := httptest.NewRecorder()
	sess.handleTurnDeliveryList(list, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery?thread=native-thread", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"clientActionId":"listed-action"`) || !strings.Contains(list.Body.String(), `"state":"accepted"`) ||
		strings.Contains(list.Body.String(), "legacy-action") {
		t.Fatalf("thread list = %d, body = %s", list.Code, list.Body.String())
	}
}

func BenchmarkSendAttemptStoreListThreadWithUnrelatedHistory(b *testing.B) {
	directory := b.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "benchmark")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = stores.control.Close() })

	attempts := make([]sendAttemptRecord, 0, 10_651)
	for i := 0; i < 10_648; i++ {
		attempts = append(attempts, sendAttemptRecord{
			ClientActionID: fmt.Sprintf("unrelated-%05d", i), Provider: "codex", NativeThreadID: "unrelated-thread",
			Status: sendAttemptFailed, UpdatedAt: time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano),
		})
	}
	for i := 0; i < 3; i++ {
		attempts = append(attempts, sendAttemptRecord{
			ClientActionID: fmt.Sprintf("matching-%d", i), Provider: "codex", NativeThreadID: "target-thread",
			Status: sendAttemptAccepted, UpdatedAt: time.Unix(int64(20_000+i), 0).UTC().Format(time.RFC3339Nano),
		})
	}
	if err := turns.SaveSendAttempts(context.Background(), stores.control.db, attempts); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := stores.attempts.ListThread("codex", "target-thread"); len(got) != 3 {
			b.Fatalf("matching attempts = %d, want 3", len(got))
		}
	}
}

// The browser's delivery journal read through the one writer connection, so
// at a restart every open tab's journal reads queued in front of
// registrations and durable writes (99 s of waits in a boot's first 30 s).
// It reads through the control store's read pool, and an attempt committed
// before the read is still listed. Boundary: the session's journal route over
// the real control store, with a transaction holding the writer.
func TestTurnDeliveryListDoesNotWaitForTheWriter(t *testing.T) {
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.control.Close() })
	if err := turns.SaveSendAttempts(context.Background(), stores.control.db, []sendAttemptRecord{
		{ClientActionID: "journal-action", Provider: "codex", NativeThreadID: "target", TextHash: "journal-hash", Status: sendAttemptAccepted, CreatedAt: "2026-10-02T11:00:00Z", UpdatedAt: "2026-10-02T11:00:01Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveQueuedTurns(context.Background(), stores.control.db, []queuedTurnRecord{{
		ClientActionID: "journal-action", Provider: "codex", HostedThreadID: "hosted", NativeThreadID: "target", Text: "journal text",
		TextHash: "journal-hash", RequestHash: "journal-hash", State: "accepted", Origin: "human", CreatedAt: "2026-10-02T11:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	sess := &session{id: "nano-session", provider: "codex", threadID: "target", hostedThreadID: "hosted",
		sendAttempts: stores.attempts, queuedTurns: stores.queue, metadata: stores.control}
	tx, err := stores.control.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	list := httptest.NewRecorder()
	listed := make(chan struct{})
	go func() {
		sess.handleTurnDeliveryList(list, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery?thread=target", nil))
		close(listed)
	}()
	select {
	case <-listed:
	case <-time.After(3 * time.Second):
		t.Fatal("the delivery journal waited for the writer connection")
	}
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"clientActionId":"journal-action"`) {
		t.Fatalf("journal = %d %s", list.Code, list.Body.String())
	}
}

func TestSendAttemptStoreListThreadPreservesStatusFieldsAndOrdering(t *testing.T) {
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "turn-completed.sqlite"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.control.Close() })
	if err := turns.SaveSendAttempts(context.Background(), stores.control.db, []sendAttemptRecord{
		{ClientActionID: "older", Provider: "codex", NativeThreadID: "target", TextHash: "older-hash", Status: sendAttemptAccepted, RunID: "old-run", CreatedAt: "2026-09-15T01:00:00Z", UpdatedAt: "2026-09-15T01:01:00Z", Confirmed: true, ProviderTurnID: "older-turn"},
		{ClientActionID: "newer-orphan", Provider: "codex", NativeThreadID: "target", TextHash: "newer-hash", Status: sendAttemptRefused, RunID: "new-run", Reason: "legacy orphan", CreatedAt: "2026-09-15T02:00:00Z", UpdatedAt: "2026-09-15T02:01:00Z", CanonicalMessageID: "newer-message"},
		{ClientActionID: "parked", Provider: "codex", NativeThreadID: "target", TextHash: "parked-hash", Status: "unknown", CreatedAt: "2026-09-15T00:00:00Z", UpdatedAt: "2026-09-15T00:01:00Z"},
		{ClientActionID: "other-provider", Provider: "claude", NativeThreadID: "target", Status: sendAttemptAccepted, UpdatedAt: "2026-09-15T03:01:00Z"},
		{ClientActionID: "other-thread", Provider: "codex", NativeThreadID: "other", Status: sendAttemptAccepted, UpdatedAt: "2026-09-15T04:01:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveQueuedTurns(context.Background(), stores.control.db, []queuedTurnRecord{{
		ClientActionID: "parked", Provider: "codex", HostedThreadID: "hosted", NativeThreadID: "target", Text: "queue text",
		PresentationText: "presentation text", TextHash: "parked-hash", RequestHash: "request", State: "parked", Origin: "human", CreatedAt: "2026-09-15T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}

	got := stores.attempts.ListThread("codex", "target")
	if len(got) != 3 || got[0].ClientActionID != "newer-orphan" || got[1].ClientActionID != "older" || got[2].ClientActionID != "parked" {
		t.Fatalf("thread ordering/filter = %+v", got)
	}
	if got[0].Reason != "legacy orphan" || got[0].CanonicalMessageID != "newer-message" || got[1].TextHash != "older-hash" || !got[1].Confirmed || got[1].ProviderTurnID != "older-turn" {
		t.Fatalf("response fields changed: %+v", got)
	}
	if got[2].Status != "parked" {
		t.Fatalf("outbox status override = %q, want parked", got[2].Status)
	}
	queue, err := turns.LoadQueuedTurns(context.Background(), stores.control.db)
	if err != nil || len(queue) != 1 || queue[0].Text != "queue text" || queue[0].PresentationText != "presentation text" {
		t.Fatalf("queue text changed: %+v, %v", queue, err)
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
	refusal := postTurn(t, refused, context.Background(), "action-refused", "not now")
	if refusal.Code != http.StatusAccepted || !strings.Contains(refusal.Body.String(), `"reason":"queued_delivery_failed"`) {
		t.Fatalf("refusal = %d %s", refusal.Code, refusal.Body.String())
	}
	if record, _, err := refused.turns.Get(context.Background(), "action-refused"); err != nil || record.Outbox.State != turns.Parked || record.ProviderText != "not now" {
		t.Fatalf("refused prompt = %s %q, %v; want parked with its text", record.Outbox.State, record.ProviderText, err)
	}

	if view, code := readPrompt(t, refused, "action-refused"); code != http.StatusOK || view.State != string(turns.Parked) ||
		view.Label != "Not delivered: rpc error -32000: provider is not accepting input" {
		t.Fatalf("refused prompt read = %d %+v; want parked, saying why", code, view)
	}
}

// A stalled provider held every send past Nano's delivery deadline and no pane
// said so. Each expiry counts in its provider's runtime status, the stream a
// pane reads, and ages out of it with no further send.
func TestDeliveryDeadlineExpiryShowsInRuntimeStatusUntilItAgesOut(t *testing.T) {
	store, err := newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	sess := readySendTestSession(t, store, func(context.Context, string) (json.RawMessage, error) {
		return nil, context.DeadlineExceeded // the provider did not answer in time
	})
	srv := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}}}
	sess.deliveryExpiries = newDeliveryExpiries(time.Second, srv.publishProviderRuntime)
	shown := func() int { return sess.projection.Status().Runtime.DeliveryExpiries }

	if rec := postTurn(t, sess, context.Background(), "slow", "slow prompt"); rec.Code != http.StatusAccepted {
		t.Fatalf("send = %d %s; want 202", rec.Code, rec.Body.String())
	}
	waitForCatalogWithin(t, 5*time.Second, func() bool { return shown() == 1 })
	waitForCatalogWithin(t, 5*time.Second, func() bool { return shown() == 0 })
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
	if rec := postTurn(t, sess, context.Background(), "action-journal", "confirm me"); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	list := httptest.NewRecorder()
	sess.handleTurnDeliveryList(list, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery?thread=native-thread", nil))
	var listed struct {
		Operations []promptView `json:"operations"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("thread list body = %s: %v", list.Body.String(), err)
	}
	if len(listed.Operations) != 1 || listed.Operations[0].State != string(turns.Accepted) || listed.Operations[0].AllowedActions != nil {
		t.Fatalf("thread list did not present the accepted prompt: %s", list.Body.String())
	}
	if view, code := readPrompt(t, sess, "action-journal"); code != http.StatusOK || view.State != string(turns.Accepted) {
		t.Fatalf("read = %d %+v", code, view)
	}

	// Same action id, after confirmation, still answers idempotently without a
	// second provider turn.
	if rec := postTurn(t, sess, context.Background(), "action-journal", "confirm me"); rec.Code != http.StatusOK {
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
	rec := sendAttemptRecord{
		ClientActionID: "action-race", Provider: "codex", NativeThreadID: "native-thread",
		TextHash: sendTextHash("racing"), Status: sendAttemptSending,
	}
	if err := store.put(rec); err != nil {
		t.Fatal(err)
	}
	if err := store.confirm("action-race"); err != nil {
		t.Fatal(err)
	}
	rec.Status = sendAttemptTimeout
	rec.Reason = "late transport timeout"
	if err := store.put(rec); err != nil {
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

// settlementFixture is one saved provider transcript indexed by a real
// catalog: an old prompted turn, three turns no Nano prompt created (typed in
// the provider's own client), a newer prompted turn, and a prompted turn still
// running. The shapes follow each provider's saved format; text and ids are
// synthetic.
type settlementFixture struct {
	native     string
	root       history.SourceRoot
	path       string            // the saved transcript file
	turns      map[string]string // stored prompt -> exact provider turn id recorded at acceptance
	savedTurns int
}

var settlementFixtureTurns = []string{"old", "native-1", "native-2", "native-3", "new", "running"}

func writeSettlementFixture(t *testing.T, provider string) settlementFixture {
	t.Helper()
	dir := t.TempDir()
	var data strings.Builder
	fixture := settlementFixture{turns: map[string]string{}, savedTurns: len(settlementFixtureTurns)}
	stored := func(name string) bool { return !strings.HasPrefix(name, "native-") }
	switch provider {
	case "codex":
		fixture.native = "codex-settle"
		fixture.root = history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: dir}
		data.WriteString(`{"type":"session_meta","payload":{"id":"codex-settle","cwd":"/disposable","timestamp":"2026-09-15T20:00:00Z"}}` + "\n")
		for _, name := range settlementFixtureTurns {
			turn := "turn-" + name
			if stored(name) {
				fixture.turns[name] = turn
			}
			fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%q}}\n", turn)
			fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n", name+" prompt")
			fmt.Fprintf(&data, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":%q,\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}}\n", name+"-answer", name+" answer")
			if name != "running" {
				fmt.Fprintf(&data, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%q}}\n", turn)
			}
		}
		fixture.turns["unproven"] = "turn-absent"
		fixture.path = filepath.Join(dir, "rollout-codex-settle.jsonl")
	case "claude":
		fixture.native = "claude-settle"
		fixture.root = history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: dir}
		// Claude saved the prompt whose delivery answer was lost; its row
		// carries the UUID Nano stamped, so it settles though uncertain.
		fixture.savedTurns = len(settlementFixtureTurns) + 1
		fmt.Fprintf(&data, `{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"unanswered prompt"}}`+"\n", submissionEventID("hosted", "unanswered"))
		data.WriteString(`{"type":"assistant","uuid":"unanswered-event","sessionId":"claude-settle","message":{"id":"unanswered-message","role":"assistant","content":[{"type":"text","text":"unanswered answer"}],"stop_reason":"end_turn"}}` + "\n")
		for index, name := range settlementFixtureTurns {
			uuid := name + "-native-row"
			if stored(name) {
				fixture.turns[name] = fmt.Sprintf("claude:shim:%d", index+1)
				uuid = submissionEventID("hosted", name)
			}
			stop := `"end_turn"`
			if name == "running" {
				stop = "null"
			}
			fmt.Fprintf(&data, `{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":%q}}`+"\n", uuid, name+" prompt")
			fmt.Fprintf(&data, `{"type":"assistant","uuid":%q,"sessionId":"claude-settle","message":{"id":%q,"role":"assistant","content":[{"type":"text","text":%q}],"stop_reason":%s}}`+"\n", name+"-event", name+"-message", name+" answer", stop)
		}
		fixture.turns["unproven"] = "claude:shim:99"
		fixture.path = filepath.Join(dir, "project", "claude-settle.jsonl")
	case "grok":
		fixture.native = "22222222-2222-4222-8222-222222222222"
		fixture.root = history.SourceRoot{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: dir}
		session := filepath.Join(dir, "-workspace", fixture.native)
		writeSettlementFile(t, filepath.Join(session, "summary.json"), `{"info":{"id":"`+fixture.native+`","cwd":"/workspace"}}`)
		for index, name := range settlementFixtureTurns {
			prompt := "prompt-" + name
			if stored(name) {
				fixture.turns[name] = prompt
			}
			at := 1893456000 + index*10
			fmt.Fprintf(&data, `{"timestamp":%d,"method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":%q}},"_meta":{"eventId":%q}}}`+"\n", at, fixture.native, name+" prompt", name+"-user-event")
			fmt.Fprintf(&data, `{"timestamp":%d,"method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}},"_meta":{"promptId":%q,"eventId":%q}}}`+"\n", at+1, fixture.native, name+" answer", prompt, name+"-agent-event")
			if name != "running" {
				fmt.Fprintf(&data, `{"timestamp":%d,"method":"_x.ai/session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"turn_completed","prompt_id":%q,"stop_reason":"end_turn"}}}`+"\n", at+2, fixture.native, prompt)
			}
		}
		fixture.turns["unproven"] = "prompt-absent"
		fixture.path = filepath.Join(session, "updates.jsonl")
	}
	writeSettlementFile(t, fixture.path, data.String())
	return fixture
}

// seedUncertainTransactionalTurn stores a prompt whose delivery got no answer.
func seedUncertainTransactionalTurn(t *testing.T, store *turns.Store, hosted string, provider model.Provider, native, action, prompt string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Submit(ctx, turns.Submission{
		HostedThreadID: hosted, Provider: provider, NativeThreadID: native, ActionID: action,
		ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: turns.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim-" + action
	if _, claimed, err := store.Claim(ctx, turns.ClaimRequest{HostedThreadID: hosted, Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim fixture %s: claimed=%v err=%v", action, claimed, err)
	}
	if _, err := store.Uncertain(ctx, turns.Uncertainty{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Reason: "no answer"}); err != nil {
		t.Fatal(err)
	}
}

func writeSettlementFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// settlementHarness is one provider's settlement fixture indexed by a real
// catalog, with the fixture's prompts stored: "old" and "new" are saved in
// ended turns, "running" is saved in a turn still open, "unproven" names a
// turn the provider never saved, and "unanswered" got no delivery answer.
type settlementHarness struct {
	t       *testing.T
	fixture settlementFixture
	// historyPath is the catalog's database file.
	historyPath string
	service     *threadCatalogService
	server      *server
	prompts     *turns.Store
	queue       *queuedTurnStore
	attempts    *sendAttemptStore
	control     *controlMetadataStore
	// interrupts counts the turn/interrupt calls a Codex session made.
	interrupts atomic.Int32
}

// interruptAnsweringCodex is Codex's own runtime operations for sess, with
// turn/interrupt answered here, as Codex would, and counted.
func interruptAnsweringCodex(t *testing.T, sess *session, interrupts *atomic.Int32) providers.Adapter {
	t.Helper()
	operations := codexProviderOperations(sess)
	operations.Interrupt = func(context.Context, providers.InterruptRequest) (providers.InterruptionResult, error) {
		interrupts.Add(1)
		return providers.InterruptionResult{Interrupted: true}, nil
	}
	adapter, err := providerAdapterFactories[model.ProviderCodex](operations)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func newSettlementHarness(t *testing.T, provider string) *settlementHarness {
	t.Helper()
	ctx := context.Background()
	fixture := writeSettlementFixture(t, provider)
	historyPath := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := history.Open(historyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service, err := newThreadCatalogService(ctx, store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{fixture.root},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	service.RequestRefresh() // the explicit discovery a new store needs
	waitForCatalogWithin(t, time.Minute, func() bool {
		id, err := service.ResolveThreadReference(ctx, fixture.native)
		if err != nil {
			return false
		}
		page, err := service.LoadTranscript(ctx, id, transcriptListQuery{Limit: 100, HiddenSpecified: true, View: defaultThreadTranscriptView})
		return err == nil && len(page.Turns) == fixture.savedTurns
	})

	dir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "attempts.json"), "run")
	if err != nil {
		t.Fatal(err)
	}
	prompts := mustBindTransactionalTurnStore(t, queue, attempts)
	seedUncertainTransactionalTurn(t, prompts, "hosted", model.Provider(provider), fixture.native, "unanswered", "unanswered prompt")
	for _, name := range []string{"old", "new", "running", "unproven"} {
		native := fixture.native
		if provider == "claude" && name == "old" {
			// A Claude thread's first prompt is accepted before Claude names
			// its session, under the shim handle that stands in for it.
			native = "cs_shim-handle"
		}
		seedAcceptedTransactionalTurn(t, prompts, "hosted", model.Provider(provider), native, name, fixture.turns[name], name+" prompt")
	}
	h := &settlementHarness{t: t, fixture: fixture, historyPath: historyPath, service: service, server: &server{catalog: service},
		prompts: prompts, queue: queue, attempts: attempts}
	if provider == "claude" {
		// The child that ran under the shim handle announced the saved
		// session; its init recorded that resolution, as captureInit does.
		h.control, _ = newTestHostedControl(t)
		if err := h.control.ensureHostedThread("hosted", "claude", presentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.control.ClaimNativeID("hosted", "claude", "cs_shim-handle", true); err != nil {
			t.Fatal(err)
		}
		pair := nativeResolution{Handle: "cs_shim-handle", Native: fixture.native}
		if err := h.control.ClaimNativeIDResolving("hosted", "claude", fixture.native, false, &pair); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// attach attaches a session to the fixture thread as a restart does. sender,
// if set, is the provider's answer to a live delivery.
func (h *settlementHarness) attach(provider string, sender func(context.Context, string) (json.RawMessage, error)) *session {
	sess := &session{id: "nano-session", provider: provider, hostedThreadID: "hosted", threadID: h.fixture.native,
		projection: newLiveProjection(provider, "hosted"),
		turns:      h.prompts, queuedTurns: h.queue, sendAttempts: h.attempts,
		sendOps: map[string]*turnSendOperation{}, turnSender: sender,
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.setProjectionThreadID(h.fixture.native)
	sess.savedPrompts = h.server.savedPromptsFor(sess)
	sess.savedPromptsIn = h.server.savedPromptRows
	sess.savedTurnFinal = h.server.savedTurnFinal
	sess.metadata = h.control
	if provider == "codex" {
		sess.providerAdapter = interruptAnsweringCodex(h.t, sess, &h.interrupts)
	}
	sess.restoreSubmissionOutbox()
	return sess
}

// appendSaved appends rows to the saved transcript, as the provider does,
// and asks the catalog to index them.
func (h *settlementHarness) appendSaved(t *testing.T, rows string) {
	t.Helper()
	file, err := os.OpenFile(h.fixture.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(rows); err != nil {
		t.Fatal(err)
	}
	file.Close()
	h.service.RequestPathRefresh([]string{h.fixture.path})
}

func (h *settlementHarness) state(t *testing.T, action string) turns.DeliveryState {
	t.Helper()
	record, _, err := h.prompts.Get(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	return record.Outbox.State
}

// Claude gives every prompt its own durable turn. A prompt it takes in while a
// turn works is saved inside that turn, and every prompt that turn consumed
// finishes with its saved final message, whichever execution Claude's live
// result named, and whether that result is recorded before or after the
// saved commit or never. No prompt's operation finishes before its own saved
// row is proven. Usage is the live result's: a turn finished from the saved
// transcript records zeros until its own result reports counts, and a count a
// turn already holds is kept. The answered prompt can settle and finish before
// another member is even accepted. Each member owes its completion until
// then, and owes nothing after. Boundary: the saved-commit settle pass and
// the finished-turn callback's member step, on a real catalog of Claude's
// format.
func TestClaudePromptsAnsweredTogetherFinishWithTheSharedResult(t *testing.T) {
	for _, order := range []string{"result first", "saved first", "answered settles first", "absorbed turn holds usage"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "claude")
			native := h.fixture.native
			executions := newTestAgentExecutionStore(t, "run-1")
			if _, err := executions.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			completed := &completedTurnStore{db: h.queue.db}
			core, err := completed.coreStore(ctx)
			if err != nil {
				t.Fatal(err)
			}
			type prompt struct{ op, turn, transport string }
			sent := map[string]*prompt{"starter": {turn: "claude:shim:200"}, "correction": {turn: "claude:shim:201"}, "late": {turn: "claude:shim:202"}}
			// An accepted Claude prompt's binding records its own stamped event,
			// as the provider runtime's acceptance does.
			ownBinding := func(p *prompt) turns.Binding {
				return turns.Binding{EventID: submissionEventID("hosted", p.transport), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
			}
			accept := func(p *prompt, text string) {
				t.Helper()
				if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
					ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				token := "fixture-claim-" + p.transport
				if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim %s: %v %v", p.transport, claimed, err)
				}
				if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: p.transport, ClaimToken: token, AttachmentEpoch: 1, Binding: ownBinding(p)}); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"starter", "correction", "late"} {
				op, _, err := executions.beginTurn("hosted", name, "hash-"+name)
				if err != nil {
					t.Fatal(err)
				}
				p := sent[name]
				p.op, p.transport = op.ID, op.TransportActionID
				if order != "answered settles first" || name != "starter" {
					accept(p, name+" prompt")
				}
				if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
					t.Fatal(err)
				}
				if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: p.turn, At: time.Now(), Source: "adapter"}); err != nil {
					t.Fatal(err)
				}
			}
			sess := h.attach("claude", nil)
			sess.agentExecutions, sess.completedTurns = executions, completed.coreStore
			if order == "answered settles first" {
				// The starter is delivered but not yet accepted when the others
				// settle; claimed after attach, so it holds the only claim.
				if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: sent["starter"].transport,
					ProviderText: "starter prompt", PresentationText: "starter prompt", TextHash: sendTextHash("starter prompt"), RequestHash: sendTextHash("starter prompt"), Origin: turns.HumanOrigin}); err != nil {
					t.Fatal(err)
				}
				if record, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "late-claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed || record.Outbox.ActionID != sent["starter"].transport {
					t.Fatalf("claim starter: %+v %v %v", record, claimed, err)
				}
			}
			// The live results as the gated result path leaves them: each turn's
			// execution recorded, its answer kept on the open operation.
			input, output := int64(10), int64(5)
			results := func() {
				for name, final := range map[string]string{"correction": "shared", "late": "late"} {
					p := sent[name]
					if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: p.turn,
						FinalMessageID: final + "-final:0", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
						t.Fatal(err)
					}
					if err := executions.ObserveCandidate("claude", native, p.turn, final+"-final:0", "final_answer", final+" answer"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if order != "saved first" {
				results()
			}
			held := int64(7)
			if order == "absorbed turn holds usage" {
				// A retained saved-source finish of the absorbed turn that
				// already counts usage of its own: never overwritten, and no
				// member completion while it does.
				if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: sent["starter"].turn,
					FinalMessageID: "shared-final:0", Outcome: agentTurnCompleted, At: time.Now(), Source: "saved", Usage: coreturns.Usage{InputTokens: &held}}); err != nil {
					t.Fatal(err)
				}
			}
			event := func(name string) string { return submissionEventID("hosted", sent[name].transport) }
			h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"starter prompt"}}
{"type":"assistant","uuid":"tool-event","sessionId":"claude-settle","message":{"id":"tool-message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"sleep 8"}}],"stop_reason":"tool_use"}}
{"type":"user","uuid":"tool-result-event","sessionId":"claude-settle","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}}
{"type":"attachment","uuid":"queued-attachment","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"correction prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"shared-event","sessionId":"claude-settle","message":{"id":"shared-final","role":"assistant","content":[{"type":"text","text":"shared answer"}],"stop_reason":"end_turn"}}
{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"late prompt"}}
{"type":"assistant","uuid":"late-event","sessionId":"claude-settle","message":{"id":"late-final","role":"assistant","content":[{"type":"text","text":"late answer"}],"stop_reason":"end_turn"}}
`, event("starter"), event("correction"), event("late")))
			settled := func(names ...string) {
				waitForCatalogWithin(t, time.Minute, func() bool {
					if _, err := sess.settlePrompts(ctx); err != nil {
						return false
					}
					for _, name := range names {
						if h.state(t, sent[name].transport) != turns.Confirmed {
							return false
						}
					}
					return true
				})
			}
			if order == "answered settles first" {
				settled("correction", "late")
				if op, _ := executions.turn(sent["correction"].op); op.Status != agentTurnCompleted {
					t.Fatalf("answered operation = %+v; want it finished on its own saved proof", op)
				}
				if op, _ := executions.turn(sent["starter"].op); threadvalues.TurnTerminal(op.Status) {
					t.Fatalf("starter operation finished before its prompt was accepted: %+v", op)
				}
				starter := sent["starter"]
				if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: starter.transport, ClaimToken: "late-claim", AttachmentEpoch: 1, Binding: ownBinding(starter)}); err != nil {
					t.Fatal(err)
				}
			}
			settled("starter", "correction", "late")
			if order == "saved first" {
				// The saved transcript alone decides every answer.
				for name, want := range map[string]string{"starter": "shared", "correction": "shared", "late": "late"} {
					if op, _ := executions.turn(sent[name].op); op.Status != agentTurnCompleted || op.FinalItemID != want+"-final:0" {
						t.Fatalf("%s operation before any result = %+v; want completed with its saved %s answer", name, op, want)
					}
				}
				results()
				// What the finished-turn callback does once a Claude result
				// persists: ask for the owed member step.
				sess.requestMemberStep()
			}

			for name, want := range map[string]string{"starter": "shared", "correction": "shared", "late": "late"} {
				p := sent[name]
				if order == "absorbed turn holds usage" && name == "starter" {
					record, err := core.Get(ctx, "hosted", p.turn)
					if err != nil || record.UsageInputTokens == nil || *record.UsageInputTokens != held {
						t.Fatalf("absorbed turn usage = %+v, %v; want its own %d kept", record, err, held)
					}
					if op, _ := executions.turn(p.op); op.Status != agentTurnCompleted || op.FinalItemID != "shared-final:0" {
						t.Fatalf("absorbed operation = %+v; want the shared answer", op)
					}
					continue
				}
				record, err := core.Get(ctx, "hosted", p.turn)
				if err != nil || record.Outcome != agentTurnCompleted || record.FinalMessageID != want+"-final:0" {
					t.Fatalf("%s turn = %+v err=%v; want completed with %s-final", name, record, err, want)
				}
				// A shared reply's usage counts once, on the turn its result named;
				// the absorbed turn records an explicit zero, not a missing value.
				usage := map[bool][3]int64{true: {0, 0, 0}, false: {input, 0, output}}[name == "starter"]
				got := []*int64{record.UsageInputTokens, record.UsageCachedInputTokens, record.UsageOutputTokens}
				for i, want := range usage {
					// The named turn records no cached count (NULL), unless the
					// saved finish came first and recorded zero; the absorbed
					// turn records explicit zeros in all three.
					if i == 1 && name != "starter" && order != "saved first" {
						if got[i] != nil {
							t.Fatalf("%s turn cached usage = %d; want unrecorded", name, *got[i])
						}
						continue
					}
					if got[i] == nil || *got[i] != want {
						t.Fatalf("%s turn usage[%d] = %v; want %d", name, i, got[i], want)
					}
				}
				op, _ := executions.turn(p.op)
				if op.Status != agentTurnCompleted || op.FinalItemID != want+"-final:0" || op.FinalResponse != want+" answer" {
					t.Fatalf("%s operation = status %s final %q %q; want the %s answer", name, op.Status, op.FinalItemID, op.FinalResponse, want)
				}
			}
			wantOwed := map[string]bool{}
			if owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["starter"].transport, sent["correction"].transport, sent["late"].transport}); err != nil || !reflect.DeepEqual(owing, wantOwed) {
				t.Fatalf("owed members = %v, %v; want %v", owing, err, wantOwed)
			}
		})
	}
}

// Two prompts proven in one saved turn that both claim the same absorbed
// durable turn: saved membership is not execution identity, so that shared
// turn is neither finished nor counted, neither prompt's operation completes,
// and both stay owed. The answering prompt, the one claim of its own turn,
// still finishes. Boundary: the saved-commit settle pass and its member step,
// on a real catalog of Claude's format.
func TestClaudeMembersClaimingOneDurableTurnLeaveItUnfinished(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := executions.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type prompt struct{ op, turn, transport string }
	sent := map[string]*prompt{"answerer": {turn: "claude:shim:401"}, "twin-a": {turn: "claude:shim:400"}, "twin-b": {turn: "claude:shim:400"}}
	for _, name := range []string{"answerer", "twin-a", "twin-b"} {
		op, _, err := executions.beginTurn("hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		p := sent[name]
		p.op, p.transport = op.ID, op.TransportActionID
		text := name + " prompt"
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := turns.Binding{EventID: submissionEventID("hosted", p.transport), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: p.transport, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, turn := range []string{"claude:shim:400", "claude:shim:401"} {
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	input, output := int64(10), int64(5)
	if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: "claude:shim:401",
		FinalMessageID: "twin-final", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
		t.Fatal(err)
	}
	if err := executions.ObserveCandidate("claude", native, "claude:shim:401", "twin-final", "final_answer", "twin answer"); err != nil {
		t.Fatal(err)
	}
	event := func(name string) string { return submissionEventID("hosted", sent[name].transport) }
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"answerer prompt"}}
{"type":"attachment","uuid":"twin-a-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"twin-a prompt"}],"source_uuid":%q}}
{"type":"attachment","uuid":"twin-b-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"twin-b prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"twin-shared","sessionId":"claude-settle","message":{"id":"twin-final","role":"assistant","content":[{"type":"text","text":"twin answer"}],"stop_reason":"end_turn"}}
`, event("answerer"), event("twin-a"), event("twin-b")))
	sess := h.attach("claude", nil)
	sess.agentExecutions, sess.completedTurns = executions, completed.coreStore
	waitForCatalogWithin(t, time.Minute, func() bool {
		if _, err := sess.settlePrompts(ctx); err != nil {
			return false
		}
		op, _ := executions.turn(sent["answerer"].op)
		return threadvalues.TurnTerminal(op.Status)
	})
	if shared, err := core.Get(ctx, "hosted", "claude:shim:400"); err != nil || shared.Outcome != "" || shared.UsageInputTokens != nil {
		t.Fatalf("the twice-claimed turn = %+v, %v; want it unfinished and uncounted", shared, err)
	}
	for _, name := range []string{"twin-a", "twin-b"} {
		if op, _ := executions.turn(sent[name].op); threadvalues.TurnTerminal(op.Status) {
			t.Fatalf("%s operation completed on a shared turn: %+v", name, op)
		}
	}
	owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["answerer"].transport, sent["twin-a"].transport, sent["twin-b"].transport})
	if err != nil || !reflect.DeepEqual(owing, map[string]bool{sent["twin-a"].transport: true, sent["twin-b"].transport: true}) {
		t.Fatalf("owed = %v, %v; want only the twins", owing, err)
	}
}

// Failure caught: two prompts saved in one turn whose executions each held a
// live result ("two answering executions") stayed owed for good, and every
// later saved commit read them again and logged it (4,089 lines from four
// turns in one log; 128 Claude prompts owed on 2026-10-21). The saved turn
// alone decides: both finish with its final message, nothing more is logged,
// and nothing stays owed. Boundary: the saved-commit settle pass and its
// member step on a real catalog of Claude's format.
func TestClaudePromptsWithTheirOwnLiveResultsInOneSavedTurnBothFinish(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := executions.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transports := map[string]string{}
	turnsByName := map[string]string{"first": "claude:shim:500", "second": "claude:shim:501"}
	for _, name := range []string{"first", "second"} {
		op, _, err := executions.beginTurn("hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		transports[name] = op.TransportActionID
		turn, text := turnsByName[name], name+" prompt"
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := turns.Binding{EventID: submissionEventID("hosted", op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
		// Each member turn holds its own provider result: two answering executions.
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn,
			FinalMessageID: "shared-final", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := executions.ObserveCandidate("claude", native, turnsByName["first"], "shared-final", "final_answer", "one answer"); err != nil {
		t.Fatal(err)
	}
	event := func(name string) string { return submissionEventID("hosted", transports[name]) }
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"first prompt"}}
{"type":"attachment","uuid":"second-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"second prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"shared","sessionId":"claude-settle","message":{"id":"shared-final","role":"assistant","content":[{"type":"text","text":"one answer"}],"stop_reason":"end_turn"}}
`, event("first"), event("second")))
	var logs syncBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	sess := h.attach("claude", nil)
	sess.agentExecutions, sess.completedTurns = executions, completed.coreStore
	// Owing is empty before saved proof confirms anything, so the wait needs
	// positive evidence: both operations finished.
	waitForCatalogWithin(t, time.Minute, func() bool {
		if _, err := sess.settlePrompts(ctx); err != nil {
			return false
		}
		for _, transport := range transports {
			if op, ok, err := executions.PersistedTurnForTransportAction(ctx, "hosted", transport); err != nil || !ok || op.Status != agentTurnCompleted {
				return false
			}
		}
		return true
	})
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{transports["first"], transports["second"]}); err != nil || len(owing) != 0 {
		t.Fatalf("owed = %v, %v; want none", owing, err)
	}
	for range 3 { // later saved commits on the thread
		if _, err := sess.settlePrompts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for name, transport := range transports {
		persisted, ok, err := executions.PersistedTurnForTransportAction(ctx, "hosted", transport)
		if err != nil || !ok {
			t.Fatalf("%s operation unreadable: %v", name, err)
		}
		if op, _ := executions.turn(persisted.ID); op.Status != agentTurnCompleted || op.FinalItemID != "shared-final:0" || op.FinalResponse != "one answer" {
			t.Fatalf("%s operation = %+v; want the saved turn's answer", name, op)
		}
	}
	if strings.Contains(logs.String(), "Claude") {
		t.Fatalf("the settled turn was logged:\n%s", logs.String())
	}
}

// Failure caught: a reboot killed Claude mid-turn (2026-10-22), and four
// prompts read "accepted" for good. The cut-off turn never ends in the saved
// file, so the three prompts it took never settled and their operations stayed
// running. The fourth was still queued when the process died, so no saved row
// carries it; Nano refused both Resend and Discard. testdata/claude-process-death
// is a scrubbed copy of that file from the cut-off turn's prompt to the first
// turn after the restart, which the person stopped. A lost prompt accepted
// under a fresh session's shim handle stayed accepted too, until its session
// was read through the handle's recorded resolution (found in review).
// Boundary: the settlement pass and owed pass over a real saved file and the
// prompt and operation stores.
func TestClaudePromptsAProcessDeathCutOffSettleAndTheOneNeverTakenWaitsForThePerson(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := "claude-process-death"
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := executions.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The shim's spool position when Nano handed each prompt over, as stored.
	// neverTakenUnderHandle stands for a prompt a fresh session accepts under
	// its shim handle before Claude names the session; the handle's recorded
	// resolution names the same session as the others.
	names := []string{"opener", "absorbed", "absorbedLater", "neverTaken", "neverTakenUnderHandle", "afterRestart"}
	seqs := map[string]int{"opener": 44236, "absorbed": 44469, "absorbedLater": 44923, "neverTaken": 45015, "neverTakenUnderHandle": 45017, "afterRestart": 45020}
	handle := "cs_process-death-handle"
	if err := h.control.ClaimNativeID("hosted", "claude", handle, true); err != nil {
		t.Fatal(err)
	}
	if err := h.control.ClaimNativeIDResolving("hosted", "claude", native, false, &nativeResolution{Handle: handle, Native: native}); err != nil {
		t.Fatal(err)
	}
	transports := map[string]string{}
	for _, name := range names {
		native := native
		if name == "neverTakenUnderHandle" {
			native = handle
		}
		op, _, err := executions.beginTurn("hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		transports[name] = op.TransportActionID
		turn, text := fmt.Sprintf("claude:hosted:%d", seqs[name]), name+" prompt"
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.AgentOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := turns.Binding{EventID: submissionEventID("hosted", op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/claude-process-death/saved.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.NewReplacer("{{NATIVE}}", native,
		"{{OPENER}}", submissionEventID("hosted", transports["opener"]),
		"{{ABSORBED}}", submissionEventID("hosted", transports["absorbed"]),
		"{{ABSORBED_LATER}}", submissionEventID("hosted", transports["absorbedLater"]),
		"{{AFTER_RESTART}}", submissionEventID("hosted", transports["afterRestart"])).Replace(string(raw))
	if strings.Contains(rows, "{{") {
		t.Fatal("fixture placeholder left unreplaced")
	}
	path := filepath.Join(filepath.Dir(h.fixture.path), native+".jsonl")
	if err := os.WriteFile(path, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	h.service.RequestRefresh()
	waitForCatalogWithin(t, time.Minute, func() bool {
		id, err := h.service.ResolveThreadReference(ctx, native)
		if err != nil {
			return false
		}
		page, err := h.service.LoadTranscript(ctx, id, transcriptListQuery{Limit: 100, HiddenSpecified: true, View: defaultThreadTranscriptView})
		return err == nil && len(page.Turns) == 2
	})
	sess := h.attach("claude", nil)
	sess.agentExecutions, sess.completedTurns = executions, completed.coreStore
	outcomes := func() string {
		var got []string
		for _, name := range names {
			op := ""
			if persisted, ok, err := executions.PersistedTurnForTransportAction(ctx, "hosted", transports[name]); err == nil && ok {
				turn, _ := executions.turn(persisted.ID)
				op = turn.Status
			}
			got = append(got, fmt.Sprintf("%s: prompt %s, operation %s", name, h.state(t, transports[name]), op))
		}
		return strings.Join(got, "; ")
	}
	want := "opener: prompt confirmed, operation interrupted; absorbed: prompt confirmed, operation interrupted; " +
		"absorbedLater: prompt confirmed, operation interrupted; neverTaken: prompt uncertain, operation running; " +
		"neverTakenUnderHandle: prompt uncertain, operation running; afterRestart: prompt confirmed, operation interrupted"
	for deadline := time.Now().Add(time.Minute); outcomes() != want; time.Sleep(50 * time.Millisecond) {
		if _, err := sess.settlePrompts(ctx); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("outcomes:\n%s\nwant:\n%s", outcomes(), want)
		}
	}
	never, _, err := h.prompts.Get(ctx, transports["neverTaken"])
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{promptResend, promptDiscard} {
		if refusal := promptActionRefusal("claude", never.Outbox, action); refusal != "" {
			t.Fatalf("%s of the prompt Claude never took refused: %s", action, refusal)
		}
	}
	if never.Outbox.Reason != turns.ReasonTurnEnded {
		t.Fatalf("never-taken reason = %q; want %q", never.Outbox.Reason, turns.ReasonTurnEnded)
	}
}

// Failure caught: an owed prompt on a thread no session holds was never
// revisited. The member step ran only in an attached session, so an exited
// thread's prompts stayed owed and their operations open for good (two of the
// six threads named for repair on 2026-10-21 had exited). One catalog commit
// of the thread's file finishes them by the same rule, with no provider: the
// operation completes then, not in a later saved-result read no pass would
// revisit, and the parent subscribed to the thread is told, though no
// finished-turn hook ran. Boundary: one catalog commit on a server with no
// session, the production saved-result reader installed, on a real catalog
// of Claude's format.
func TestClaudeOwedPromptOnAThreadWithNoSessionFinishesOnASavedCommit(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	parent, _, err := executions.beginSession("parent", "parent-hash", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := executions.beginSessionWithSubscriptions("child", "child-hash", "claude", presentationBackground, parent, subscriptions.Choices{Inbox: subscriptions.Send})
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(child.ID, "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := executions.control.ClaimNativeID(child.ID, "claude", native, false); err != nil {
		t.Fatal(err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	h.server.turns, h.server.agentExecutions, h.server.completedTurns, h.server.controlMetadata = h.prompts, executions, newCompletedTurnSource(completed), executions.control
	executions.useSavedResults(h.server.savedOperationResult, h.server.savedOperationResultFinished)
	client := &feedSubscriptionClient{s: h.server}
	h.server.subscriptions = subscriptions.NewDispatcher(executions.control.db, client, nil, nil)
	op, _, err := executions.beginTurn(child.ID, "exited", "hash-exited")
	if err != nil {
		t.Fatal(err)
	}
	turn, text := "claude:shim:600", "exited prompt"
	if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: child.ID, Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
		ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.AgentOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: child.ID, Token: "claim-exited", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	binding := turns.Binding{EventID: submissionEventID(child.ID, op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
	if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-exited", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
	if err := executions.MarkTurnDelivery(op.ID, turn, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	// Confirmed and owed before the thread's session ended.
	if _, settled, err := h.prompts.Settle(ctx, op.TransportActionID); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	h.server.subscriptions.Sweep("") // the parent's cursor, as a running Nano holds it
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"exited prompt"}}
{"type":"assistant","uuid":"exited-answer","sessionId":"claude-settle","message":{"id":"exited-final","role":"assistant","content":[{"type":"text","text":"answered after all"}],"stop_reason":"end_turn"}}
`, submissionEventID(child.ID, op.TransportActionID)))
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := h.server.savedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID(child.ID, op.TransportActionID)}})
		return err == nil && len(rows) == 1
	})
	threadID, err := h.service.ResolveThreadReference(ctx, native)
	if err != nil {
		t.Fatal(err)
	}
	h.server.confirmDurableThread(ctx, transcriptChange{ThreadID: threadID, Provider: "claude", NativeThreadID: native})
	if finished, _ := executions.turn(op.ID); finished.Status != agentTurnCompleted || finished.FinalItemID != "exited-final:0" || finished.FinalResponse != "answered after all" {
		t.Fatalf("operation after the commit = %+v; want its saved answer", finished)
	}
	if owing, err := h.prompts.Owing(ctx, child.ID, []string{op.TransportActionID}); err != nil || len(owing) != 0 {
		t.Fatalf("owed after the commit = %v, %v; want none", owing, err)
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		_, messages := client.counts()
		return messages == 1
	})
}

// Failure caught: the completion feed published a Claude execution's stored
// live final, which before 2026-10-19 could be another prompt's answer, and
// the observer could not find any Claude answer by the execution's turn id.
// The feed publishes the rule's answer and the saved turn that holds it,
// never the stored final in its place: it holds its cursor before a completed
// row whose prompt Claude has not saved yet, and publishes it once saved; an
// execution two prompts claim publishes no answer.
// Boundary: the public completed-turn feed on a real catalog of Claude's
// format.
func TestCompletedTurnFeedPublishesTheClaudeAnswerItsSavedTurnHolds(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.server.turns, h.server.completedTurns, h.server.controlMetadata = h.prompts, newCompletedTurnSource(completed), h.control
	h.server.agentExecutions = newTestAgentExecutionStore(t, "run")
	turnOf := map[string]string{"answered": "claude:shim:701", "twin-a": "claude:shim:703", "twin-b": "claude:shim:703", "unsaved": "claude:shim:702"}
	for _, action := range []string{"answered", "twin-a", "twin-b", "unsaved"} {
		turn := turnOf[action]
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: action,
			ProviderText: action, PresentationText: action, TextHash: sendTextHash(action), RequestHash: sendTextHash(action), Origin: turns.AgentOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		binding := turns.Binding{EventID: submissionEventID("hosted", action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if action == "twin-b" {
			continue // its twin's execution is its own too
		}
		// The live result as a wrong binding left it for the first.
		final := map[string]string{"answered": "earlier-answer:0", "twin-a": "retained:0", "unsaved": "unsaved-final:0"}[action]
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, ActionID: action,
			FinalMessageID: final, Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"answered"}}
{"type":"assistant","uuid":"answered-final","sessionId":"claude-settle","message":{"id":"right-final","role":"assistant","content":[{"type":"text","text":"the right answer"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", "answered")))
	feed := func(cursor string) interact.CompletedTurnPage {
		t.Helper()
		rec := httptest.NewRecorder()
		h.server.handleCompletedTurns(rec, httptest.NewRequest(http.MethodGet, "/completed-turns?after="+cursor, nil), "hosted")
		var page interact.CompletedTurnPage
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil {
			t.Fatalf("feed = %d %s", rec.Code, rec.Body.String())
		}
		return page
	}
	var page interact.CompletedTurnPage
	waitForCatalogWithin(t, time.Minute, func() bool {
		page = feed("")
		return len(page.Items) == 2
	})
	rows, err := h.server.savedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", "answered")}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("saved rows = %+v, %v", rows, err)
	}
	if got := page.Items[0]; got.TurnID != turnOf["answered"] || got.FinalMessageID != "right-final:0" || got.SavedTurnID != rows[0].Item.TurnKey || got.NativeThreadID != native {
		t.Fatalf("feed = %+v; want the saved turn's right-final:0 in turn %s", page, rows[0].Item.TurnKey)
	}
	if got := page.Items[1]; got.TurnID != turnOf["twin-a"] || got.FinalMessageID != "" || page.NextCursor != turnOf["twin-a"] {
		t.Fatalf("feed = %+v; want the twice-claimed execution with no answer, the cursor held at %s", page, turnOf["twin-a"])
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"unsaved"}}
{"type":"assistant","uuid":"unsaved-answer","sessionId":"claude-settle","message":{"id":"unsaved-final","role":"assistant","content":[{"type":"text","text":"saved now"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", "unsaved")))
	waitForCatalogWithin(t, time.Minute, func() bool {
		page = feed(turnOf["twin-a"])
		return len(page.Items) == 1
	})
	if got := page.Items[0]; got.TurnID != turnOf["unsaved"] || got.FinalMessageID != "unsaved-final:0" {
		t.Fatalf("feed after the save = %+v", page)
	}
}

// Failure caught (design audit): three operations that failed with another
// prompt's live answer (aop_f697b23a, aop_868a30b2, aop_78827850) resolve,
// on a projection the older reader built, to a later turn's answer. Their
// late read runs at startup, before the upgrade re-reads the file, and once it
// no longer required the live final it would complete them with that wrong
// answer for good. The read waits for the current reader's projection, then
// takes the answer of the saved turn that consumed the prompt, whatever final
// the live result named. A reader upgrade can also make the older projection
// the current reader's without a re-read, by proving its file unaffected; that
// proof must wake the waiting read as a re-read's commit does, or the
// operation stays failed until its file next changes. Boundary: the operation
// store's startup and commit-driven late reads through the server's saved
// answer, on a real catalog of Claude's format, with no session.
func TestClaudeLateAnswerWaitsForTheCurrentReaderThenTakesItsSavedTurns(t *testing.T) {
	for _, upgrade := range []string{"re-read", "proved unaffected"} {
		t.Run(upgrade, func(t *testing.T) { testClaudeLateAnswerAfterReaderUpgrade(t, upgrade) })
	}
}

func testClaudeLateAnswerAfterReaderUpgrade(t *testing.T, upgrade string) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	first := newTestAgentExecutionStore(t, "run-1")
	if _, err := first.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := first.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	op, _, err := first.beginTurn("hosted", "late", "hash-late")
	if err != nil {
		t.Fatal(err)
	}
	turn := "claude:shim:800"
	if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: op.TransportActionID,
		ProviderText: "late prompt", PresentationText: "late prompt", TextHash: sendTextHash("late prompt"), RequestHash: sendTextHash("late prompt"), Origin: turns.AgentOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-late", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	binding := turns.Binding{EventID: submissionEventID("hosted", op.TransportActionID), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
	if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: op.TransportActionID, ClaimToken: "claim-late", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
	// Failed after its wait, holding the live final a wrong binding gave it.
	first.mu.Lock()
	rec := first.turns[op.ID]
	rec.Status, rec.ProviderTurnID, rec.NativeThreadID, rec.FinalItemID = agentTurnFailed, turn, native, "earlier-answer:0"
	rec.Error = &agentExecutionError{Source: "saved", Code: "result_not_saved", Message: "not saved"}
	rec.Revision++
	first.setTurnLocked(op.ID, &rec)
	err = first.persistAndNotifyLocked()
	first.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"late prompt"}}
{"type":"assistant","uuid":"late-answer","sessionId":"claude-settle","message":{"id":"right-final","role":"assistant","content":[{"type":"text","text":"the right answer"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", op.TransportActionID)))
	threadID, err := h.service.ResolveThreadReference(ctx, native)
	if err != nil {
		t.Fatal(err)
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := h.server.savedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", op.TransportActionID)}})
		return err == nil && len(rows) == 1
	})
	db, err := sql.Open("sqlite", h.historyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reader := func(version string) {
		t.Helper()
		if _, err := db.Exec(`UPDATE conversation_projections SET parser_version=? WHERE thread_id=? AND state='active'`, version, threadID); err != nil {
			t.Fatal(err)
		}
	}
	current := history.CurrentTranscriptParserVersion(history.ProviderClaude)
	if upgrade == "re-read" {
		reader("claude-reader-v34")
	} else {
		// One reader behind, under an upgrade whose marker this file lacks.
		reader(strings.Replace(current, "claude-reader-v36", "claude-reader-v35", 1))
		h.service.setTranscriptChanged(h.server.confirmDurableThread)
	}

	// Nano starts: the operation store reloads and reads not-saved answers.
	h.server.turns, h.server.controlMetadata = h.prompts, h.control
	store, err := newControlAgentExecutionStore(first.control, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	h.server.agentExecutions = store
	var reads atomic.Int32
	store.useSavedResults(func(ctx context.Context, rec agentTurnRecord) (string, string, error) {
		defer reads.Add(1)
		return h.server.savedOperationResult(ctx, rec)
	}, nil)
	waitForCatalogWithin(t, time.Minute, func() bool { return reads.Load() >= 1 })
	if got, _ := store.turn(op.ID); got.Status != agentTurnFailed {
		t.Fatalf("operation on the older reader's projection = %+v; want it still waiting", got)
	}
	if upgrade == "re-read" {
		reader(current)
		waitForCatalogWithin(t, time.Minute, func() bool {
			h.server.confirmDurableThread(ctx, transcriptChange{ThreadID: threadID, Provider: "claude", NativeThreadID: native})
			got, _ := store.turn(op.ID)
			return got.Status == agentTurnCompleted
		})
	} else {
		// The read has finished and parked, and no earlier commit's
		// notification is still due, so only the proof can wake it.
		waitForCatalogWithin(t, time.Minute, func() bool {
			backlog, inFlight := h.service.transcriptChangeStatus()
			store.mu.Lock()
			defer store.mu.Unlock()
			job := store.savedResultJobs[op.ID]
			return backlog == 0 && inFlight == 0 && job != nil && !job.reading
		})
		if got, _ := store.turn(op.ID); got.Status != agentTurnFailed {
			t.Fatalf("operation before the proof = %+v; want it still waiting", got)
		}
		if _, _, err := h.service.RunTranscriptParserBackfillBatch(ctx, 100); err != nil {
			t.Fatal(err)
		}
		waitForCatalogWithin(t, time.Minute, func() bool {
			got, _ := store.turn(op.ID)
			return got.Status == agentTurnCompleted
		})
	}
	if got, _ := store.turn(op.ID); got.FinalItemID != "right-final:0" || got.FinalResponse != "the right answer" {
		t.Fatalf("operation = %+v; want the answer of the saved turn that consumed its prompt", got)
	}
}

// An agent prompt answered with another's result whose own operation row
// retention has since removed: a missing row is not proof it never had one,
// so its durable turn may finish but its owed completion stays, while the
// answering prompt, whose row is retained and exact, clears. Boundary: the
// saved-commit settle pass and its member step on a real catalog of Claude's
// format, with the operation owner's committed rows.
func TestClaudeAgentMemberWithoutItsRetainedOperationStaysOwed(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	executions := newTestAgentExecutionStore(t, "run-1")
	if _, err := executions.ensureSessionWithIdentity("hosted", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession("hosted", "nano-session", native, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type prompt struct{ op, turn, transport string }
	sent := map[string]*prompt{"answerer": {turn: "claude:shim:501"}, "absorbed": {turn: "claude:shim:500"}}
	for _, name := range []string{"answerer", "absorbed"} {
		op, _, err := executions.beginTurn("hosted", name, "hash-"+name)
		if err != nil {
			t.Fatal(err)
		}
		p := sent[name]
		p.op, p.transport = op.ID, op.TransportActionID
		text := name + " prompt"
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: p.transport,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.AgentOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + name, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", name, claimed, err)
		}
		binding := turns.Binding{EventID: submissionEventID("hosted", p.transport), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &p.turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: p.transport, ClaimToken: "claim-" + name, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if err := executions.MarkTurnDelivery(op.ID, p.turn, agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: p.turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	input, output := int64(10), int64(5)
	if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: sent["answerer"].turn,
		FinalMessageID: "kept-final", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
		t.Fatal(err)
	}
	if err := executions.ObserveCandidate("claude", native, sent["answerer"].turn, "kept-final", "final_answer", "kept answer"); err != nil {
		t.Fatal(err)
	}
	// Retention removes the absorbed prompt's operation from the committed
	// rows and, on its reload, from the cache.
	if _, err := executions.control.db.Exec(`DELETE FROM hosted_turn_operations WHERE operation_id=?`, sent["absorbed"].op); err != nil {
		t.Fatal(err)
	}
	executions.mu.Lock()
	executions.resetControlCachesLocked()
	reloadErr := loadControlExecutionRows(executions)
	executions.mu.Unlock()
	if reloadErr != nil {
		t.Fatal(reloadErr)
	}
	event := func(name string) string { return submissionEventID("hosted", sent[name].transport) }
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"answerer prompt"}}
{"type":"attachment","uuid":"absorbed-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"absorbed prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"kept-shared","sessionId":"claude-settle","message":{"id":"kept-final","role":"assistant","content":[{"type":"text","text":"kept answer"}],"stop_reason":"end_turn"}}
`, event("answerer"), event("absorbed")))
	sess := h.attach("claude", nil)
	sess.agentExecutions, sess.completedTurns = executions, completed.coreStore
	// Owing is empty before saved proof confirms anything, so the wait needs
	// positive evidence: both prompts confirmed and the absorbed turn finished.
	waitForCatalogWithin(t, time.Minute, func() bool {
		if _, err := sess.settlePrompts(ctx); err != nil {
			return false
		}
		if h.state(t, sent["answerer"].transport) != turns.Confirmed || h.state(t, sent["absorbed"].transport) != turns.Confirmed {
			return false
		}
		absorbed, err := core.Get(ctx, "hosted", sent["absorbed"].turn)
		return err == nil && absorbed.Outcome != ""
	})
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["answerer"].transport}); err != nil || len(owing) != 0 {
		t.Fatalf("the answering prompt with its retained exact operation is still owed: %v, %v", owing, err)
	}
	if absorbed, err := core.Get(ctx, "hosted", sent["absorbed"].turn); err != nil || absorbed.Outcome != agentTurnCompleted || absorbed.FinalMessageID != "kept-final:0" || !explicitZeroUsage(absorbed) {
		t.Fatalf("absorbed turn = %+v, %v; want the saved turn's kept-final:0 with explicit zeros", absorbed, err)
	}
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{sent["absorbed"].transport}); err != nil || !owing[sent["absorbed"].transport] {
		t.Fatalf("an agent member without its retained operation cleared: owing=%v, %v", owing, err)
	}
}

// Two people's prompts answered together, with no agent operation: the saved
// commit confirmed both, owing their member completion, and Nano stopped
// before its member step ran. The new session learns from the owed prompts it
// reads at attach that a later saved commit must still run the member step,
// and that commit finishes the absorbed prompt's own turn with the shared
// answer and explicit zero usage, read from the saved turn's own final row.
// Boundary: attach and the saved-commit settle pass on a real catalog of
// Claude's format.
func TestClaudeOwedPersonPromptFinishesAfterARestartOnALaterSavedCommit(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	native := h.fixture.native
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attach := func() *session {
		sess := h.attach("claude", nil)
		sess.agentExecutions, sess.completedTurns = newTestAgentExecutionStore(t, "run-1"), completed.coreStore
		return sess
	}
	turnOf := map[string]string{"person-starter": "claude:shim:300", "person-correction": "claude:shim:301"}
	for _, action := range []string{"person-starter", "person-correction"} {
		turn, text := turnOf[action], action+" prompt"
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderClaude, NativeThreadID: native, ActionID: action,
			ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		binding := turns.Binding{EventID: submissionEventID("hosted", action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	h.appendSaved(t, fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"person-starter prompt"}}
{"type":"attachment","uuid":"person-queued","sessionId":"claude-settle","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":"person-correction prompt"}],"source_uuid":%q}}
{"type":"assistant","uuid":"person-shared","sessionId":"claude-settle","message":{"id":"person-final","role":"assistant","content":[{"type":"text","text":"person answer"}],"stop_reason":"end_turn"}}
`, submissionEventID("hosted", "person-starter"), submissionEventID("hosted", "person-correction")))
	// The saved commit confirmed both, owing their member completion, when
	// Nano stopped before its member step ran.
	for _, action := range []string{"person-starter", "person-correction"} {
		if _, settled, err := h.prompts.Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", action, settled, err)
		}
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := h.server.savedPromptRows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", "person-correction")}})
		return err == nil && len(rows) == 1
	})

	// Restart: a new session with nothing waiting. Its attach pass reads the
	// owed prompts, and the answer is recorded with no live session to tell.
	after := attach()
	after.settleSavedPrompts(ctx)
	if !after.owedHint.Load() {
		t.Fatal("attach read owed prompts but left saved commits gated out")
	}
	input, output := int64(10), int64(5)
	if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: native, TurnID: turnOf["person-correction"],
		FinalMessageID: "person-final:0", Outcome: agentTurnCompleted, At: time.Now(), Source: "adapter", Usage: coreturns.Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
		t.Fatal(err)
	}
	waitForCatalogWithin(t, time.Minute, func() bool {
		after.settleWaitingPrompts(ctx) // what a later saved commit runs
		record, err := core.Get(ctx, "hosted", turnOf["person-starter"])
		return err == nil && record.Outcome != ""
	})
	record, err := core.Get(ctx, "hosted", turnOf["person-starter"])
	if err != nil || record.Outcome != agentTurnCompleted || record.FinalMessageID != "person-final:0" || !explicitZeroUsage(record) {
		t.Fatalf("absorbed person turn = %+v, %v; want the shared result with explicit zero usage", record, err)
	}
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{"person-starter", "person-correction"}); err != nil || len(owing) != 0 {
		t.Fatalf("person prompts still owed: %v, %v", owing, err)
	}
}

// The historical accepted-forever duplicate: every attach republished every
// delivered prompt. The loaded saved tail grows with the number of those
// prompts, not with the turns the thread gained without one, so an old prompt
// whose saved row sat before the tail came back as a new last row. Attach must settle what the saved transcript
// proves, publish only the rest, and keep that true across a restart.
// Boundary: attach on a real catalog of each provider's saved format.
func TestReattachSettlesSavedPromptsAndPublishesOnlyTheRest(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, provider)
			// Only Claude can prove an uncertain delivery: Codex and Grok record
			// no turn id without an answer, so that prompt stays visible.
			unanswered := turns.Uncertain
			if provider == "claude" {
				unanswered = turns.Confirmed
			}
			for restart := 0; restart < 2; restart++ {
				sess := h.attach(provider, nil)
				// What a pane opened now shows: the live text a new loop seeds
				// from the outbox. A settled prompt is never published again,
				// so no prompt is shown twice; unsettled prompts show once.
				sess.liveText = newLiveTextLoop(h.server, sess)
				liveTextOf(sess).reset()
				liveTextOf(sess).seedOutbox()
				actions, users := map[string]int{}, map[string]int{}
				for _, entry := range liveTextOf(sess).visible() {
					if entry.Kind == "prompt" {
						actions[entry.ActionID]++
						users[strings.SplitN(entry.Text, "\n\n", 2)[0]]++
					}
				}
				duplicated := false
				for _, count := range users {
					duplicated = duplicated || count > 1
				}
				if actions["old"] != 0 || actions["new"] != 0 || actions["running"] != 1 || actions["unproven"] != 1 ||
					users["running prompt"] != 1 || users["unproven prompt"] != 1 || duplicated ||
					unanswered == turns.Uncertain && (actions["unanswered"] != 1 || users["unanswered prompt"] != 1) ||
					unanswered == turns.Confirmed && actions["unanswered"] != 0 {
					t.Fatalf("attach %d published settled prompts or lost unsettled ones: actions=%v users=%v", restart, actions, users)
				}
				for name, want := range map[string]turns.DeliveryState{"old": turns.Confirmed, "new": turns.Confirmed, "running": turns.Accepted, "unproven": turns.Accepted, "unanswered": unanswered} {
					record, _, err := h.prompts.Get(ctx, name)
					if err != nil || record.Outbox.State != want || record.ProviderText != name+" prompt" {
						t.Fatalf("attach %d: %s state=%q text kept=%v err=%v, want %q", restart, name, record.Outbox.State, record.ProviderText != "", err, want)
					}
				}
			}
			if provider == "claude" {
				// Confirmed by the submission id derived from its action, which
				// its stored binding lacks: its answered turn decides it too, so
				// the person's prompt owes nothing more.
				sess := h.attach(provider, nil)
				sess.agentExecutions, sess.completedTurns = newTestAgentExecutionStore(t, "run-1"), (&completedTurnStore{db: h.queue.db}).coreStore
				waitForCatalogWithin(t, time.Minute, func() bool {
					if _, err := sess.settlePrompts(ctx); err != nil {
						return false
					}
					owing, err := h.prompts.Owing(ctx, "hosted", []string{"unanswered"})
					return err == nil && len(owing) == 0
				})
			}
		})
	}
}

// savedTurnRows is one ended turn as each provider saves it. A prompted turn
// carries the identity its delivery receipt names (receiptFor).
func savedTurnRows(provider, name string, prompted bool, at int) string {
	switch provider {
	case "codex":
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-%[1]s"}}
{"type":"event_msg","payload":{"type":"user_message","message":"%[1]s prompt"}}
{"type":"response_item","payload":{"type":"message","id":"%[1]s-answer","role":"assistant","content":[{"type":"output_text","text":"%[1]s answer"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-%[1]s"}}
`, name)
	case "claude":
		uuid := name + "-native-row"
		if prompted {
			uuid = submissionEventID("hosted", name)
		}
		return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"claude-settle","message":{"role":"user","content":"%[2]s prompt"}}
{"type":"assistant","uuid":"%[2]s-event","sessionId":"claude-settle","message":{"id":"%[2]s-message","role":"assistant","content":[{"type":"text","text":"%[2]s answer"}],"stop_reason":"end_turn"}}
`, uuid, name)
	default:
		return fmt.Sprintf(`{"timestamp":%[2]d,"method":"session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"%[1]s prompt"}},"_meta":{"eventId":"%[1]s-user-event"}}}
{"timestamp":%[3]d,"method":"session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%[1]s answer"}},"_meta":{"promptId":"prompt-%[1]s","eventId":"%[1]s-agent-event"}}}
{"timestamp":%[4]d,"method":"_x.ai/session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-%[1]s","stop_reason":"end_turn"}}}
`, name, at, at+1, at+2)
	}
}

// receiptFor is the provider's answer to delivering the prompt name.
func receiptFor(provider, name string) json.RawMessage {
	switch provider {
	case "claude":
		return json.RawMessage(`{"turn":{"id":"claude:shim:` + name + `"},"event":{"id":"` + submissionEventID("hosted", name) + `"}}`)
	case "grok":
		return json.RawMessage(`{"turn":{"id":"prompt-` + name + `"}}`)
	default:
		return json.RawMessage(`{"turn":{"id":"turn-` + name + `"}}`)
	}
}

// The live-settlement regression: settlement ran only at attach, reconcile
// and repair, so a prompt answered while its session stayed attached stayed
// accepted, and shown, until the next restart. With no restart, each prompt
// must settle by exact identity: one whose turn was open at attach, once the
// turn ends; one delivered live, once its turn is saved; and one whose whole
// turn the provider saved before its delivery receipt returned, when no
// later commit comes. A prompt the saved transcript cannot prove
// ("unproven") must cost later commits no saved read.
// Boundary: the catalog's transcript-changed callback, on a real catalog of
// each provider's saved format, and delivery through the send path.
func TestSavedCommitSettlesPromptsWithoutReattach(t *testing.T) {
	for _, test := range []struct {
		provider string
		ends     string // the provider ends the turn open at attach
	}{
		{provider: "codex", ends: `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-running"}}` + "\n"},
		{provider: "claude", ends: `{"type":"assistant","uuid":"running-end","sessionId":"claude-settle","message":{"id":"running-message","role":"assistant","content":[{"type":"text","text":"running answer"}],"stop_reason":"end_turn"}}` + "\n"},
		{provider: "grok", ends: `{"timestamp":1893456100,"method":"_x.ai/session/update","params":{"sessionId":"22222222-2222-4222-8222-222222222222","update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-running","stop_reason":"end_turn"}}}` + "\n"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			h := newSettlementHarness(t, test.provider)
			var running, finished atomic.Int32
			// commit appends saved rows and returns once the catalog's callback
			// for them has reached the session, and, if finish, has finished.
			commit := func(rows string, finish bool) {
				t.Helper()
				for running.Load() != 0 {
					time.Sleep(time.Millisecond)
				}
				before := finished.Load()
				h.appendSaved(t, rows)
				deadline := time.Now().Add(30 * time.Second)
				for finished.Load() == before && (finish || running.Load() == 0) || finish && running.Load() != 0 {
					if time.Now().After(deadline) {
						t.Fatal("the saved commit did not reach the session")
					}
					time.Sleep(time.Millisecond)
				}
			}
			var answer func(name string) json.RawMessage
			sess := h.attach(test.provider, func(_ context.Context, text string) (json.RawMessage, error) {
				return answer(strings.TrimSuffix(text, " prompt")), nil
			})
			var reads atomic.Int32
			savedPrompts := sess.savedPrompts
			sess.savedPrompts = func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
				reads.Add(1)
				return savedPrompts(ctx, keys)
			}
			h.server.sessions = map[string]*session{sess.id: sess}
			h.service.setTranscriptChanged(func(ctx context.Context, change transcriptChange) {
				running.Add(1)
				defer running.Add(-1)
				h.server.confirmDurableThread(ctx, change)
				finished.Add(1)
			})
			settles := func(action string) {
				t.Helper()
				deadline := time.Now().Add(30 * time.Second)
				for h.state(t, action) != turns.Confirmed {
					if time.Now().After(deadline) {
						t.Fatalf("%q stayed %s", action, h.state(t, action))
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			deliver := func(name string) {
				t.Helper()
				if rec := postTurn(t, sess, context.Background(), name, name+" prompt"); rec.Code != http.StatusOK {
					t.Fatalf("%s delivery = %d %s", name, rec.Code, rec.Body.String())
				}
			}

			commit(test.ends, true)
			settles("running")

			answer = func(name string) json.RawMessage { return receiptFor(test.provider, name) }
			deliver("live")
			commit(savedTurnRows(test.provider, "live", true, 1893456110), true)
			settles("live")
			var turn struct {
				Turn struct{ ID string } `json:"turn"`
			}
			if err := json.Unmarshal(receiptFor(test.provider, "live"), &turn); err != nil {
				t.Fatal(err)
			}
			// The provider ends the live turn, so the next prompt is sent now.
			waitTurnSendDone(t, sess, "live")
			observeTestTurnCompletion(t, sess, turn.Turn.ID)
			sess.setTurnActive(true, turn.Turn.ID, time.Now().UnixMilli(), true)
			sess.setTurnActive(false, turn.Turn.ID, 0, true)

			// The provider saves the whole turn, and the catalog commits it,
			// before the receipt returns. Codex and Grok callbacks wait for the
			// delivery; Claude's finishes first, and no later commit comes.
			answer = func(name string) json.RawMessage {
				commit(savedTurnRows(test.provider, name, true, 1893456120), false)
				return receiptFor(test.provider, name)
			}
			deliver("early")
			settles("early")

			if state := h.state(t, "unproven"); state != turns.Accepted {
				t.Fatalf("unproven prompt state = %q, want accepted", state)
			}
			before := reads.Load()
			commit(savedTurnRows(test.provider, "later", false, 1893456130), true)
			if test.provider == "claude" {
				// Each settled Claude prompt owes its member completion, which
				// these fixture turns never finish, so commits keep reading for
				// it. TestOverlappingSettlementPassesLeaveNothingWaiting proves
				// the reads stop once owed work drains.
				if owing, err := h.prompts.Owing(context.Background(), "hosted", []string{"running", "live", "early"}); err != nil || len(owing) != 3 {
					t.Fatalf("owed after settlement = %v, %v; want all three", owing, err)
				}
			} else if got := reads.Load(); got != before {
				t.Fatalf("a commit with no prompt left to settle read saved prompts %d times", got-before)
			}
		})
	}
}

// A person's Codex prompt with no saved row is resent only on proof that its
// turn dropped it: this attachment saw the turn from its start end
// interrupted with no echo of the prompt's client id, and the saved
// transcript holds no row of it. Codex records the prompt a turn starts with
// only once the turn begins work, after any context compaction, and holds a
// steered one until the turn's next step; an interruption before then drops
// it. Anything less leaves the prompt as it is, for the person to resend or
// discard, and identity decides which prompt was dropped, never text. A turn
// the operator stopped is proof too, but Stop means stop: the dropped prompt
// is held for the person, never sent again, also when the Stop lands while a
// pass is already reading saved rows. Boundary: the live Codex wire and the
// Stop route, through the settlement pass of a real catalog of the saved
// transcript, to the provider sender.
func TestCodexPromptIsResentOnlyOnProofItsTurnDroppedIt(t *testing.T) {
	type frame string
	const (
		started     frame = "started"
		echoed      frame = "echo of the prompt"
		neighbour   frame = "echo of an equal-text prompt"
		interrupted frame = "interrupted"
		completed   frame = "completed"
		reattached  frame = "re-attach"
		stop        frame = "Stop"
		reading     frame = "a pass starts reading saved rows"
		read        frame = "that pass finishes"
	)
	for _, test := range []struct {
		name       string
		watched    bool    // this attachment delivered the prompt
		frames     []frame // what this attachment saw of the prompt's turn
		savedRow   bool    // the saved transcript holds the prompt's row
		savedEnded bool    // ... and the turn's end
		resent     bool
		wantState  turns.DeliveryState
	}{
		{name: "delivered before a restart", frames: []frame{started, interrupted}, wantState: turns.Accepted},
		{name: "turn not seen from its start", watched: true, frames: []frame{interrupted}, wantState: turns.Accepted},
		{name: "turn still open", watched: true, frames: []frame{started}, wantState: turns.Accepted},
		{name: "turn completed", watched: true, frames: []frame{started, completed}, wantState: turns.Accepted},
		{name: "echoed, row not saved yet", watched: true, frames: []frame{started, echoed, interrupted}, wantState: turns.Accepted},
		{name: "evidence from the previous attachment: reset still queued, then the link drops again", watched: true, frames: []frame{started, interrupted, reattached}, wantState: turns.Accepted},
		{name: "saved in an interrupted turn the wire did not echo", watched: true, frames: []frame{started, interrupted}, savedRow: true, savedEnded: true, wantState: turns.Confirmed},
		{name: "saved in a turn still open", watched: true, frames: []frame{started}, savedRow: true, wantState: turns.Accepted},
		{name: "dropped beside an echoed equal-text prompt", watched: true, frames: []frame{started, neighbour, interrupted}, resent: true, wantState: turns.Expired},
		{name: "dropped by a Stop", watched: true, frames: []frame{started, stop, interrupted}, wantState: turns.Uncertain},
		{name: "dropped by a Stop pressed while a pass reads", watched: true, frames: []frame{started, reading, stop, interrupted, read}, wantState: turns.Uncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "codex")
			native, turn, text := h.fixture.native, "turn-case", "same prompt"
			event := submissionEventID("hosted", "prompt")
			if _, _, err := h.prompts.Submit(ctx, turns.Submission{HostedThreadID: "hosted", Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "prompt",
				ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text), Origin: turns.HumanOrigin}); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: "case", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
				t.Fatalf("claim = %v, %v", claimed, err)
			}
			binding := turns.Binding{EventID: event, Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}
			if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: "prompt", ClaimToken: "case", AttachmentEpoch: 1, Binding: binding}); err != nil {
				t.Fatal(err)
			}
			sent := make(chan string, 4)
			sess := h.attach("codex", func(_ context.Context, text string) (json.RawMessage, error) {
				sent <- text
				return json.RawMessage(`{"turn":{"id":"turn-next"}}`), nil
			})
			if test.watched {
				sess.awaitSavedPrompt("prompt")
			}
			// Saved rows are indexed first: a row the saved transcript holds
			// vetoes a resend, whatever the wire showed.
			if test.savedRow {
				rows := fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%[1]q}}\n"+
					"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"case-message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%[2]q}],\"internal_chat_message_metadata_passthrough\":{\"turn_id\":%[1]q}}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"turn_id\":%[1]q,\"item\":{\"type\":\"UserMessage\",\"id\":\"case-item\",\"client_id\":%[3]q,\"content\":[{\"type\":\"text\",\"text\":%[2]q}]}}}\n", turn, text, event)
				if test.savedEnded {
					rows += fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"turn_aborted\",\"turn_id\":%q,\"reason\":\"interrupted\"}}\n", turn)
				}
				h.appendSaved(t, rows)
				waitForCatalogWithin(t, time.Minute, func() bool {
					rows, err := sess.savedPrompts(ctx, history.SavedUserKeys{NativeTurnIDs: []string{turn}})
					return err == nil && len(rows) > 0 && (!test.savedEnded || rows[0].TurnStatus == history.TurnInterrupted)
				})
			}
			echo := func(client string) {
				sess.recordWireLive(protocol.Recv, []byte(`{"method":"item/completed","params":{"threadId":"`+native+`","turnId":"`+turn+`","item":{"id":"`+client+`-item","type":"userMessage","clientId":"`+client+`","content":[{"type":"text","text":"`+text+`"}]}}}`), true)
			}
			var release, passed chan struct{}
			var play func(frame)
			play = func(f frame) {
				switch f {
				case stop:
					// The person stops the turn the pane shows working.
					waitForCatalogWithin(t, 10*time.Second, func() bool { _, active := sess.activeTurn(); return active == turn })
					rec := httptest.NewRecorder()
					sess.handleInterrupt(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interrupt", nil))
					if rec.Code != http.StatusOK || h.interrupts.Load() != 1 {
						t.Fatalf("Stop = %d %s, %d interrupts", rec.Code, rec.Body.String(), h.interrupts.Load())
					}
				case reading:
					entered, read := make(chan struct{}), sess.savedPrompts
					release, passed = make(chan struct{}), make(chan struct{})
					var hold sync.Once
					sess.savedPrompts = func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
						hold.Do(func() { close(entered); <-release })
						return read(ctx, keys)
					}
					go func() { sess.settleWaitingPrompts(ctx); close(passed) }()
					<-entered
				case read:
					close(release)
					<-passed
				case started:
					sess.recordWireLive(protocol.Recv, []byte(`{"method":"turn/started","params":{"threadId":"`+native+`","turn":{"id":"`+turn+`","status":"inProgress","items":[]}}}`), true)
				case echoed:
					echo(event)
				case neighbour:
					echo(submissionEventID("hosted", "neighbour"))
				case interrupted, completed:
					status := map[frame]string{interrupted: "interrupted", completed: "completed"}[f]
					sess.recordWireLive(protocol.Recv, []byte(`{"method":"turn/completed","params":{"threadId":"`+native+`","turn":{"id":"`+turn+`","status":"`+status+`"}}}`), true)
				case reattached:
					// The handshake's reset waits behind work already in the
					// session's mailbox, and the link drops again before it
					// runs. Once it runs, frames the dead connection still
					// delivers rebuild the evidence: none of it is proof.
					if err := sess.ensureCoreSession(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(sess.coreSession().Close)
					busy := make(chan struct{})
					sess.coreSession().ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error { <-busy; return nil }})
					sess.forgetCodexTurnInputs() // what each Codex handshake does first
					sess.settleWaitingPrompts(ctx)
					sess.observeConnection(lifecycle.EventLinkLost)
					close(busy)
					for _, f := range []frame{started, interrupted} {
						play(f)
					}
					if err := sess.coreSession().Observe(ctx, coresession.AdapterEvent{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, f := range test.frames {
				play(f)
			}
			sess.settleWaitingPrompts(ctx) // the pass a saved commit runs

			select {
			case got := <-sent:
				if !test.resent || got != text {
					t.Fatalf("sent %q again; want resent=%v", got, test.resent)
				}
			case <-time.After(time.Second):
				if test.resent {
					t.Fatal("the dropped prompt was never sent again")
				}
			}
			select {
			case got := <-sent:
				t.Fatalf("sent a second time: %q", got)
			case <-time.After(200 * time.Millisecond):
			}
			if state := h.state(t, "prompt"); state != test.wantState {
				t.Fatalf("prompt = %s; want %s", state, test.wantState)
			}
			if test.wantState == turns.Uncertain {
				if view, code := readPrompt(t, sess, "prompt"); code != http.StatusOK || view.State != string(turns.Uncertain) || view.Label != "Stopped before Codex took it" {
					t.Fatalf("held read = %d %+v; want held for the person", code, view)
				}
				// Codex saves it after all: the hold ends like any prompt's wait.
				if _, settled, err := h.prompts.Settle(ctx, "prompt"); err != nil || !settled {
					t.Fatalf("settle = %v, %v", settled, err)
				}
				if view, code := readPrompt(t, sess, "prompt"); code != http.StatusOK || view.State != string(turns.Confirmed) || view.AllowedActions != nil {
					t.Fatalf("read once saved = %d %+v; want confirmed", code, view)
				}
			}
		})
	}
}

// Two settlement passes can overlap, such as a delivery's catch-up and a
// catalog commit. If the one that finishes last restores the waiting mark of
// a prompt the other settled, every later idle commit reads saved prompts
// again. Boundary: the catalog callback's session step on a real catalog,
// with the session's saved reads held so that, unserialized, both passes read
// the accepted prompt before either settles it.
func TestOverlappingSettlementPassesLeaveNothingWaiting(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "claude")
	// Nothing may be left to do once the overlap settles, owed member
	// completions included: a settled Claude prompt owes its own completion,
	// and while it does, commits keep reading. The fixture's identity-less
	// uncertain prompt could never finish, so it is expired up front, and each
	// settled prompt's own turn carries its result.
	if _, expired, err := h.prompts.Expire(ctx, "unanswered", time.Now()); err != nil || !expired {
		t.Fatalf("expire unanswered = %v, %v", expired, err)
	}
	completed := &completedTurnStore{db: h.queue.db}
	core, err := completed.coreStore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "new", "running"} {
		turn := h.fixture.turns[name]
		if _, _, err := core.Started(ctx, coreturns.Start{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: h.fixture.native, TurnID: turn, At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.Finished(ctx, coreturns.Finish{HostedThreadID: "hosted", Provider: "claude", NativeThreadID: h.fixture.native, TurnID: turn, Outcome: "failed", At: time.Now(), Source: "adapter"}); err != nil {
			t.Fatal(err)
		}
	}
	sess := h.attach("claude", nil) // "running" waits: its saved turn is open
	sess.agentExecutions, sess.completedTurns = newTestAgentExecutionStore(t, "run-1"), completed.coreStore
	h.appendSaved(t, `{"type":"assistant","uuid":"running-end","sessionId":"claude-settle","message":{"id":"running-message","role":"assistant","content":[{"type":"text","text":"running answer"}],"stop_reason":"end_turn"}}`+"\n")
	saved := sess.savedPrompts
	running := history.SavedUserKeys{NativeEventIDs: []string{submissionEventID("hosted", "running")}}
	waitForCatalogWithin(t, time.Minute, func() bool {
		rows, err := saved(context.Background(), running)
		return err == nil && len(rows) > 0 && durableTerminalStatus(string(rows[0].TurnStatus))
	})
	// Only each competing pass's first settlement read is held, found by the
	// pass's own context; the member step's reads run under a context of
	// their own and are never held, however many a pass makes.
	type passKey struct{}
	var reads atomic.Int32
	var held [3]atomic.Bool
	entered := make(chan struct{}, 2)
	gates := [3]chan struct{}{nil, make(chan struct{}), make(chan struct{})}
	first, second := gates[1], gates[2]
	sess.savedPrompts = func(ctx context.Context, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
		reads.Add(1)
		rows, err := saved(ctx, keys)
		if n, ok := ctx.Value(passKey{}).(int); ok && held[n].CompareAndSwap(false, true) {
			entered <- struct{}{}
			<-gates[n]
		}
		return rows, err
	}
	passes := 0
	pass := func() chan struct{} {
		passes++
		passCtx := context.WithValue(context.Background(), passKey{}, passes)
		done := make(chan struct{})
		go func() { h.server.confirmDurableItems(passCtx, sess); close(done) }()
		return done
	}
	done1 := pass()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first pass did not read saved prompts")
	}
	done2 := pass()
	select { // unserialized, the second pass reads too
	case <-entered:
	case <-time.After(200 * time.Millisecond):
	}
	close(first)
	<-done1
	close(second)
	<-done2
	if state := h.state(t, "running"); state != turns.Confirmed {
		t.Fatalf("running = %s, want confirmed", state)
	}
	// The regression this case owns: the pass that finishes last must not
	// restore the waiting mark of the prompt the other settled. Checked before
	// any later commit, which would otherwise drop a restored mark again.
	sess.stateMu.Lock()
	waiting := len(sess.settleWaiting)
	sess.stateMu.Unlock()
	if waiting != 0 {
		t.Fatalf("the overlapping passes left %d prompts waiting for a saved commit", waiting)
	}
	// The pass that finished each owed completion still saw it; one more
	// commit reads that none is left. After that, nothing waits.
	h.server.confirmDurableItems(ctx, sess)
	if owing, err := h.prompts.Owing(ctx, "hosted", []string{"old", "new", "running"}); err != nil || len(owing) != 0 {
		t.Fatalf("owed after the overlap = %v, %v; want every completion finished", owing, err)
	}
	before := reads.Load()
	h.server.confirmDurableItems(context.Background(), sess)
	if got := reads.Load(); got != before {
		t.Fatalf("an idle commit after the overlap read saved prompts %d times", got-before)
	}
}

// Steering puts several prompts in one Codex turn, so the turn no longer
// identifies a prompt: each must settle only when the saved transcript holds
// its own client id. A prompt Codex acknowledged but never saved (a hook
// rejected it) stays accepted rather than settling with its neighbours.
// Boundary: settlement on a real catalog of Codex's saved format, where the
// client id is on the completed copy of each user row.
func TestCodexPromptsSharingATurnSettleByTheirOwnEvents(t *testing.T) {
	ctx := context.Background()
	h := newSettlementHarness(t, "codex")
	native, turn := h.fixture.native, "turn-shared"
	for _, name := range []string{"starter", "steered", "rejected"} {
		prompt := name + " prompt"
		if _, _, err := h.prompts.Submit(ctx, turns.Submission{
			HostedThreadID: "hosted", Provider: model.ProviderCodex, NativeThreadID: native, ActionID: name,
			ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: turns.AgentOrigin,
		}); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + name
		if _, claimed, err := h.prompts.Claim(ctx, turns.ClaimRequest{HostedThreadID: "hosted", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", name, claimed, err)
		}
		binding := turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: submissionEventID("hosted", name)}
		if _, err := h.prompts.Accepted(ctx, turns.Acceptance{ActionID: name, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatalf("accept %s into the shared turn: %v", name, err)
		}
	}
	var rows strings.Builder
	fmt.Fprintf(&rows, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%q}}\n", turn)
	for _, name := range []string{"starter", "steered"} {
		fmt.Fprintf(&rows, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":%q,\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%q}],\"internal_chat_message_metadata_passthrough\":{\"turn_id\":%q}}}\n", name+"-message", name+" prompt", turn)
		fmt.Fprintf(&rows, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"turn_id\":%q,\"item\":{\"type\":\"UserMessage\",\"id\":%q,\"client_id\":%q,\"content\":[{\"type\":\"text\",\"text\":%q}]}}}\n", turn, name+"-item", submissionEventID("hosted", name), name+" prompt")
	}
	fmt.Fprintf(&rows, "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"shared-answer\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"shared answer\"}]}}\n")
	fmt.Fprintf(&rows, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%q}}\n", turn)
	h.appendSaved(t, rows.String())

	sess := h.attach("codex", nil)
	var results map[string]transcript.Settlement
	waitForCatalogWithin(t, time.Minute, func() bool {
		var err error
		results, err = settleThreadPrompts(ctx, h.prompts, sess.savedPrompts, "hosted", "codex", native)
		return err == nil && h.state(t, "steered") == turns.Confirmed
	})
	for name, want := range map[string]turns.DeliveryState{"starter": turns.Confirmed, "steered": turns.Confirmed, "rejected": turns.Accepted} {
		if got := h.state(t, name); got != want {
			t.Fatalf("%s state=%s; want %s (results=%v)", name, got, want, results)
		}
	}
}

// Claude can save a prompt in a form other than the text Nano sent; the row
// still carries the send's stamped id and must settle it. Before, each case
// left the prompt pending for good although Claude had run it:
//   - Prompts that queued together merge into one row: its uuid is one send's
//     id, and mergedSendUuids names the others' (Claude 2.1.300).
//   - A prompt that is a slash command is saved as Claude's expansion of it
//     (<command-message>, <command-name>, <command-args>), which read as
//     injected context, not as the prompt (Claude 2.1.310). It is read as the
//     command sent, "/X A".
//
// Fixtures are real rows' shapes, scrubbed, with ids stamped for this
// thread. Boundary: settlement on a real catalog of Claude's saved format.
func TestClaudePromptsSettleWithTheirSavedRow(t *testing.T) {
	for _, test := range []struct {
		name, fixture string
		actions       []string
		sent          string // the prompt's text as sent, when the row shows it
	}{
		{name: "merged prompts", fixture: "claude-merged-prompts.jsonl", actions: []string{"merged-first", "merged-second", "merged-third"}},
		{name: "a slash command", fixture: "claude-sent-command.jsonl", actions: []string{"sent-command"}, sent: "/prepare-repo /tmp/project\n\nCheck it is ready."},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "claude")
			native := h.fixture.native
			for i, action := range test.actions {
				prompt := action + " prompt"
				if test.sent != "" {
					prompt = test.sent
				}
				seedAcceptedTransactionalTurn(t, h.prompts, "hosted", model.ProviderClaude, native, action, fmt.Sprintf("claude:shim:%d", 300+i), prompt)
			}
			var rows strings.Builder
			for _, line := range fixtureLines(t, test.fixture) {
				rows.WriteString(strings.ReplaceAll(string(line), liveTextFixtureSession, native) + "\n")
			}
			for _, action := range test.actions {
				if !strings.Contains(rows.String(), submissionEventID("hosted", action)) {
					t.Fatalf("fixture lacks %s's stamped id", action)
				}
			}
			h.appendSaved(t, rows.String())

			sess := h.attach("claude", nil)
			waitForCatalogWithin(t, time.Minute, func() bool {
				_, err := sess.settlePrompts(ctx)
				return err == nil && h.state(t, test.actions[0]) == turns.Confirmed
			})
			for _, action := range test.actions {
				if got := h.state(t, action); got != turns.Confirmed {
					t.Fatalf("%s state=%s; want confirmed by its saved row", action, got)
				}
			}
			if test.sent == "" {
				return
			}
			threadID, err := h.service.ResolveThreadReference(ctx, native)
			if err != nil {
				t.Fatal(err)
			}
			page, err := h.service.LoadTranscript(ctx, threadID, transcriptListQuery{Limit: 100, View: defaultThreadTranscriptView, HiddenSpecified: true})
			if err != nil {
				t.Fatal(err)
			}
			shown := 0
			for _, item := range page.Items {
				if item.Type == "user_message" && item.Text == test.sent {
					shown++
				}
			}
			if shown != 1 {
				t.Fatalf("the transcript shows the prompt %d times as sent; want once, as %q", shown, test.sent)
			}
		})
	}
}

// Codex can take a prompt after Nano stopped waiting for its turn/start reply.
// The prompt is then uncertain, with no binding, and its saved row alone could
// never settle it, so the pane would show it pending while the agent answers.
// Codex's echo of the prompt's own client id binds it to the turn that took
// it: before or after Nano records the uncertain outcome, in a turn this
// attachment did not see start, after a reconnect, or after the turn was
// saved and ended. A prompt that waited in the queue first is not parked. The
// saved row then confirms it, and nothing is sent again. Boundary: the HTTP
// send and the live Codex wire, settled against a real catalog.
func TestCodexPromptTakenPastTheDeadlineIsBoundByItsEcho(t *testing.T) {
	for _, test := range []struct {
		name        string
		queuedFirst bool // sent while a turn worked, delivered when it ended
		echoFirst   bool // Codex echoes it, in a turn already running, before Nano records the outcome
		reconnect   bool // the link is re-established before the echo
		savedFirst  bool // the turn is saved and ended, and its commit settled, before the echo
	}{
		{name: "echoed after the deadline"},
		{name: "echoed in a running turn before the outcome", echoFirst: true},
		{name: "queued first, echoed after the deadline", queuedFirst: true},
		{name: "echoed after a reconnect", reconnect: true},
		{name: "echoed after its turn was saved and ended", savedFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSettlementHarness(t, "codex")
			// The fixture's other unsettled prompts would keep every saved
			// commit's pass running; only this prompt may.
			for _, action := range []string{"unanswered", "old", "new", "running", "unproven"} {
				if _, expired, err := h.prompts.Expire(ctx, action, time.Now()); err != nil || !expired {
					t.Fatalf("expire %s = %v, %v", action, expired, err)
				}
			}
			native, turn, text := h.fixture.native, "turn-late", "late prompt"
			event := submissionEventID("hosted", "late")
			started := `{"method":"turn/started","params":{"threadId":"` + native + `","turn":{"id":"` + turn + `","status":"inProgress","items":[]}}}`
			echo := `{"method":"item/started","params":{"threadId":"` + native + `","turnId":"` + turn + `","item":{"id":"late-item","type":"userMessage","clientId":"` + event + `","content":[{"type":"text","text":"` + text + `"}]}}}`
			var sess *session
			var sent atomic.Int32
			sess = h.attach("codex", func(context.Context, string) (json.RawMessage, error) {
				sent.Add(1)
				if test.echoFirst {
					// Frames that arrive during the call wait in the session's
					// mailbox and may be read before Nano records the outcome;
					// this reads the echo first, deterministically.
					sess.observeCodexTurnInput([]byte(echo))
				}
				return nil, context.DeadlineExceeded // Nano stopped waiting for the reply
			})
			if test.queuedFirst {
				sess.lifecycleState = testLifecycleSnapshot(sess.connectionStatusLocked(), true)
			}
			if rec := postTurn(t, sess, ctx, "late", text); rec.Code != http.StatusAccepted {
				t.Fatalf("send = %d %s; want 202", rec.Code, rec.Body.String())
			}
			if test.queuedFirst {
				sess.setTurnActive(false, "", 0, true)
			}
			waitForCatalogWithin(t, 5*time.Second, func() bool {
				sess.sendMu.Lock()
				op := sess.sendOps["late"]
				sess.sendMu.Unlock()
				if op == nil {
					return false
				}
				select {
				case <-op.done:
					return true
				default:
					return false
				}
			})
			saved := func() {
				h.appendSaved(t, fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":%[1]q}}\n"+
					"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"id\":\"late-message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":%[2]q}],\"internal_chat_message_metadata_passthrough\":{\"turn_id\":%[1]q}}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"item_completed\",\"turn_id\":%[1]q,\"item\":{\"type\":\"UserMessage\",\"id\":\"late-item\",\"client_id\":%[3]q,\"content\":[{\"type\":\"text\",\"text\":%[2]q}]}}}\n"+
					"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":%[1]q}}\n", turn, text, event))
			}
			if test.savedFirst {
				saved()
				waitForCatalogWithin(t, time.Minute, func() bool {
					rows, err := sess.savedPrompts(ctx, history.SavedUserKeys{NativeTurnIDs: []string{turn}})
					return err == nil && len(rows) > 0 && rows[0].TurnStatus == history.TurnCompleted
				})
				sess.settleWaitingPrompts(ctx) // the commit's pass, while the prompt is still unbound
			}
			if test.reconnect {
				sess.forgetCodexTurnInputs() // what each Codex handshake does first
			}
			if !test.echoFirst {
				for _, frame := range []string{started, echo} {
					sess.recordWireLive(protocol.Recv, []byte(frame), true)
				}
			}
			if test.savedFirst {
				// No later saved commit comes: the bind itself must settle it.
				waitForCatalogWithin(t, 5*time.Second, func() bool { return h.state(t, "late") == turns.Confirmed })
			} else {
				waitForCatalogWithin(t, 5*time.Second, func() bool { return h.state(t, "late") == turns.Accepted })
				saved()
				waitForCatalogWithin(t, time.Minute, func() bool {
					sess.settleWaitingPrompts(ctx) // the pass a saved commit runs
					return h.state(t, "late") == turns.Confirmed
				})
			}
			record, _, err := h.prompts.Get(ctx, "late")
			if err != nil || record.ProviderTurnID != turn || record.Outbox.Binding == nil || record.Outbox.Binding.EventID != event {
				t.Fatalf("late prompt = %+v, %v; want it bound to %s by its own event", record.Outbox, err, turn)
			}
			if got := sent.Load(); got != 1 {
				t.Fatalf("sent %d times; want once", got)
			}
		})
	}
}

// A delivery's outcome and its prompt's echo arrive on different goroutines,
// the send and the wire reader. Each must read the shared wait only under the
// session's lock; under -race, an unlocked read fails here. Boundary: the two
// entry points the send and the wire reader call.
func TestCodexEchoAndDeliveryOutcomeMayArriveTogether(t *testing.T) {
	sess := newSettlementHarness(t, "codex").attach("codex", nil)
	for i := range 100 {
		action := fmt.Sprintf("race-%d", i)
		sess.expectCodexEcho(codexEchoWait{actionID: action, claimToken: "claim", claimEpoch: 1})
		echo := `{"method":"item/started","params":{"turnId":"turn-race","item":{"type":"userMessage","clientId":"` + submissionEventID("hosted", action) + `"}}}`
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sess.deliveredCodexEcho(action, true) }()
		go func() { defer wg.Done(); sess.observeCodexTurnInput([]byte(echo)) }()
		wg.Wait()
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
	sess.projection = newLiveProjection("claude", "hosted-thread")
	sess.projection.SetNativeThreadID("native-thread")
	if err := sess.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.coreSession().Close)
	sess.setTurnActive(true, "claude:hosted-thread:1", 0, false)

	if rec := postTurn(t, sess, ctx, "correction", "Use the other file."); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	waitTurnSendDone(t, sess, "correction")
	record, _, err := sess.turns.Get(ctx, "correction")
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
			fake.setSteerError(test.steerErr)
			fake.setDropSteer(test.dropped)
			sess.codex = fake.daemon()
			if err := sess.ensureCoreSession(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.coreSession().Close)
			if test.queued {
				text := "Use the other file."
				if _, _, err := sess.turns.Submit(ctx, turns.Submission{HostedThreadID: sess.hostedThreadID, Provider: model.ProviderCodex, NativeThreadID: sess.threadID,
					ActionID: "correction", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
					t.Fatal(err)
				}
			}
			sess.setTurnActive(true, "turn-running", 0, false)

			if !test.queued {
				rec := postTurn(t, sess, ctx, "correction", "Use the other file.")
				if test.state == turns.Accepted && rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
				}
			}
			waitTurnSendDone(t, sess, "correction")
			sess.kickQueuedTurnDrain() // a settled attempt is never sent again
			time.Sleep(50 * time.Millisecond)
			steers := fake.steerParams()
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
				params.ClientUserMessageID != submissionEventID(sess.hostedThreadID, "correction") ||
				len(params.Input) != 1 || !strings.HasPrefix(params.Input[0].Text, "Use the other file.") {
				t.Fatalf("turn/steer params = %s", steers[0])
			}
			record, attempt, err := sess.turns.Get(ctx, "correction")
			if err != nil || record.Outbox.State != test.state || test.reason != "" && attempt.Reason != test.reason {
				t.Fatalf("record state=%s reason=%q err=%v; want %s %q", record.Outbox.State, attempt.Reason, err, test.state, test.reason)
			}
			if test.state == turns.Accepted && (record.Outbox.Binding == nil || record.Outbox.Binding.EventID != params.ClientUserMessageID || record.ProviderTurnID != "turn-running") {
				t.Fatalf("binding = %+v turn=%q; want the running turn and the client id", record.Outbox.Binding, record.ProviderTurnID)
			}
			fake.mu.Lock()
			started := fake.turnStarts
			fake.mu.Unlock()
			if started != 0 {
				t.Fatalf("turn/start calls = %d, want 0 while the turn works", started)
			}
		})
	}
}

// Prompts steered into one Codex turn share its reply. When that turn ends
// while Nano is down, no live item reached their operations, so each must
// take the turn's own final answer from what resume resolves, never an empty
// or stale reply, once its prompt is proven saved. That includes a member
// bound after resume, which never observed the answer itself.
// Boundary: resume of a hosted Codex thread through the completed-turn record
// to the agent operations.
func TestSharedTurnEndedWhileDownGivesEveryProvenOperationItsFinalAnswer(t *testing.T) {
	ctx := context.Background()
	s, _, rec, _, _ := subscribedCodexServer(t, `[{"id":"turn-shared","status":"inProgress","startedAt":1787600000}]`)
	var ops []agentTurnRecord
	for _, action := range []string{"starter", "correction"} {
		op, _, err := s.agentExecutions.beginTurn(rec.HostedThreadID, action, "hash-"+action)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.agentExecutions.MarkTurnDelivery(op.ID, "turn-shared", agentTurnRunning, nil); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	s.reattachOnBoot()
	var sess *session
	waitForClaude(t, "the reattached session", func() bool {
		sess = s.sessionForHostedThread(rec.HostedThreadID)
		return sess != nil && sess.summary().Status == "ready" && sess.turns != nil
	})
	native, turn := rec.NativeThreadID, "turn-shared"
	proveSaved := func(op agentTurnRecord) {
		action, text := op.TransportActionID, "prompt "+op.ClientActionID
		if _, _, err := sess.turns.Submit(ctx, turns.Submission{HostedThreadID: rec.HostedThreadID, Provider: model.ProviderCodex, NativeThreadID: native,
			ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := sess.turns.Claim(ctx, turns.ClaimRequest{HostedThreadID: rec.HostedThreadID, Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		binding := turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: submissionEventID(rec.HostedThreadID, action)}
		if _, err := sess.turns.Accepted(ctx, turns.Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding}); err != nil {
			t.Fatal(err)
		}
		if _, settled, err := sess.turns.Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s: %v %v", action, settled, err)
		}
	}
	for _, op := range ops {
		proveSaved(op)
	}

	resumed := `{"thread":{"id":"` + native + `","turns":[{"id":"turn-shared","status":"completed","completedAt":1787600100,"items":[{"id":"final-shared","type":"agentMessage","phase":"final_answer","text":"the shared answer"}]}]}}`
	sess.observeCodexResume(decodedCodexResume([]byte(resumed)))
	waitForClaude(t, "both operations finished", func() bool {
		for _, op := range ops {
			if got, _ := s.agentExecutions.turn(op.ID); !threadvalues.TurnTerminal(got.Status) {
				return false
			}
		}
		return true
	})
	for _, op := range ops {
		got, _ := s.agentExecutions.turn(op.ID)
		if got.Status != agentTurnCompleted || got.FinalItemID != "final-shared" || got.FinalResponse != "the shared answer" {
			t.Fatalf("operation %s = status %s final %q %q; want the turn's final answer", op.ClientActionID, got.Status, got.FinalItemID, got.FinalResponse)
		}
	}
	completed, err := sess.completedTurns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := completed.Get(ctx, rec.HostedThreadID, turn); err != nil || record.FinalMessageID != "final-shared" {
		t.Fatalf("completed turn final = %q err=%v; want the final answer resume decoded", record.FinalMessageID, err)
	}

	late, _, err := s.agentExecutions.beginTurn(rec.HostedThreadID, "late", "hash-late")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.MarkTurnDelivery(late.ID, turn, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	proveSaved(late)
	sess.finishJoinedAgentTurns(ctx, turn)
	if got, _ := s.agentExecutions.turn(late.ID); got.Status != agentTurnCompleted || got.FinalItemID != "final-shared" || got.FinalResponse != "the shared answer" {
		t.Fatalf("late operation = status %s final %q %q; want the turn's final answer", got.Status, got.FinalItemID, got.FinalResponse)
	}
}

// An agent's correction steered into a turn a person started is the turn's
// only operation, but the turn ending does not prove Codex consumed it: a
// hook can reject it after Codex acknowledged it, and then its own row never
// appears. Its operation stays open rather than finishing with that turn, and
// finishes with the turn's final answer once its own row is proven saved.
// Boundary: the live Codex wire through the completed-turn record to the
// agent operation.
func TestCorrectionInAPersonsTurnDoesNotFinishWithoutItsOwnRow(t *testing.T) {
	ctx := context.Background()
	s, daemon, rec, _, _ := subscribedCodexServer(t, `[{"id":"turn-human","status":"inProgress","startedAt":1787600000}]`)
	op, _, err := s.agentExecutions.beginTurn(rec.HostedThreadID, "correction", "hash-correction")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.MarkTurnDelivery(op.ID, "turn-human", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	s.reattachOnBoot()
	var sess *session
	waitForClaude(t, "the reattached turn to be active", func() bool {
		sess = s.sessionForHostedThread(rec.HostedThreadID)
		if sess == nil || sess.turns == nil {
			return false
		}
		sess.stateMu.Lock()
		defer sess.stateMu.Unlock()
		return sess.workActiveLocked()
	})
	native, turn, action := rec.NativeThreadID, "turn-human", op.TransportActionID
	text := "prompt correction"
	if _, _, err := sess.turns.Submit(ctx, turns.Submission{HostedThreadID: rec.HostedThreadID, Provider: model.ProviderCodex, NativeThreadID: native,
		ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := sess.turns.Claim(ctx, turns.ClaimRequest{HostedThreadID: rec.HostedThreadID, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	binding := turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}, EventID: submissionEventID(rec.HostedThreadID, action)}
	if _, err := sess.turns.Accepted(ctx, turns.Acceptance{ActionID: action, ClaimToken: "claim", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}

	daemon.emitThreadNotification(native, map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": native, "turnId": turn, "item": map[string]any{"id": "final-human", "type": "agentMessage", "phase": "final_answer", "text": "done"},
	}})
	daemon.emitThreadNotification(native, map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": native, "turn": map[string]any{"id": turn, "status": "completed", "completedAt": 1787600100},
	}})
	waitForClaude(t, "the turn's completion recorded", func() bool {
		feed, err := s.completedTurns.after(rec.HostedThreadID, "", 10)
		return err == nil && len(feed) == 1
	})
	if got, _ := s.agentExecutions.turn(op.ID); threadvalues.TurnTerminal(got.Status) {
		t.Fatalf("operation finished with the turn although its own row was never saved: %#v", got)
	}

	// Once its own row is proven saved, it finishes with the turn's recorded
	// final answer.
	if _, settled, err := sess.turns.Settle(ctx, action); err != nil || !settled {
		t.Fatalf("settle: %v %v", settled, err)
	}
	sess.finishJoinedAgentTurns(ctx, turn)
	if got, _ := s.agentExecutions.turn(op.ID); got.Status != agentTurnCompleted || got.FinalItemID != "final-human" || got.FinalResponse != "done" {
		t.Fatalf("proven operation = status %s final %q %q; want the turn's final answer", got.Status, got.FinalItemID, got.FinalResponse)
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
			sess.codex = fake.daemon()
			// The thread is already off working before the core exists, so no
			// kick from reaching that state can send the prompt queued next:
			// only the return under test can.
			sess.setTurnActiveState(true, "turn-running", 0, false, false)
			if path == "reconnected" {
				sess.setStatus("reconnecting")
			} else {
				sess.addPendingInteraction(pendingInteraction{ID: "approval-1", Method: "item/commandExecution/requestApproval", TurnID: "turn-running", ThreadID: sess.threadID})
			}
			if err := sess.ensureCoreSession(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sess.coreSession().Close)
			text := "Use the other file."
			if _, _, err := sess.turns.Submit(ctx, turns.Submission{HostedThreadID: sess.hostedThreadID, Provider: model.ProviderCodex, NativeThreadID: sess.threadID,
				ActionID: "queued", ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
				t.Fatal(err)
			}

			if path == "reconnected" {
				sess.observeConnection(lifecycle.EventReadyWorking)
			} else if _, ok := sess.claimPendingInteraction("approval-1"); !ok {
				t.Fatal("approval not pending")
			}
			waitTurnSendDone(t, sess, "queued")
			steers := fake.steerParams()
			var params struct {
				ExpectedTurnID string `json:"expectedTurnId"`
			}
			if len(steers) != 1 || json.Unmarshal(steers[0], &params) != nil || params.ExpectedTurnID != "turn-running" {
				t.Fatalf("turn/steer = %s; want one steer into turn-running", steers)
			}
			if record, _, err := sess.turns.Get(ctx, "queued"); err != nil || record.Outbox.State != turns.Accepted {
				t.Fatalf("queued prompt state=%s err=%v; want accepted", record.Outbox.State, err)
			}
		})
	}
}

// explicitZeroUsage reports a turn that records zero for all three counters.
func explicitZeroUsage(record coreturns.Record) bool {
	for _, value := range []*int64{record.UsageInputTokens, record.UsageCachedInputTokens, record.UsageOutputTokens} {
		if value == nil || *value != 0 {
			return false
		}
	}
	return true
}
