package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/interact"
)

func stage0Server(t *testing.T) (*server, *agentExecutionStore, *session, agentSessionRecord, agentSessionRecord, chan string) {
	t.Helper()
	control := newTestControlMetadata(t)
	mail := newTestAgentMessageStore(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	sender, _, _ := store.beginSession("sender", "sender", "claude", presentationShared)
	if err := store.BindSession(sender.ID, "nano-sender", "019f0000-0000-7000-8000-000000000001", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	sender, _ = store.session(sender.ID)
	worker, _, _ := store.beginSessionWithInvoker("worker", "worker", "codex", presentationShared, sender, nil)
	if err := store.BindSession(worker.ID, "nano-worker", "native-worker", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	worker, _ = store.session(worker.ID)
	prompts := make(chan string, 8)
	queue := mustQueuedTurnStore(t)
	turnStore, err := turns.OpenStore(context.Background(), queue.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, err := newSendAttemptStore(queue.db, queue.path, "stage0-test", testPromptStore(t, queue.db))
	if err != nil {
		t.Fatal(err)
	}
	queue.turns, attempts.turns = turnStore, turnStore
	projection := newLiveProjection("codex", worker.ID)
	projection.SetNativeThreadID("native-worker")
	sess := &session{id: "nano-worker",
		provider:        "codex",
		hostedThreadID:  worker.ID,
		threadID:        "native-worker",
		createdAt:       time.Now(),
		interactions:    map[string]pendingInteraction{},
		projection:      projection,
		agentExecutions: store,
		sendOps:         map[string]*turnSendOperation{},
		queuedTurns:     queue,
		sendAttempts:    attempts,
		turns:           turnStore,
		lifecycleState:  testLifecycleSnapshot("ready", false)}
	sess.turnSender = func(_ context.Context, prompt string) (json.RawMessage, error) {
		prompts <- prompt
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	}
	// newTestControlMetadata closes the shared database during cleanup. Drain
	// every delivery first so the fixture observes production's shutdown order.
	t.Cleanup(func() {
		sess.sendMu.Lock()
		actionIDs := make([]string, 0, len(sess.sendOps))
		for actionID := range sess.sendOps {
			actionIDs = append(actionIDs, actionID)
		}
		sess.sendMu.Unlock()
		for _, actionID := range actionIDs {
			waitTurnSendDone(t, sess, actionID)
		}
	})
	notices := interact.NewNotices()
	sess.appendUnread = notices.Append
	s := &server{agentExecutions: store, controlMetadata: control, mail: mail, notices: notices, catalog: renamedCatalog{name: "Claude sender"}, sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}}, queuedTurns: queue, sendAttempts: attempts, turns: turnStore}
	return s, store, sess, sender, worker, prompts
}

func TestAgentPromptCarriesSenderFooterAndAttribution(t *testing.T) {
	s, store, _, sender, worker, prompts := stage0Server(t)
	body := `{"clientActionId":"ask-1","prompt":"Please review the parser.","senderThreadRef":"` + sender.ID + `"}`
	rec := httptest.NewRecorder()
	s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"warning"`) {
		t.Fatalf("identified sender must not warn: %s", rec.Body.String())
	}
	op, ok := store.turnForAction(worker.ID, "ask-1")
	if !ok {
		t.Fatal("operation missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, op.SenderSubmittedAt); err != nil {
		t.Fatalf("senderSubmittedAt = %q, want RFC3339Nano UTC admission time: %v", op.SenderSubmittedAt, err)
	}
	var view agentTurnView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.SenderSubmittedAt != op.SenderSubmittedAt {
		t.Fatalf("response senderSubmittedAt = %q, operation = %q, decode error = %v", view.SenderSubmittedAt, op.SenderSubmittedAt, err)
	}
	select {
	case got := <-prompts:
		want := "Please review the parser.\n\n" + senderFooter(turnAttribution{Origin: turnOriginAgent, SenderName: "Claude sender", SenderNativeThreadID: sender.NativeThreadID, SenderSubmittedAt: op.SenderSubmittedAt})
		if got != want {
			t.Fatalf("delivered = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery")
	}
	deadline := time.Now().Add(time.Second)
	for op.Status == agentTurnDeliveryPending && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		op, _ = store.turnForAction(worker.ID, "ask-1")
	}
	if op.Status != agentTurnRunning {
		t.Fatalf("delivery status = %q, want %q", op.Status, agentTurnRunning)
	}
	if op.Origin != turnOriginAgent || op.SenderHostedThreadID != sender.ID || op.SenderNativeThreadID != sender.NativeThreadID || op.SenderProvider != "claude" {
		t.Fatalf("attribution = %+v", op)
	}
	// The idempotency hash covers the caller's prompt only, not the footer.
	if op.PromptHash != hostedTurnPromptHash("Please review the parser.", false) {
		t.Fatalf("prompt hash covers footer")
	}
	// Replay with the same id is idempotent (200, same operation).
	rec = httptest.NewRecorder()
	s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
	}
	// Survives reload from the control store.
	reloaded, err := newControlAgentExecutionStore(s.controlMetadata, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := reloaded.turn(op.ID)
	if again.Origin != turnOriginAgent || again.SenderHostedThreadID != sender.ID || again.SenderSubmittedAt != op.SenderSubmittedAt {
		t.Fatalf("attribution lost on reload: %+v", again)
	}
}

func TestAgentPromptRejectsMissingUnresolvedOrIncompleteSenderBeforeArtifacts(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		code    string
		prepare func(*server, agentSessionRecord)
	}{
		{name: "missing", body: `{"clientActionId":"missing","prompt":"hi"}`, code: "sender_required"},
		{name: "unresolved", body: `{"clientActionId":"unresolved","prompt":"hi","senderThreadRef":"nht_nope"}`, code: "sender_not_hosted"},
		{name: "provisional native id", body: `{"clientActionId":"provisional","prompt":"hi","senderThreadRef":"sender"}`, code: "sender_identity_incomplete", prepare: func(s *server, sender agentSessionRecord) {
			if err := s.agentExecutions.BindSession(sender.ID, "nano-sender", "cs_provisional", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, store, sess, sender, worker, prompts := stage0Server(t)
			if test.prepare != nil {
				test.prepare(s, sender)
			}
			rec := httptest.NewRecorder()
			s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(test.body)), worker.ID)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.code) {
				t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
			}
			if _, ok := store.turnForAction(worker.ID, strings.Split(strings.TrimPrefix(test.body, `{"clientActionId":"`), `"`)[0]); ok {
				t.Fatalf("rejected request created an operation")
			}
			if got := sess.queuedTurns.QueuedCount("codex", "native-worker", worker.ID); got != 0 {
				t.Fatalf("rejected request queued %d turns", got)
			}
			select {
			case got := <-prompts:
				t.Fatalf("rejected request reached provider: %q", got)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestInternalTurnHelperRejectsUnattributedAgentOriginBeforeArtifacts(t *testing.T) {
	s, store, sess, _, worker, prompts := stage0Server(t)
	_, _, err := s.submitHostedTurnWithOptions(worker.ID, "legacy-agent-action", "do not deliver", true)
	if err == nil || !strings.Contains(err.Error(), "senderThreadRef") {
		t.Fatalf("error = %v", err)
	}
	if _, ok := store.turnForAction(worker.ID, "legacy-agent-action"); ok {
		t.Fatal("unattributed internal agent action created an operation")
	}
	if got := sess.queuedTurns.QueuedCount("codex", "native-worker", worker.ID); got != 0 {
		t.Fatalf("unattributed internal agent action queued %d turns", got)
	}
	select {
	case got := <-prompts:
		t.Fatalf("unattributed internal agent action reached provider: %q", got)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestQueueDeliversHumanBeforeAgentBeforeSystem(t *testing.T) {
	q := mustQueuedTurnStore(t)
	ctx := context.Background()
	put := func(id string, origin turns.Origin) {
		if _, _, err := q.turns.Submit(ctx, turns.Submission{HostedThreadID: "h", Provider: "codex", NativeThreadID: "n", ActionID: id, ProviderText: id, TextHash: id, RequestHash: id, Origin: origin}); err != nil {
			t.Fatal(err)
		}
	}
	put("sys-1", turns.SystemOrigin)
	put("agent-1", turns.AgentOrigin)
	put("agent-2", turns.AgentOrigin)
	put("human-1", turns.HumanOrigin)
	put("legacy-1", "") // legacy records count as human
	if got := q.Position("human-1"); got != 1 {
		t.Fatalf("human position = %d", got)
	}
	if got := q.Position("legacy-1"); got != 2 {
		t.Fatalf("legacy position = %d", got)
	}
	if got := q.Position("agent-1"); got != 3 {
		t.Fatalf("agent-1 position = %d (should rise behind both human prompts)", got)
	}
	if got := q.Position("sys-1"); got != 5 {
		t.Fatalf("system position = %d", got)
	}
	var order []string
	for {
		rec, ok := q.First("codex", "n", "h")
		if !ok {
			break
		}
		order = append(order, rec.ClientActionID)
		if _, err := q.turns.Cancel(ctx, rec.ClientActionID); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(order, ",") != "human-1,legacy-1,agent-1,agent-2,sys-1" {
		t.Fatalf("delivery order = %v", order)
	}
}

func TestAvailabilityFromHardFacts(t *testing.T) {
	s, store, sess, _, worker, _ := stage0Server(t)
	now := time.Now().UTC()
	if v := s.availabilityFor(worker.ID, now); v.State != availabilityAvailable {
		t.Fatalf("idle = %+v", v)
	}
	sess.setTurnActive(true, "t1", 0, false)
	if v := s.availabilityFor(worker.ID, now); v.State != availabilityBusy || v.Reasons[0] != "turn_active" {
		t.Fatalf("active = %+v", v)
	}
	sess.setTurnActive(false, "", 0, true)
	sess.noteHumanPrompt(now.Add(-2 * time.Minute))
	if v := s.availabilityFor(worker.ID, now); v.State != availabilityAvailable || v.Facts.LastHumanMessageAt == "" {
		t.Fatalf("recent human remains diagnostic only = %+v", v)
	}
	// Durable copy survives a reload.
	reloaded, err := newControlAgentExecutionStore(s.controlMetadata, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := reloaded.session(worker.ID)
	if rec.LastHumanPromptAt == "" {
		t.Fatal("last human prompt not persisted")
	}
	// Pending interaction holds the thread.
	sess.addPendingInteraction(pendingInteraction{ID: "req-1", Method: "item/permissions/requested"})
	if v := s.availabilityFor(worker.ID, now.Add(10*time.Minute)); v.State != availabilityAwaitingHuman {
		t.Fatalf("pending interaction = %+v", v)
	}
	sess.takePendingInteraction("req-1")
	// Detached and unknown threads are unreachable.
	delete(s.sessions, sess.id)
	if v := s.availabilityFor(worker.ID, now); v.State != availabilityUnreachable || v.Facts.Attachment != "detached" {
		t.Fatalf("detached = %+v", v)
	}
	if v := s.availabilityFor("nht_missing", now); v.State != availabilityUnreachable || v.Facts.Attachment != "deleted" {
		t.Fatalf("missing = %+v", v)
	}
	_ = store
}

func TestRetryReplaysPersistedFooterAfterRename(t *testing.T) {
	s, store, sess, sender, worker, prompts := stage0Server(t)
	sess.setTurnActive(true, "busy", 0, false) // force queueing so the text persists without delivery
	body := `{"clientActionId":"ask-2","prompt":"Q","senderThreadRef":"` + sender.ID + `"}`
	rec := httptest.NewRecorder()
	s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	op, ok := store.turnForAction(worker.ID, "ask-2")
	if !ok || op.SenderSubmittedAt == "" {
		t.Fatalf("queued operation missing immutable submission timestamp: %+v", op)
	}
	// A retry must replay the original committed footer, not stamp a new
	// submission time after the sender is renamed.
	if err := store.MarkTurnDelivery(op.ID, "", agentTurnDeliveryPending, nil); err != nil {
		t.Fatalf("restore pending delivery = %v", err)
	}
	// Simulate a rename: the footer Nano would compose now differs.
	s.catalog = renamedCatalog{name: "Renamed Sender"}
	rec = httptest.NewRecorder()
	s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after rename = %d %s", rec.Code, rec.Body.String())
	}
	sess.setTurnActive(false, "", 0, true)
	select {
	case got := <-prompts:
		wantFooter := senderFooter(turnAttribution{Origin: turnOriginAgent, SenderName: "Claude sender", SenderNativeThreadID: sender.NativeThreadID, SenderSubmittedAt: op.SenderSubmittedAt})
		if !strings.HasSuffix(got, wantFooter) {
			t.Fatalf("delivered text was recomposed after rename: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("queued prompt was not drained")
	}
}

type renamedCatalog struct{ name string }

func (c renamedCatalog) ThreadNameFor(context.Context, string, string) (string, bool) {
	return c.name, true
}

func (renamedCatalog) ListThreads(context.Context, catalogListQuery) (catalogPage, error) {
	return catalogPage{}, nil
}

func (renamedCatalog) LoadThread(context.Context, string) (catalogThread, error) {
	return catalogThread{}, nil
}

func (renamedCatalog) IndexStatus(context.Context) (catalogIndexStatus, error) {
	return catalogIndexStatus{}, nil
}

func (renamedCatalog) RequestRefresh() bool { return false }

// Catches: the newest human prompt time losing to an older one when the older
// one's fraction ends in zeros. RFC3339Nano trims them, so as text ".2374Z"
// sorts after the later ".237401Z". Boundary: the session's note, in memory and
// in the durable copy a reload reads.
func TestLastHumanPromptMovesForwardAcrossTrimmedFractions(t *testing.T) {
	s, _, sess, _, worker, _ := stage0Server(t)
	earlier := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	later := earlier.Add(time.Microsecond)
	if earlier.Format(time.RFC3339Nano) <= later.Format(time.RFC3339Nano) {
		t.Fatal("fixture: the earlier time must sort after the later one as text")
	}
	for _, at := range []time.Time{earlier, later, earlier} {
		sess.noteHumanPrompt(at)
	}
	want := later.Format(time.RFC3339Nano)
	if rec, _ := s.agentExecutions.session(worker.ID); rec.LastHumanPromptAt != want {
		t.Fatalf("last human prompt = %q, want %q", rec.LastHumanPromptAt, want)
	}
	reloaded, err := newControlAgentExecutionStore(s.controlMetadata, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := reloaded.session(worker.ID); rec.LastHumanPromptAt != want {
		t.Fatalf("durable last human prompt = %q, want %q", rec.LastHumanPromptAt, want)
	}
}
