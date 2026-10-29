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

	promptstore "github.com/dkroshow/nano/internal/core/prompts"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threads"
)

func stage0Server(t *testing.T, seed ...func(*hostedstore.ExecutionStore, threads.SessionRecord)) (*server, *hostedstore.ExecutionStore, *sessions.Session, threads.SessionRecord, threads.SessionRecord, chan string, *fakeAppServer) {
	t.Helper()
	control := newTestControlMetadata(t)
	mail := newTestAgentMessageStore(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	sender, _, _ := fixtureBeginSession(store, "sender", "sender", "claude", presentationShared)
	if err := store.BindSession(sender.ID, "nano-sender", "019f0000-0000-7000-8000-000000000001", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	sender, _ = store.Session(sender.ID)
	worker, _, _ := fixtureBeginSessionWithInvoker(store, "worker", "worker", "codex", presentationShared, sender, nil)
	if err := store.BindSession(worker.ID, "nano-worker", "native-worker", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	worker, _ = store.Session(worker.ID)
	if len(seed) > 0 {
		seed[0](store, sender)
		store, err = newControlAgentExecutionStore(control, "run-1")
		if err != nil {
			t.Fatal(err)
		}
	}
	prompts := make(chan string, 8)
	queue := mustQueuedTurnStore(t)
	turnStore, err := promptstore.OpenStore(context.Background(), queue.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "stage0-test", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	queue.BindTransactionStore(turnStore)
	attempts.BindTransactionStore(turnStore)
	projection := newLiveProjection("codex", worker.ID)
	notices := interact.NewNotices()
	fake := newFakeAppServer(t)
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "provider-turn"}})
	fake.OnTurnStart = func(prompt string) { prompts <- prompt }
	daemon := fake.daemon()
	s := &server{agentExecutions: store, controlMetadata: control, mail: mail, notices: notices, catalog: fixtureNamedCatalog(t, sender.Provider, sender.NativeThreadID, "Claude sender"), queuedTurns: queue, sendAttempts: attempts, turns: turnStore}
	sess := newSession(sessions.Config{ID: "nano-worker", Provider: "codex", HostedThreadID: worker.ID, CreatedAt: time.Now(), Projection: projection, AgentExecutions: store, QueuedTurns: queue, SendAttempts: attempts, Turns: turnStore, Metadata: control, AppendUnread: notices.Append, NanoStopping: &s.stopping})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID("native-worker")
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: "native-worker"}, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.View().Summary().Status == "ready" })
	// Stop transport work before the shared database cleanup.
	t.Cleanup(func() {
		for _, done := range sess.Delivery().InFlightDeliveries() {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("fixture delivery did not finish")
			}
		}
		sess.Lifecycle().Close()
	})
	seedTestSessionRegistry(&s.Registry, map[string]*sessions.Session{sess.ID(): sess})
	return s, store, sess, sender, worker, prompts, fake
}

func TestAgentPromptCarriesSenderFooterAndAttribution(t *testing.T) {
	s, store, _, sender, worker, prompts, _ := stage0Server(t)
	body := `{"clientActionId":"ask-1","prompt":"Please review the parser.","senderThreadRef":"` + sender.ID + `"}`
	rec := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"warning"`) {
		t.Fatalf("identified sender must not warn: %s", rec.Body.String())
	}
	op, ok := fixtureTurnForAction(store, worker.ID, "ask-1")
	if !ok {
		t.Fatal("operation missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, op.SenderSubmittedAt); err != nil {
		t.Fatalf("senderSubmittedAt = %q, want RFC3339Nano UTC admission time: %v", op.SenderSubmittedAt, err)
	}
	var view hostedmodel.TurnView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.SenderSubmittedAt != op.SenderSubmittedAt {
		t.Fatalf("response senderSubmittedAt = %q, operation = %q, decode error = %v", view.SenderSubmittedAt, op.SenderSubmittedAt, err)
	}
	select {
	case got := <-prompts:
		want := "Please review the parser.\n\n" + senderFooter(hostedstore.TurnAttribution{Origin: turnOriginAgent, SenderName: "Claude sender", SenderNativeThreadID: sender.NativeThreadID, SenderSubmittedAt: op.SenderSubmittedAt})
		if got != want {
			t.Fatalf("delivered = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery")
	}
	deadline := time.Now().Add(time.Second)
	for op.Status == agentTurnDeliveryPending && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		op, _ = fixtureTurnForAction(store, worker.ID, "ask-1")
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
	fixtureOwners(s).hostedController().HandleTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay = %d %s", rec.Code, rec.Body.String())
	}
	// Survives reload from the control store.
	reloaded, err := newControlAgentExecutionStore(s.controlMetadata, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := reloaded.Turn(op.ID)
	if again.Origin != turnOriginAgent || again.SenderHostedThreadID != sender.ID || again.SenderSubmittedAt != op.SenderSubmittedAt {
		t.Fatalf("attribution lost on reload: %+v", again)
	}
}

func TestAgentPromptRejectsMissingUnresolvedOrIncompleteSenderBeforeArtifacts(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		code    string
		prepare func(*server, threads.SessionRecord)
	}{
		{name: "missing", body: `{"clientActionId":"missing","prompt":"hi"}`, code: "sender_required"},
		{name: "unresolved", body: `{"clientActionId":"unresolved","prompt":"hi","senderThreadRef":"nht_nope"}`, code: "sender_not_hosted"},
		{name: "provisional native id", body: `{"clientActionId":"provisional","prompt":"hi","senderThreadRef":"sender"}`, code: "sender_identity_incomplete", prepare: func(s *server, sender threads.SessionRecord) {
			if err := s.agentExecutions.BindSession(sender.ID, "nano-sender", "cs_provisional", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, store, sess, sender, worker, prompts, _ := stage0Server(t)
			if test.prepare != nil {
				test.prepare(s, sender)
			}
			rec := httptest.NewRecorder()
			fixtureOwners(s).hostedController().HandleTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(test.body)), worker.ID)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.code) {
				t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
			}
			if _, ok := fixtureTurnForAction(store, worker.ID, strings.Split(strings.TrimPrefix(test.body, `{"clientActionId":"`), `"`)[0]); ok {
				t.Fatalf("rejected request created an operation")
			}
			if got := sess.Delivery().QueuedPromptCount(); got != 0 {
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
	s, store, sess, _, worker, prompts, _ := stage0Server(t)
	_, _, err := s.submitHostedTurnWithOptions(worker.ID, "legacy-agent-action", "do not deliver", true)
	if err == nil || !strings.Contains(err.Error(), "senderThreadRef") {
		t.Fatalf("error = %v", err)
	}
	if _, ok := fixtureTurnForAction(store, worker.ID, "legacy-agent-action"); ok {
		t.Fatal("unattributed internal agent action created an operation")
	}
	if got := sess.Delivery().QueuedPromptCount(); got != 0 {
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
	put := func(id string, origin promptstore.Origin) {
		if _, _, err := q.TransactionStore().Submit(ctx, promptstore.Submission{HostedThreadID: "h", Provider: "codex", NativeThreadID: "n", ActionID: id, ProviderText: id, TextHash: id, RequestHash: id, Origin: origin}); err != nil {
			t.Fatal(err)
		}
	}
	put("sys-1", promptstore.SystemOrigin)
	put("agent-1", promptstore.AgentOrigin)
	put("agent-2", promptstore.AgentOrigin)
	put("human-1", promptstore.HumanOrigin)
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
		if _, err := q.TransactionStore().Cancel(ctx, rec.ClientActionID); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(order, ",") != "human-1,legacy-1,agent-1,agent-2,sys-1" {
		t.Fatalf("delivery order = %v", order)
	}
}

func TestAvailabilityFromHardFacts(t *testing.T) {
	s, store, sess, _, worker, prompts, fake := stage0Server(t)
	now := time.Now().UTC()
	if v := fixtureOwners(s).hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityAvailable {
		t.Fatalf("idle = %+v", v)
	}
	(&providerRuntimeFixture{session: sess, codex: fake}).activate(t)
	if v := fixtureOwners(s).hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityBusy || v.Reasons[0] != "turn_active" {
		t.Fatalf("active = %+v", v)
	}
	(&providerRuntimeFixture{session: sess, codex: fake}).emitAndWait(t, []byte(`{"method":"turn/completed","params":{"threadId":"native-worker","turn":{"status":"completed"}}}`))
	if response := postTurn(t, sess.Delivery(), context.Background(), "human-recent", "hello"); response.Code != http.StatusOK {
		t.Fatalf("human prompt = %d %s", response.Code, response.Body.String())
	}
	<-prompts
	(&providerRuntimeFixture{session: sess, codex: fake}).emitAndWait(t, []byte(`{"method":"turn/completed","params":{"threadId":"native-worker","turn":{"id":"provider-turn","status":"completed"}}}`))
	if v := fixtureOwners(s).hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityAvailable || v.Facts.LastHumanMessageAt == "" {
		t.Fatalf("recent human remains diagnostic only = %+v", v)
	}
	// Durable copy survives a reload.
	reloaded, err := newControlAgentExecutionStore(s.controlMetadata, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := reloaded.Session(worker.ID)
	if rec.LastHumanPromptAt == "" {
		t.Fatal("last human prompt not persisted")
	}
	// Pending interaction holds the thread.
	fake.EmitNotification(map[string]any{"id": "req-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": sess.State().ThreadID()}})
	waitForClaude(t, "pending permissions", func() bool { return len(sess.Delivery().PendingInteractionList()) == 1 })
	if v := fixtureOwners(s).hostedViews().AvailabilityFor(worker.ID, now.Add(10*time.Minute)); v.State != availabilityAwaitingHuman {
		t.Fatalf("pending interaction = %+v", v)
	}
	response := httptest.NewRecorder()
	sess.Delivery().HandleInteraction(response, httptest.NewRequest(http.MethodPost, "/interactions/req-1", strings.NewReader(`{"decision":"decline"}`)), sess.Delivery().PendingInteractionList()[0].ID)
	if response.Code != http.StatusOK {
		t.Fatalf("interaction decline = %d %s", response.Code, response.Body.String())
	}
	// Detached and unknown threads are unreachable.
	s.RemoveSession(sess.ID())
	if v := fixtureOwners(s).hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityUnreachable || v.Facts.Attachment != "detached" {
		t.Fatalf("detached = %+v", v)
	}
	if v := fixtureOwners(s).hostedViews().AvailabilityFor("nht_missing", now); v.State != availabilityUnreachable || v.Facts.Attachment != "deleted" {
		t.Fatalf("missing = %+v", v)
	}
	_ = store
}

func TestRetryReplaysPersistedFooterAfterRename(t *testing.T) {
	s, store, sess, sender, worker, prompts, fake := stage0Server(t)
	(&providerRuntimeFixture{session: sess, codex: fake}).activate(t) // force queueing so the text persists without delivery
	body := `{"clientActionId":"ask-2","prompt":"Q","senderThreadRef":"` + sender.ID + `"}`
	rec := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	op, ok := fixtureTurnForAction(store, worker.ID, "ask-2")
	if !ok || op.SenderSubmittedAt == "" {
		t.Fatalf("queued operation missing immutable submission timestamp: %+v", op)
	}
	// A retry must replay the original committed footer, not stamp a new
	// submission time after the sender is renamed.
	if err := store.MarkTurnDelivery(op.ID, "", agentTurnDeliveryPending, nil); err != nil {
		t.Fatalf("restore pending delivery = %v", err)
	}
	// Simulate a rename: the footer Nano would compose now differs.
	s.catalog = fixtureNamedCatalog(t, sender.Provider, sender.NativeThreadID, "Renamed Sender")
	rec = httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/x", bytes.NewBufferString(body)), worker.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after rename = %d %s", rec.Code, rec.Body.String())
	}
	(&providerRuntimeFixture{session: sess, codex: fake}).emitAndWait(t, []byte(`{"method":"turn/completed","params":{"threadId":"native-worker","turn":{"status":"completed"}}}`))
	select {
	case got := <-prompts:
		wantFooter := senderFooter(hostedstore.TurnAttribution{Origin: turnOriginAgent, SenderName: "Claude sender", SenderNativeThreadID: sender.NativeThreadID, SenderSubmittedAt: op.SenderSubmittedAt})
		if !strings.HasSuffix(got, wantFooter) {
			t.Fatalf("delivered text was recomposed after rename: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("queued prompt was not drained")
	}
}

func TestHumanPromptStoreAdvancesAcrossTrimmedFractions(t *testing.T) {
	s, _, _, _, worker, _, _ := stage0Server(t)
	earlier := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	later := earlier.Add(time.Microsecond)
	if earlier.Format(time.RFC3339Nano) <= later.Format(time.RFC3339Nano) {
		t.Fatal("fixture: the earlier time must sort after the later one as text")
	}
	for _, at := range []time.Time{earlier, later, earlier} {
		if err := s.agentExecutions.NoteHumanPrompt(worker.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	want := later.Format(time.RFC3339Nano)
	if rec, _ := s.agentExecutions.Session(worker.ID); rec.LastHumanPromptAt != want {
		t.Fatalf("last human prompt = %q, want %q", rec.LastHumanPromptAt, want)
	}
	reloaded, err := newControlAgentExecutionStore(s.controlMetadata, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := reloaded.Session(worker.ID); rec.LastHumanPromptAt != want {
		t.Fatalf("durable last human prompt = %q, want %q", rec.LastHumanPromptAt, want)
	}
}
