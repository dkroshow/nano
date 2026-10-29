package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestAgentTurnAPIIsProviderNeutralAndIdempotent(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "executions.sqlite"), "run-1")
			if err != nil {
				t.Fatal(err)
			}
			queue := mustQueuedTurnStore(t)
			attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "run-1", testPromptStore(t, queue.Database()))
			if err != nil {
				t.Fatal(err)
			}
			turnStore, err := turns.OpenStore(context.Background(), queue.Database())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
				t.Fatal(err)
			}
			queue.BindTransactionStore(turnStore)
			attempts.BindTransactionStore(turnStore)
			clientID := provider + "-caller-session"
			hosted, _, _ := fixtureBeginSession(store, clientID, "request-hash", provider, presentationBackground)
			_ = store.BindSession(hosted.ID, "nano-1", provider+"-native", "ready", presentationBackground)
			fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-1", Provider: provider, HostedThreadID: hosted.ID, PresentationMode: presentationBackground, CreatedAt: time.Now(), Projection: newLiveProjection(provider), AgentExecutions: store, Turns: turnStore, QueuedTurns: queue, SendAttempts: attempts}, provider+"-native", "")
			sess := fixture.session
			s := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store}, map[string]*session{"nano-1": sess})
			sender := confirmedTestSender(t, s, provider)

			body, err := json.Marshal(map[string]string{"prompt": "inspect only", "clientActionId": "action-1", "senderThreadRef": sender})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-sessions/"+clientID+"/turns", bytes.NewReader(body))
			applicationHandler(s).ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("submit status = %d, body %s", rec.Code, rec.Body.String())
			}
			var submitted agentTurnView
			if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
				t.Fatal(err)
			}
			latest, ok := fixtureWaitTurn(store, context.Background(), submitted.ID, submitted.Revision, time.Second)
			if !ok || latest.Status != agentTurnRunning || latest.ProviderTurnID != "provider-turn-1" {
				t.Fatalf("delivered operation = %#v", latest)
			}

			// The same caller action returns the same operation and never invokes
			// the provider a second time.
			rec = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodPost, "/api/v1/agent-sessions/"+clientID+"/turns", bytes.NewReader(body))
			applicationHandler(s).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("duplicate status = %d, body %s", rec.Code, rec.Body.String())
			}
			var duplicate agentTurnView
			_ = json.Unmarshal(rec.Body.Bytes(), &duplicate)
			if duplicate.ID != submitted.ID {
				t.Fatalf("duplicate operation %q != %q", duplicate.ID, submitted.ID)
			}

			rec = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodGet, "/api/v1/turn-operations/"+submitted.ID, nil)
			applicationHandler(s).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("operation status = %d, body %s", rec.Code, rec.Body.String())
			}
			var view agentTurnView
			_ = json.Unmarshal(rec.Body.Bytes(), &view)
			if view.Provider != provider || view.Status != agentTurnRunning || view.Terminal {
				t.Fatalf("operation view = %#v", view)
			}

			cannotCancel := httptest.NewRecorder()
			applicationHandler(s).ServeHTTP(cannotCancel, httptest.NewRequest(http.MethodDelete,
				"/api/v1/turn-operations/"+submitted.ID, nil))
			if cannotCancel.Code != http.StatusConflict {
				t.Fatalf("running operation cancel status = %d, body %s", cannotCancel.Code, cannotCancel.Body.String())
			}
		})
	}
}

func TestBindingPersistFailureMarksAgentDeliveryUnknown(t *testing.T) {
	store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, _ := fixtureBeginSession(store, "codex-caller-session", "request-hash", "codex", presentationBackground)
	_ = store.BindSession(hosted.ID, "nano-1", "codex-native", "ready", presentationBackground)
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "run-1", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	turnStore, err := turns.OpenStore(context.Background(), queue.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turnStore.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue.BindTransactionStore(turnStore)
	attempts.BindTransactionStore(turnStore)
	projection := newLiveProjection("codex", hosted.ID)
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, CreatedAt: time.Now(), Projection: projection, AgentExecutions: store, Turns: turnStore, QueuedTurns: queue, SendAttempts: attempts}, "codex-native", "")
	sess := fixture.session
	fixture.codex.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "provider-turn-bind-fail"}})
	if _, err := queue.Database().Exec(`CREATE TRIGGER fail_transactional_bind BEFORE UPDATE OF provider_turn_id ON queued_turns
		WHEN NEW.provider_turn_id='provider-turn-bind-fail' BEGIN SELECT RAISE(FAIL,'synthetic bind failure'); END`); err != nil {
		t.Fatal(err)
	}
	s := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store}, map[string]*session{"nano-1": sess})
	sender := confirmedTestSender(t, s, "codex")
	body, err := json.Marshal(map[string]string{"prompt": "keep this text", "clientActionId": "action-bind-fail", "senderThreadRef": sender})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-sessions/codex-caller-session/turns", bytes.NewReader(body))
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body %s", rec.Code, rec.Body.String())
	}
	var submitted agentTurnView
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	latest, ok := store.Turn(submitted.ID)
	if !ok {
		t.Fatal("missing operation")
	}
	waitTurnSendDone(t, sess, latest.TransportActionID)
	latest, ok = store.Turn(submitted.ID)
	if !ok || latest.Status != agentTurnDeliveryUnknown {
		t.Fatalf("operation after bind failure = %#v found=%v", latest, ok)
	}
	queued, found := queue.Get(latest.TransportActionID)
	if !found || queued.Text == "" || queued.State != submissionUncertain {
		t.Fatalf("text was not retained: %#v found=%v", queued, found)
	}
}

func TestAgentExecutionObserverUsesGrokPromptCompleteEvidence(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "grok-caller", "request-hash", "grok", presentationShared)
	_ = store.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared)
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-1", Provider: "grok", HostedThreadID: hosted.ID, AgentExecutions: store, Projection: newLiveProjection("grok")}, "native-1", "")
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt-hash")
	_ = store.MarkTurnDelivery(op.ID, "turn-1", agentTurnRunning, nil)

	fixture.emit(t, []byte(`{
      "jsonrpc":"2.0","method":"session/update",
      "params":{"sessionId":"native-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}},"_meta":{"promptId":"turn-1","eventId":"event-1"}}
    }`))
	fixture.emit(t, []byte(`{
      "jsonrpc":"2.0","method":"_x.ai/session/prompt_complete",
      "params":{"sessionId":"native-1","promptId":"turn-1","stopReason":"end_turn"}
    }`))
	waitForClaude(t, "terminal operation", func() bool { got, _ := store.Turn(op.ID); return got.Status == agentTurnCompleted })
	after, _ := store.Turn(op.ID)
	if after.Status != agentTurnCompleted || after.FinalResponse != "done" {
		t.Fatalf("Grok terminal operation = %#v", after)
	}
}

func TestAgentExecutionObserverUsesCodexTurnTerminalEvidence(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "caller-session", "request-hash", "codex", presentationShared)
	_ = store.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared)
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt-hash")
	_ = store.MarkTurnDelivery(op.ID, "turn-1", agentTurnRunning, nil)
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID, AgentExecutions: store}, "native-1", "")

	fixture.emit(t, []byte(`{
      "method":"item/completed",
      "params":{"threadId":"native-1","turnId":"turn-1","item":{"id":"answer-1","type":"agentMessage","phase":"final_answer","text":"done"}}
    }`))
	waitForClaude(t, "candidate answer", func() bool { got, _ := store.Turn(op.ID); return got.CandidateItemID == "answer-1" })
	before, _ := store.Turn(op.ID)
	if before.Status != agentTurnRunning {
		t.Fatalf("assistant message ended operation: %#v", before)
	}
	fixture.emit(t, []byte(`{
      "method":"turn/completed",
      "params":{"threadId":"native-1","turn":{"id":"turn-1","status":"completed"}}
    }`))
	waitForClaude(t, "terminal operation", func() bool { got, _ := store.Turn(op.ID); return got.Status == agentTurnCompleted })
	after, _ := store.Turn(op.ID)
	if after.Status != agentTurnCompleted || after.FinalItemID != "answer-1" || after.FinalResponse != "done" {
		t.Fatalf("terminal operation = %#v", after)
	}
}

func TestAgentSessionCreateRejectsInvalidPresentationBeforeProviderWork(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	s := &server{
		defaults: sessionDefaults{cwd: t.TempDir()},
		Registry: newSessionRegistry(), agentExecutions: store,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent-sessions", bytes.NewReader([]byte(`{
    "clientSessionId":"caller-session","provider":"codex","presentationMode":"invisible"
  }`)))
	s.handleAgentSessionCreate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid presentation status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, ok := (&server{agentExecutions: store, Registry: newSessionRegistry()}).resolveHostedThreadRecord("caller-session"); ok {
		t.Fatal("invalid request was persisted")
	}
}

func TestAgentCreatedHostedThreadRequiresKnownInvoker(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	s := &server{
		defaults: sessionDefaults{cwd: t.TempDir()},
		Registry: newSessionRegistry(), agentExecutions: store,
	}
	for name, body := range map[string]string{
		"missing": `{"clientSessionId":"worker-missing","provider":"codex","mode":"start"}`,
		"unknown": `{"clientSessionId":"worker-unknown","provider":"codex","mode":"start","invokerThreadRef":"not-a-thread"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleAgentSessionCreate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads", bytes.NewReader([]byte(body))))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
		})
	}
	if records := fixtureListSessions(store); len(records) != 0 {
		t.Fatalf("invalid invoker requests were persisted: %#v", records)
	}
}

func TestHumanCreatedSessionUsesUniversalHostedThreadAPI(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	provider := newFakeAppServer(t)
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "run-1", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	s := &server{
		defaults: sessionDefaults{cwd: t.TempDir()}, codex: provider.daemon(),
		spoolDir: t.TempDir(), Registry: newSessionRegistry(), agentExecutions: store, controlMetadata: control,
		queuedTurns: queue, sendAttempts: attempts, turns: turnStore,
	}

	created := httptest.NewRecorder()
	s.handleSessionCreate(created, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader([]byte(`{
	      "provider":"codex","mode":"start","presentationMode":"shared","serviceTier":"priority"
    }`))))
	if created.Code != http.StatusOK {
		t.Fatalf("New Window create status = %d, body %s", created.Code, created.Body.String())
	}
	var summary sessionSummary
	if err := json.Unmarshal(created.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.HostedThreadID == "" {
		t.Fatalf("human-created identity = %#v", summary)
	}
	if summary.Status != "starting" {
		t.Fatalf("New Window response status = %q, want async starting", summary.Status)
	}
	var runtimeRows int
	if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM hosted_execution_sessions WHERE hosted_thread_id=?`, summary.HostedThreadID).Scan(&runtimeRows); err != nil || runtimeRows != 1 {
		t.Fatalf("execution row = %d, err=%v", runtimeRows, err)
	}
	var providerName string
	if err := control.CanonicalDatabase().QueryRow(`SELECT provider FROM hosted_threads WHERE hosted_thread_id=?`, summary.HostedThreadID).Scan(&providerName); err != nil || providerName != "codex" {
		t.Fatalf("hosted identity = %q, err=%v", providerName, err)
	}
	record, ok := store.Session(summary.HostedThreadID)
	if !ok || record.ClientSessionID != "" || record.Provider != "codex" {
		t.Fatalf("human-created hosted record = %#v, present=%v", record, ok)
	}
	if assignment, err := control.CurrentOrchestrationAssignment(summary.HostedThreadID); err != nil || assignment != nil {
		t.Fatalf("human session assignment = %#v, err=%v", assignment, err)
	}
	listed := httptest.NewRecorder()
	s.handleAgentSessionsList(listed, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads", nil))
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(summary.HostedThreadID)) {
		t.Fatalf("hosted-thread list status = %d, body %s", listed.Code, listed.Body.String())
	}
	sess := s.LookupSession(summary.ID)
	t.Cleanup(sess.Close)
	deadline := time.Now().Add(time.Second)
	for sess.Summary().Status != "ready" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := sess.Summary().Status; got != "ready" {
		t.Fatalf("human-created session status = %q", got)
	}
	provider.Mu.Lock()
	startParams := append([]byte(nil), provider.StartParams...)
	provider.Mu.Unlock()
	if !bytes.Contains(startParams, []byte(`"serviceTier":"priority"`)) {
		t.Fatalf("thread/start params omitted priority: %s", startParams)
	}
	var desired string
	if err := control.CanonicalDatabase().QueryRow(`SELECT desired_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, summary.HostedThreadID, postureFieldServiceTier).Scan(&desired); err != nil || desired != metadataDispositionRequested {
		t.Fatalf("priority desired metadata = %q, err=%v", desired, err)
	}
	if resolved, ok := s.resolveHostedThreadRecord(sess.ThreadID()); !ok || resolved.ID != summary.HostedThreadID {
		t.Fatalf("resolve human-created native id = %#v, present=%v", resolved, ok)
	}
	provider.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "provider-turn-human"}})

	// Address the ordinary thread by its current Nano session id. The handler
	// resolves that reference to the same durable hostedThreadId used by an
	// API-created thread before creating the operation.
	sender := confirmedTestSender(t, s, "codex")
	body, err := json.Marshal(map[string]string{"prompt": "follow up", "clientActionId": "human-action-1", "senderThreadRef": sender})
	if err != nil {
		t.Fatal(err)
	}
	submitted := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(submitted, httptest.NewRequest(http.MethodPost,
		"/api/v1/hosted-threads/"+summary.ID+"/turns",
		bytes.NewReader(body)))
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("universal turn status = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var operation agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	latest, ok := fixtureWaitTurn(store, context.Background(), operation.ID, operation.Revision, time.Second)
	if !ok || latest.HostedThreadID != summary.HostedThreadID || latest.Status != agentTurnRunning {
		t.Fatalf("human-created operation = %#v", latest)
	}
	waitForTurnSendDone(t, sess, latest.TransportActionID)
}

func TestAPICreatedHostedThreadUsesOrdinarySessionControl(t *testing.T) {
	control, store := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(store, "parent-alias", "parent-request", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(parent.ID, "parent-nano", "parent-native", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := control.ClaimNativeID(parent.ID, "claude", "parent-native", false); err != nil {
		t.Fatal(err)
	}
	provider := newFakeAppServer(t)
	queue := mustQueuedTurnStore(t)
	attempts, err := newSendAttemptStore(queue.Database(), queue.Path(), "run-1", testPromptStore(t, queue.Database()))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	s := &server{
		defaults: sessionDefaults{cwd: t.TempDir()}, codex: provider.daemon(),
		spoolDir: t.TempDir(), Registry: newSessionRegistry(), agentExecutions: store, controlMetadata: control,
		queuedTurns: queue, sendAttempts: attempts, turns: turnStore,
	}

	created := httptest.NewRecorder()
	s.handleAgentSessionCreate(created, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads", bytes.NewReader([]byte(`{
      "clientSessionId":"api-created","provider":"codex","mode":"start","presentationMode":"shared",
      "invokerThreadRef":"parent-native"
    }`))))
	if created.Code != http.StatusCreated {
		t.Fatalf("hosted API create status = %d, body %s", created.Code, created.Body.String())
	}
	var hosted agentSessionView
	if err := json.Unmarshal(created.Body.Bytes(), &hosted); err != nil {
		t.Fatal(err)
	}
	if hosted.ID == "" || hosted.ID == hosted.ClientSessionID || hosted.ClientSessionID != "api-created" {
		t.Fatalf("API-created identity = %#v", hosted)
	}
	if hosted.InvokerHostedThreadID != parent.ID || hosted.InvokerNativeThreadID != "parent-native" || hosted.InvokerProvider != "claude" {
		t.Fatalf("normalized cross-provider invoker = %#v", hosted)
	}
	duplicate := httptest.NewRecorder()
	duplicateBody := []byte(`{
      "clientSessionId":"api-created","provider":"codex","mode":"start","presentationMode":"shared",
      "invokerThreadRef":"` + parent.ID + `"
    }`)
	s.handleAgentSessionCreate(duplicate, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads", bytes.NewReader(duplicateBody)))
	if duplicate.Code != http.StatusOK {
		t.Fatalf("normalized invoker retry status = %d, body %s", duplicate.Code, duplicate.Body.String())
	}
	var retried agentSessionView
	if err := json.Unmarshal(duplicate.Body.Bytes(), &retried); err != nil || retried.ID != hosted.ID {
		t.Fatalf("normalized invoker retry = %#v, err=%v", retried, err)
	}
	sess := s.SessionForHostedThread(hosted.ID)
	if sess == nil {
		t.Fatal("API-created hosted thread did not enter the ordinary session registry")
	}
	t.Cleanup(sess.Close)
	deadline := time.Now().Add(time.Second)
	for sess.Summary().Status != "ready" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if summary := sess.Summary(); summary.Status != "ready" || summary.HostedThreadID != hosted.ID || summary.InvokerHostedThreadID != parent.ID {
		t.Fatalf("ordinary session summary = %#v", summary)
	}
	provider.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "provider-turn-api"}})

	// The legacy browser/New Window control path accepts the API-created thread
	// because creation origin is not represented in the runtime session type.
	turn := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(turn, httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+sess.ID()+"/turn",
		bytes.NewReader([]byte(`{"text":"ordinary follow up","clientActionId":"browser-action-1"}`))))
	if turn.Code != http.StatusOK {
		t.Fatalf("ordinary session turn status = %d, body %s", turn.Code, turn.Body.String())
	}
}

func TestHostedThreadConversationDefaultsToMessagesAndResolvesCallerAlias(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, err := fixtureBeginSession(store, "reader-alias", "request-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	projection := newLiveProjection("codex", hosted.ID)
	sess := configuredFixtureSession(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID, Projection: projection}, "native-1", "ready", "", true)
	savedStore, err := history.Open(filepath.Join(t.TempDir(), "conversation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { savedStore.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := savedStore.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	seedSidebarThread(t, savedStore, root, "native-1", "", history.ThreadMain, time.Now(), []historyCourt{{final: true, text: "working on it"}, {tool: true, text: "secret command output"}})
	saved := newStoredCatalog(t, savedStore)
	s := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store, catalog: saved}, map[string]*session{"nano-1": sess})
	s.startLiveText(sess)

	read := func(path string) (liveProjectionSnapshot, int) {
		t.Helper()
		rec := httptest.NewRecorder()
		applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var snapshot liveProjectionSnapshot
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
		}
		return snapshot, rec.Code
	}

	messages, code := read("/api/v1/hosted-threads/reader-alias/conversation")
	if code != http.StatusOK {
		t.Fatalf("messages view status = %d", code)
	}
	if len(messages.Items) != 1 || messages.Items[0].Type != "agentMessage" || !messages.Runtime.TurnActive {
		t.Fatalf("messages view = %#v", messages)
	}
	if messages.Items[0].Text != "working on it" {
		t.Fatalf("messages view exposed a non-message row: %#v", messages.Items)
	}
	full, code := read("/api/v1/hosted-threads/" + hosted.ID + "/conversation?view=full")
	if code != http.StatusOK || len(full.Items) != 2 {
		t.Fatalf("full view status = %d, snapshot = %#v", code, full)
	}
	if full.Items[1].Text != "secret command output" {
		t.Fatalf("full view omitted the command row: %#v", full.Items)
	}
	_, code = read("/api/v1/hosted-threads/" + hosted.ID + "/conversation?view=unknown")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown view status = %d, want 400", code)
	}
}

// DELETE is destroy since the 2026-08-06 verb split: attachment forgotten,
// durable record and operations erased — the contract the docs always
// published. Close-meaning callers use POST {ref}/close (tests below).
func TestAgentSessionDeleteDestroysDurableRecord(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) { c.AnswerRest() })
	control, store := newTestHostedControl(t)
	hosted, _, _ := fixtureBeginSession(store, "caller-session", "request-hash", "claude", presentationBackground)
	_ = store.BindSession(hosted.ID, "old-nano", "native-1", "ready", presentationBackground)
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt-hash")
	if _, err := control.AppendEvent(metadataEvent{HostedThreadID: hosted.ID, Field: postureFieldModel, Value: "opus", Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive}); err != nil {
		t.Fatal(err)
	}
	attachments := newTestStore(t)
	attachments.Remember(attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1", HostedThreadID: hosted.ID,
		ShimSessionID:    "shim-delete-test",
		PresentationMode: presentationBackground, AttachedAt: time.Now().Format(time.RFC3339Nano),
	})
	s := &server{Registry: newSessionRegistry(), agentExecutions: store, attachments: attachments, controlMetadata: control}
	s.defaults.claudeShimWS = shim.WSURL()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/agent-sessions/caller-session", nil)
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.attachmentFor("claude", "native-1"); ok {
		t.Fatal("detached attachment survived delete")
	}
	if _, ok := store.Session(hosted.ID); ok {
		t.Fatal("durable record survived delete — DELETE must destroy, not close")
	}
	if _, ok := store.Turn(op.ID); ok {
		t.Fatal("operation record survived its thread's delete")
	}
	var current, events int
	if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_current WHERE hosted_thread_id=?`, hosted.ID).Scan(&current); err != nil || current != 0 {
		t.Fatalf("canonical current survived destroy: count=%d err=%v", current, err)
	}
	if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=?`, hosted.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("canonical events survived destroy: count=%d err=%v", events, err)
	}
}

// POST {ref}/close carries the old DELETE meaning truthfully: attachment
// forgotten, record retained as exited — and the detached-branch settlement
// bug is fixed: a non-terminal operation on an unattached thread settles as
// interrupted instead of dangling forever.
func TestAgentSessionCloseSettlesDetachedOperations(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) { c.AnswerRest() })
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "caller-session", "request-hash", "claude", presentationBackground)
	_ = store.BindSession(hosted.ID, "old-nano", "native-1", "ready", presentationBackground)
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt-hash")
	attachments := newTestStore(t)
	attachments.Remember(attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1", HostedThreadID: hosted.ID,
		ShimSessionID:    "shim-close-test",
		PresentationMode: presentationBackground, AttachedAt: time.Now().Format(time.RFC3339Nano),
	})
	s := &server{Registry: newSessionRegistry(), agentExecutions: store, attachments: attachments}
	s.defaults.claudeShimWS = shim.WSURL()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/caller-session/close", nil)
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.attachmentFor("claude", "native-1"); ok {
		t.Fatal("attachment survived close")
	}
	session, ok := store.Session(hosted.ID)
	if !ok || session.Status != sessionStatusExited {
		t.Fatalf("closed record = %+v ok=%v, want retained with status exited", session, ok)
	}
	turn, _ := store.Turn(op.ID)
	if turn.Status != agentTurnInterrupted {
		t.Fatalf("detached-thread operation status = %q, want interrupted, not a dangling %q", turn.Status, op.Status)
	}
	if turn.Error == nil || turn.Error.Code != "session_closed" {
		t.Fatalf("settled operation error = %+v, want session_closed evidence", turn.Error)
	}
}

// Split-brain check (slice-1 item 7): the earlier live incident was a
// hosted-thread summary that looked finished while a turn-operation record
// still knew a turn was open. After any lifecycle verb, a record claiming
// exited must never coexist with an operation lacking terminal evidence —
// across every non-terminal state, including waiting_for_input.
func TestLifecycleVerbsKeepSessionAndOperationsAgreeing(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "caller-session", "request-hash", "claude", presentationBackground)
	_ = store.BindSession(hosted.ID, "old-nano", "native-1", "ready", presentationBackground)
	queued, _, _ := fixtureBeginTurn(store, hosted.ID, "action-q", "hash-q")
	_ = store.MarkTurnDelivery(queued.ID, "", agentTurnQueued, nil)
	waiting, _, _ := fixtureBeginTurn(store, hosted.ID, "action-w", "hash-w")
	_ = store.MarkTurnDelivery(waiting.ID, "claude:1", agentTurnRunning, nil)
	_ = store.MarkWaiting(hosted.ID, "claude:1", true)

	s := &server{Registry: newSessionRegistry(), agentExecutions: store, attachments: newTestStore(t)}
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/caller-session/close", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body %s", rec.Code, rec.Body.String())
	}

	session, ok := store.Session(hosted.ID)
	if !ok || session.Status != sessionStatusExited {
		t.Fatalf("record = %+v ok=%v, want exited", session, ok)
	}
	if store.HasNonTerminalTurns(hosted.ID) {
		q, _ := store.Turn(queued.ID)
		w, _ := store.Turn(waiting.ID)
		t.Fatalf("exited record still owns open operations: queued=%q waiting=%q — the split-brain seam", q.Status, w.Status)
	}
	for _, op := range []agentTurnRecord{queued, waiting} {
		if settled, _ := store.Turn(op.ID); settled.Status != agentTurnInterrupted {
			t.Fatalf("operation %s status = %q, want interrupted", op.ID, settled.Status)
		}
	}
}

// Claude detach keeps the durable thread, but now shares the confirmed stop
// path. An open operation still makes detach refuse.
func TestAgentSessionDetach(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) { c.AnswerRest() })
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "caller-session", "request-hash", "claude", presentationBackground)
	_ = store.BindSession(hosted.ID, "old-nano", "native-1", "ready", presentationBackground)
	attachments := newTestStore(t)
	attachments.Remember(attachmentRecord{
		Provider: "claude", NativeThreadID: "native-1", HostedThreadID: hosted.ID,
		ShimSessionID:    "shim-detach-test",
		PresentationMode: presentationBackground, AttachedAt: time.Now().Format(time.RFC3339Nano),
	})
	s := &server{Registry: newSessionRegistry(), agentExecutions: store, attachments: attachments}
	s.defaults.claudeShimWS = shim.WSURL()

	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt-hash")
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/caller-session/detach", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("detach with an open operation = %d, want 409", rec.Code)
	}

	_ = store.MarkSessionTurns(hosted.ID, agentTurnCompleted, nil)
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/caller-session/detach", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("detach status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.attachmentFor("claude", "native-1"); ok {
		t.Fatal("Claude detach retained a dead shim attachment")
	}
	session, ok := store.Session(hosted.ID)
	if !ok || session.Status != sessionStatusExited {
		t.Fatalf("closed record = %+v ok=%v, want retained with status exited", session, ok)
	}
	turn, _ := store.Turn(op.ID)
	if turn.Status != agentTurnCompleted {
		t.Fatalf("terminal operation status = %q, want untouched by detach", turn.Status)
	}
}

func TestAgentClientSessionIDIsPathSafe(t *testing.T) {
	for _, value := range []string{"worker-1", "team.alpha:task_2@local"} {
		if !validAgentClientID(value) {
			t.Fatalf("safe id %q rejected", value)
		}
	}
	for _, value := range []string{"", "worker/1", "worker 1", "café"} {
		if validAgentClientID(value) {
			t.Fatalf("unsafe id %q accepted", value)
		}
	}
}

func TestAgentTurnViewScopesInteractionsToTheOpenTurn(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "caller-session", "request-hash", "codex", presentationShared)
	_ = store.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared)
	old, _, _ := fixtureBeginTurn(store, hosted.ID, "old-action", "old-prompt")
	_ = store.MarkTurnDelivery(old.ID, "turn-old", agentTurnRunning, nil)
	_ = store.MarkTurnTerminal("codex", "native-1", "turn-old", agentTurnCompleted, nil, "done", "item-old")
	current, _, _ := fixtureBeginTurn(store, hosted.ID, "current-action", "current-prompt")
	_ = store.MarkTurnDelivery(current.ID, "turn-current", agentTurnWaiting, nil)
	f := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID}, "native-1", "")
	sess := f.session
	f.codex.EmitNotification(map[string]any{"id": 1, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "native-1", "turnId": "turn-current"}})
	waitForClaude(t, "the current turn's interaction", func() bool { return len(sess.PendingInteractionList()) == 1 })

	s := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store}, map[string]*session{"nano-1": sess})
	oldRecord, _ := store.Turn(old.ID)
	currentRecord, _ := store.Turn(current.ID)
	oldView := s.agentTurnView(oldRecord)
	currentView := s.agentTurnView(currentRecord)
	if len(oldView.Interactions) != 0 || len(currentView.Interactions) != 1 || currentView.Interactions[0].ID != "1" {
		t.Fatalf("interaction scopes = old:%#v current:%#v", oldView.Interactions, currentView.Interactions)
	}
}
func TestHostedTurnQueuesBehindActiveTurnAndDrains(t *testing.T) {
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, _ := fixtureBeginSession(executions, "queue-active", "request-hash", "codex", presentationBackground)
	_ = executions.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationBackground)
	var providerCalls atomic.Int32
	var sess *session
	runtimeFixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "native-1", "", func(f *providerRuntimeFixture) {
		f.send = func(context.Context, string) (json.RawMessage, error) {
			providerCalls.Add(1)
			return json.RawMessage(`{"turn":{"id":"provider-queued-turn"}}`), nil
		}
	})
	sess = runtimeFixture.session
	runtimeFixture.activate(t)
	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{"nano-1": sess})

	submitted := submitHostedTurn(t, s, hosted.ID, "queued-action", "send after the active turn")
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("queue status = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var queued agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.Status != agentTurnQueued || queued.QueuePosition != 1 || queued.Terminal {
		t.Fatalf("queued operation = %#v", queued)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls before drain = %d", providerCalls.Load())
	}

	runtimeFixture.finishTurn(t)
	latest := waitForAgentTurnStatus(t, executions, queued.ID, agentTurnRunning)
	if latest.ProviderTurnID != "provider-queued-turn" {
		t.Fatalf("drained operation = %#v", latest)
	}
	waitForTurnSendDone(t, sess, latest.TransportActionID)
	if providerCalls.Load() != 1 {
		t.Fatalf("provider calls after drain = %d", providerCalls.Load())
	}
}

func TestHostedClaudeFollowupIgnoresStaleRunningOperationAfterRestart(t *testing.T) {
	dir := t.TempDir()
	executionPath := filepath.Join(dir, "executions.sqlite")
	first, err := newAgentExecutionStore(executionPath, "run-before-restart")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, err := fixtureBeginSession(first, "claude-restart-stale", "request-hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.BindSession(hosted.ID, "nano-before-restart", "native-restart-stale", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	stale, _, err := fixtureBeginTurn(first, hosted.ID, "stale-first-action", "stale-prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.MarkTurnDelivery(stale.ID, "claude:stale", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := first.Control().Close(); err != nil {
		t.Fatal(err)
	}

	// A restart preserves a provider-bound in_progress operation for audit. It
	// must not become the live admission gate once the reattached runtime is
	// ready and has no active provider turn.
	executions, err := newAgentExecutionStore(executionPath, "run-after-restart")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = executions.Control().Close() })
	if err := executions.BindSession(hosted.ID, "nano-after-restart", "native-restart-stale", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-after-restart")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	var providerCalls atomic.Int32
	var sess *session
	runtimeFixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-after-restart", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "native-restart-stale", "", func(f *providerRuntimeFixture) {
		f.send = func(context.Context, string) (json.RawMessage, error) {
			providerCalls.Add(1)
			return json.RawMessage(`{"turn":{"id":"claude:fresh"}}`), nil
		}
	})
	sess = runtimeFixture.session
	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{sess.ID(): sess})

	submitted := submitHostedTurn(t, s, hosted.ID, "fresh-after-restart", "dispatch after restart")
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("follow-up admission = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var operation agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	running := waitForAgentTurnStatus(t, executions, operation.ID, agentTurnRunning)
	if running.ProviderTurnID != "claude:fresh" || providerCalls.Load() != 1 {
		t.Fatalf("follow-up did not reach Claude exactly once: operation=%#v calls=%d", running, providerCalls.Load())
	}
	if prior, ok := executions.Turn(stale.ID); !ok || prior.Status != agentTurnRunning || prior.ProviderTurnID != "claude:stale" {
		t.Fatalf("restart audit operation changed during admission: %#v, present=%v", prior, ok)
	}
}

func TestHostedClaudeFollowupQueuesWhileFirstTurnRunsThenDrains(t *testing.T) {
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, err := fixtureBeginSession(executions, "claude-followup-active", "request-hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "nano-claude-active", "native-claude-active", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	var providerCalls atomic.Int32
	var sess *session
	runtimeFixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-claude-active", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "native-claude-active", "", func(f *providerRuntimeFixture) {
		f.send = func(context.Context, string) (json.RawMessage, error) {
			providerCalls.Add(1)
			return json.RawMessage(`{"turn":{"id":"claude:followup"}}`), nil
		}
	})
	sess = runtimeFixture.session
	runtimeFixture.activate(t)
	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{sess.ID(): sess})

	submitted := submitHostedTurn(t, s, hosted.ID, "claude-followup", "queue behind the first turn")
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("follow-up admission = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var queued agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.Status != agentTurnQueued || queued.QueuePosition != 1 || providerCalls.Load() != 0 {
		t.Fatalf("follow-up did not wait behind active turn: operation=%#v calls=%d", queued, providerCalls.Load())
	}

	runtimeFixture.finishTurn(t)
	running := waitForAgentTurnStatus(t, executions, queued.ID, agentTurnRunning)
	if running.ProviderTurnID != "claude:followup" || providerCalls.Load() != 1 {
		t.Fatalf("follow-up did not drain after completion: operation=%#v calls=%d", running, providerCalls.Load())
	}
}

func TestHostedClaudeFollowupDispatchesAfterAcceptedFirstPrompt(t *testing.T) {
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, err := fixtureBeginSession(executions, "claude-accepted-first", "request-hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "nano-accepted-first", "native-accepted-first", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	var providerCalls atomic.Int32
	var sess *session
	runtimeFixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-accepted-first", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "native-accepted-first", "", func(f *providerRuntimeFixture) {
		f.send = func(context.Context, string) (json.RawMessage, error) {
			n := providerCalls.Add(1)
			return json.RawMessage(`{"turn":{"id":"claude:` + strconv.Itoa(int(n)) + `"}}`), nil
		}
	})
	sess = runtimeFixture.session
	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{sess.ID(): sess})

	first := submitHostedTurn(t, s, hosted.ID, "first", "prompt one")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first admission = %d, body %s", first.Code, first.Body.String())
	}
	var firstView agentTurnView
	if err := json.Unmarshal(first.Body.Bytes(), &firstView); err != nil {
		t.Fatal(err)
	}
	running := waitForAgentTurnStatus(t, executions, firstView.ID, agentTurnRunning)
	if running.ProviderTurnID != "claude:1" {
		t.Fatalf("first did not reach Claude: %#v", running)
	}
	if err := executions.MarkTurnTerminal("claude", "native-accepted-first", "claude:1", agentTurnCompleted, nil, "done", "item-1"); err != nil {
		t.Fatal(err)
	}
	runtimeFixture.finishTurn(t)
	record, _, err := turnStore.Get(context.Background(), running.TransportActionID)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := queue.Get(running.TransportActionID)
	attempt, _ := attempts.Get(running.TransportActionID)
	if row.State != submissionAccepted || record.ClaimToken != "" || attempt.Confirmed || attempt.ProviderTerminalStatus != "" {
		t.Fatalf("first accepted prompt retained dispatch ownership: row=%#v record=%#v attempt=%#v", row, record, attempt)
	}
	second := submitHostedTurn(t, s, hosted.ID, "second", "prompt two")
	if second.Code != http.StatusAccepted {
		t.Fatalf("second admission = %d, body %s", second.Code, second.Body.String())
	}
	var secondView agentTurnView
	if err := json.Unmarshal(second.Body.Bytes(), &secondView); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if op, ok := executions.Turn(secondView.ID); ok && op.Status == agentTurnRunning {
			if op.ProviderTurnID != "claude:2" || providerCalls.Load() != 2 {
				t.Fatalf("follow-up dispatch = %#v calls=%d", op, providerCalls.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	op, _ := executions.Turn(secondView.ID)
	row2, _ := queue.Get(op.TransportActionID)
	t.Fatalf("prompt 2 never dispatched: op.status=%s row.state=%s calls=%d", op.Status, row2.State, providerCalls.Load())
}

func TestHostedClaudeAcceptedPromptDoesNotHoldFollowupClaim(t *testing.T) {
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, err := fixtureBeginSession(executions, "claude-held-mismatch", "request-hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "nano-held-mismatch", "native-held-mismatch", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	operation, _, err := fixtureBeginTurn(executions, hosted.ID, "held-first", "held-first-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.MarkTurnDelivery(operation.ID, "claude:other", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := executions.MarkTurnTerminal("claude", "native-held-mismatch", "claude:other", agentTurnCompleted, nil, "other result", "other-item"); err != nil {
		t.Fatal(err)
	}
	seedAcceptedTransactionalTurn(t, turnStore, hosted.ID, model.ProviderClaude, "native-held-mismatch", operation.TransportActionID, "claude:1", "held prompt")
	var providerCalls atomic.Int32
	var sess *session
	runtimeFixture := newProviderRuntimeFixture(t, sessionConfig{ID: "nano-held-mismatch", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "native-held-mismatch", "", func(f *providerRuntimeFixture) {
		f.send = func(context.Context, string) (json.RawMessage, error) {
			providerCalls.Add(1)
			return json.RawMessage(`{"turn":{"id":"claude:2"}}`), nil
		}
	})
	sess = runtimeFixture.session
	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{sess.ID(): sess})

	second := submitHostedTurn(t, s, hosted.ID, "second", "prompt two")
	if second.Code != http.StatusAccepted {
		t.Fatalf("follow-up admission = %d, body %s", second.Code, second.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for providerCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("accepted prompt blocked follow-up: calls=%d", providerCalls.Load())
	}
	if accepted, _, err := turnStore.Get(context.Background(), operation.TransportActionID); err != nil || accepted.Outbox.State != turns.Accepted || accepted.ClaimToken != "" {
		t.Fatalf("accepted prompt retained claim: record=%#v err=%v", accepted, err)
	}
}

func TestHostedTurnQueuesWhileSessionStartsWithoutNativeIdentity(t *testing.T) {
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, _ := fixtureBeginSession(executions, "queue-starting", "request-hash", "claude", presentationBackground)
	var providerCalls atomic.Int32
	sess := configuredFixtureSession(t, sessionConfig{ID: "nano-1", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, Projection: newLiveProjection("claude"), AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "", "starting", "")
	shim := newFakeShim(t, func(c *shimConn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "native-after-start"})
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			if call.Method == shimMethodInput {
				var params struct {
					Line string `json:"line"`
				}
				_ = json.Unmarshal(call.Params, &params)
				var input struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(params.Line), &input)
				if input.Type == "user" {
					providerCalls.Add(1)
					c.Reply(call, map[string]any{"turnId": "claude:1"})
					continue
				}
			}
			c.Reply(call, map[string]any{})
		}
	})

	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{"nano-1": sess})

	submitted := submitHostedTurn(t, s, hosted.ID, "startup-action", "send once ready")
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("startup queue status = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var queued agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.Status != agentTurnQueued || queued.QueuePosition != 1 {
		t.Fatalf("startup queued operation = %#v", queued)
	}
	queuedOperation, _ := executions.Turn(queued.ID)
	queuedRecord, ok := queue.Get(queuedOperation.TransportActionID)
	if !ok || queuedRecord.HostedThreadID != hosted.ID || queuedRecord.NativeThreadID != "" {
		t.Fatalf("pre-identity queue record = %#v, present=%v", queuedRecord, ok)
	}

	sess.ReserveNativeID("native-after-start")
	if err := sess.BeginProviderSpawn(providerRequest{Provider: "claude", ClaudeShimWS: shim.WSURL(), ClaudeBin: "/fake/claude", Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)

	latest := waitForAgentTurnStatus(t, executions, queued.ID, agentTurnRunning)
	if latest.ProviderTurnID != "claude:1" {
		t.Fatalf("startup drain = %#v", latest)
	}
	waitForTurnSendDone(t, sess, latest.TransportActionID)
	if providerCalls.Load() != 1 {
		t.Fatalf("startup provider calls = %d", providerCalls.Load())
	}
}

func TestFreshClaudeTurnBindsBeforeFastNativeCompletion(t *testing.T) {
	dir := t.TempDir()
	// The child's init records its handle's resolution, which needs the
	// current control schema.
	_, executions := newTestHostedControl(t)
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	hosted, _, err := fixtureBeginSession(executions, "claude-fast-start", "request-hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "nano-1", "cs_fast_start", "starting", presentationBackground); err != nil {
		t.Fatal(err)
	}
	projection := newLiveProjection("claude")
	sess := configuredFixtureSession(t, sessionConfig{ID: "nano-1", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, Projection: projection, AgentExecutions: executions, Metadata: executions.Control(), Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "cs_fast_start", "starting", "")
	observer := observeProjection(t, sess)

	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{"nano-1": sess})

	submitted := submitHostedTurn(t, s, hosted.ID, "claude-fast-action", "finish before the input ack returns")
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("startup queue status = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var operation agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Status != agentTurnQueued {
		t.Fatalf("startup operation = %#v", operation)
	}
	queuedOperation, ok := executions.Turn(operation.ID)
	if !ok {
		t.Fatal("queued public operation disappeared")
	}
	startFastClaudeFixture(t, sess, true)
	waitForTurnSendDone(t, sess, queuedOperation.TransportActionID)

	// The operation binds to the turn and native the child announced. It
	// finishes only on its own prompt's saved proof, which this fixture never
	// writes, so it stays running.
	waitForCatalogWithin(t, 5*time.Second, func() bool {
		turns := observer.Snapshot().Turns
		return len(turns) == 1 && turns[0].Status == agentTurnCompleted
	})
	latest, _ := executions.Turn(operation.ID)
	if latest.ProviderTurnID != "shim-turn-fast-1" || latest.NativeThreadID != "native-fast-start" || latest.Status != agentTurnRunning {
		t.Fatalf("bound public operation = %#v", latest)
	}
	snapshot := observer.Snapshot()
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "shim-turn-fast-1" || snapshot.Turns[0].Status != agentTurnCompleted {
		t.Fatalf("Claude projection turns = %#v", snapshot.Turns)
	}
	if snapshot.Turns[0].DurationMS < 0 {
		t.Fatalf("Claude projection duration = %dms", snapshot.Turns[0].DurationMS)
	}
	seedAcceptedTransactionalTurn(t, sess.PromptStore(), "unrelated-owner", model.ProviderClaude, "native-fast-start", "unrelated-action", "claude:9", "leave me alone")

	(&server{}).confirmDurableItems(context.Background(), sess)
	if accepted, ok := queue.Get(queuedOperation.TransportActionID); !ok || accepted.State != submissionAccepted || accepted.Text == "" {
		t.Fatalf("completed turn changed accepted prompt = %#v, present=%v", accepted, ok)
	}
	attempt, ok := attempts.Get(queuedOperation.TransportActionID)
	if !ok || attempt.Confirmed || attempt.ProviderTurnID != "shim-turn-fast-1" || attempt.NativeThreadID != "native-fast-start" {
		t.Fatalf("completed turn changed accepted attempt = %#v, present=%v", attempt, ok)
	}
	if unrelated, ok := queue.Get("unrelated-action"); !ok || unrelated.ProviderTurnID != "claude:9" {
		t.Fatalf("unrelated Claude submission changed = %#v, present=%v", unrelated, ok)
	}
	if unrelated, ok := attempts.Get("unrelated-action"); !ok || unrelated.Confirmed {
		t.Fatalf("unrelated Claude send attempt changed = %#v, present=%v", unrelated, ok)
	}
	followup := submitHostedTurn(t, s, hosted.ID, "claude-fast-followup", "dispatch after the confirmed first turn")
	if followup.Code != http.StatusAccepted {
		t.Fatalf("follow-up admission = %d, body %s", followup.Code, followup.Body.String())
	}
	var followupView agentTurnView
	if err := json.Unmarshal(followup.Body.Bytes(), &followupView); err != nil {
		t.Fatal(err)
	}
	followupRunning := waitForAgentTurnStatus(t, executions, followupView.ID, agentTurnRunning)
	if followupRunning.ProviderTurnID == "" {
		t.Fatalf("follow-up never reached Claude = %#v", followupRunning)
	}
}

func TestFreshClaudeDurableCompletionDoesNotSettleAcceptedPrompt(t *testing.T) {
	dir := t.TempDir()
	executions, err := newAgentExecutionStore(filepath.Join(dir, "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	migrateTestControl(t, executions.Control())
	hosted, _, err := fixtureBeginSession(executions, "claude-output-first", "request-hash", "claude", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	// Real shim handles use cs_; only that provisional namespace may advance
	// to the native ID on init. A different native thread must remain a conflict.
	if err := executions.BindSession(hosted.ID, "nano-output-first", "cs_output_first", "starting", presentationBackground); err != nil {
		t.Fatal(err)
	}
	projection := newLiveProjection("claude")
	sess := configuredFixtureSession(t, sessionConfig{ID: "nano-output-first", Provider: "claude", HostedThreadID: hosted.ID, PresentationMode: presentationBackground, Projection: projection, AgentExecutions: executions, Metadata: executions.Control(), Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "cs_output_first", "starting", "")
	observer := observeProjection(t, sess)

	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{sess.ID(): sess})

	submitted := submitHostedTurn(t, s, hosted.ID, "claude-output-first-action", "finish without a result line")
	if submitted.Code != http.StatusAccepted {
		t.Fatalf("startup queue status = %d, body %s", submitted.Code, submitted.Body.String())
	}
	var operation agentTurnView
	if err := json.Unmarshal(submitted.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	queuedOperation, ok := executions.Turn(operation.ID)
	if !ok || operation.Status != agentTurnQueued {
		t.Fatalf("startup operation = %#v, durable=%#v, present=%v", operation, queuedOperation, ok)
	}
	startFastClaudeFixture(t, sess, false)
	waitForTurnSendDone(t, sess, queuedOperation.TransportActionID)

	running, ok := executions.Turn(operation.ID)
	if !ok || running.Status != agentTurnRunning || running.ProviderTurnID != "shim-turn-fast-1" || running.NativeThreadID != "native-fast-start" {
		t.Fatalf("output-first public operation = %#v, error=%+v, present=%v", running, running.Error, ok)
	}
	exactSubmission, ok := queue.Get(queuedOperation.TransportActionID)
	if !ok || exactSubmission.State != submissionAccepted || exactSubmission.ProviderTurnID != "shim-turn-fast-1" || exactSubmission.NativeThreadID != "native-fast-start" {
		t.Fatalf("output-first queue row = %#v, present=%v", exactSubmission, ok)
	}
	exactAttempt, ok := attempts.Get(queuedOperation.TransportActionID)
	if !ok || exactAttempt.Status != sendAttemptAccepted || exactAttempt.Confirmed || exactAttempt.ProviderTurnID != "shim-turn-fast-1" {
		t.Fatalf("output-first send attempt = %#v, present=%v", exactAttempt, ok)
	}

	// Delivery completion closes before every projection callback from the same
	// provider response has necessarily applied. This test-only 50ms headroom
	// lets the synthetic asynchronous callback queue drain; production has no timer.
	time.Sleep(50 * time.Millisecond)
	startedDeadline := time.Now().Add(time.Second)
	var snapshot liveProjectionSnapshot
	for time.Now().Before(startedDeadline) {
		snapshot = observer.Snapshot()
		if len(snapshot.Turns) == 1 && snapshot.Turns[0].StartedAtMs != 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "shim-turn-fast-1" || snapshot.Turns[0].Status != "in_progress" {
		t.Fatalf("projection before durable completion = %#v", snapshot.Turns)
	}
	if snapshot.Turns[0].FinalItemID != "" {
		t.Fatalf("result-free projection claimed a terminal item: %#v", snapshot.Turns[0])
	}
	seedAcceptedTransactionalTurn(t, sess.PromptStore(), "unrelated-owner", model.ProviderClaude, "native-fast-start", "unrelated-output-first", "claude:9", "leave me alone")

	s.confirmDurableItems(context.Background(), sess)
	if current, _ := executions.Turn(operation.ID); current.Status != agentTurnRunning {
		t.Fatalf("incomplete durable turn settled operation: %#v", current)
	}
	if attempt, _ := attempts.Get(queuedOperation.TransportActionID); attempt.Confirmed {
		t.Fatalf("incomplete durable turn confirmed send attempt: %#v", attempt)
	}
	if _, ok := queue.Get(queuedOperation.TransportActionID); !ok {
		t.Fatal("incomplete durable turn scrubbed exact queue row")
	}
	completed, ok := executions.Turn(operation.ID)
	if !ok || completed.Status != agentTurnRunning || completed.ProviderTurnID != "shim-turn-fast-1" {
		t.Fatalf("durable file evidence changed live operation = %#v, present=%v", completed, ok)
	}
	if accepted, ok := queue.Get(queuedOperation.TransportActionID); !ok || accepted.State != submissionAccepted || accepted.Text == "" {
		t.Fatalf("durable file evidence changed accepted prompt = %#v, present=%v", accepted, ok)
	}
	exactAttempt, ok = attempts.Get(queuedOperation.TransportActionID)
	if !ok || exactAttempt.Confirmed || exactAttempt.ProviderTurnID != "shim-turn-fast-1" || exactAttempt.NativeThreadID != "native-fast-start" {
		t.Fatalf("durable file evidence changed accepted attempt = %#v, present=%v", exactAttempt, ok)
	}
	if unrelated, ok := queue.Get("unrelated-output-first"); !ok || unrelated.ProviderTurnID != "claude:9" {
		t.Fatalf("unrelated Claude submission changed = %#v, present=%v", unrelated, ok)
	}
	if unrelated, ok := attempts.Get("unrelated-output-first"); !ok || unrelated.Confirmed {
		t.Fatalf("unrelated Claude send attempt changed = %#v, present=%v", unrelated, ok)
	}
	// Claude saved turns share no id with the live turn, so a catalog change
	// never settles it; only the live result does.
	snapshot = observer.Snapshot()
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].ID != "shim-turn-fast-1" || snapshot.Turns[0].Status != "in_progress" {
		t.Fatalf("a catalog change settled the live Claude turn: %#v", snapshot.Turns)
	}
}

// The peer sends the native identity and result before acknowledging input,
// exposing the same early-completion race across the real shim transport.
func startFastClaudeFixture(t *testing.T, sess *session, includeResult bool) {
	t.Helper()
	f := &providerRuntimeFixture{session: sess}
	provisional := sess.ThreadID()
	sess.ReserveNativeID("")
	base := time.Now()
	ordinal := 0
	shim := newFakeShim(t, func(c *shimConn) {
		f.claude = c
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": provisional})
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			if call.Method != shimMethodInput {
				c.Reply(call, map[string]any{})
				continue
			}
			var params struct {
				Line string `json:"line"`
			}
			_ = json.Unmarshal(call.Params, &params)
			var user struct {
				Type string `json:"type"`
				UUID string `json:"uuid"`
			}
			_ = json.Unmarshal([]byte(params.Line), &user)
			if user.Type != "user" {
				c.Reply(call, map[string]any{})
				continue
			}
			ordinal++
			turn := fmt.Sprintf("shim-turn-fast-%d", ordinal)
			lines := []struct {
				at   time.Time
				line string
			}{
				{base.Add(time.Millisecond), `{"type":"system","subtype":"init","session_id":"native-fast-start","model":"claude-test","permissionMode":"bypassPermissions"}`},
				{base.Add(3 * time.Millisecond), `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg-fast"}}}`},
				{base.Add(4 * time.Millisecond), `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`},
				{base.Add(5 * time.Millisecond), `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}}`},
				{base.Add(6 * time.Millisecond), `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`},
			}
			if includeResult {
				lines = append(lines, struct {
					at   time.Time
					line string
				}{base.Add(2 * time.Millisecond), `{"type":"result","subtype":"success","is_error":false,"result":"done","user_message_uuid":"` + user.UUID + `"}`})
			}
			for _, entry := range lines {
				f.emitAt(t, []byte(entry.line), entry.at, turn)
			}
			c.Reply(call, map[string]any{"turnId": turn})
		}
	})
	if err := sess.BeginProviderSpawn(providerRequest{Provider: "claude", ClaudeShimWS: shim.WSURL(), ClaudeBin: "/fake/claude", Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
	t.Cleanup(sess.Close)
}

func TestQueuedHostedTurnReportsPositionAndCanBeCanceled(t *testing.T) {
	dir := t.TempDir()
	// One threads database, as in Nano: a cancel ends the prompt and its
	// operation in one transaction.
	stores, err := openThreadRuntimeStores(context.Background(), dir, filepath.Join(dir, "turn-completed.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.control.Close() })
	executions, attempts, queue, turnStore := stores.executions, stores.attempts, stores.queue, stores.turns
	hosted, _, _ := fixtureBeginSession(executions, "queue-cancel", "request-hash", "codex", presentationShared)
	_ = executions.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared)
	sess := configuredFixtureSession(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID, PresentationMode: presentationShared, AgentExecutions: executions, Turns: turnStore, SendAttempts: attempts, QueuedTurns: queue}, "native-1", "ready", "", true)
	s := testServerWithSessions(&server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}, map[string]*session{"nano-1": sess})

	firstResponse := submitHostedTurn(t, s, hosted.ID, "queue-first", "first")
	secondResponse := submitHostedTurn(t, s, hosted.ID, "queue-second", "second")
	var first, second agentTurnView
	_ = json.Unmarshal(firstResponse.Body.Bytes(), &first)
	_ = json.Unmarshal(secondResponse.Body.Bytes(), &second)
	if first.QueuePosition != 1 || second.QueuePosition != 2 {
		t.Fatalf("queue positions = first:%#v second:%#v", first, second)
	}

	// Cancellation addresses the durable queue directly when Nano is between
	// attachments, so callers do not need a process-local session id.
	s.Registry = newSessionRegistry()
	canceled := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(canceled, httptest.NewRequest(http.MethodDelete, "/api/v1/turn-operations/"+second.ID, nil))
	if canceled.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, body %s", canceled.Code, canceled.Body.String())
	}
	var canceledView agentTurnView
	_ = json.Unmarshal(canceled.Body.Bytes(), &canceledView)
	if canceledView.Status != agentTurnInterrupted || !canceledView.Terminal || canceledView.QueuePosition != 0 {
		t.Fatalf("canceled operation = %#v", canceledView)
	}
	firstRecord, _ := executions.Turn(first.ID)
	if got := s.agentTurnView(firstRecord).QueuePosition; got != 1 {
		t.Fatalf("first queue position after cancel = %d", got)
	}
}

func submitHostedTurn(t *testing.T, s *server, threadRef, actionID, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]string{"clientActionId": actionID, "prompt": prompt, "senderThreadRef": confirmedTestSender(t, s, "codex")}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/hosted-threads/"+threadRef+"/turns", bytes.NewReader(encoded)))
	return rec
}

// confirmedTestSender gives public-route tests a sender that can satisfy the
// same admission contract as production callers, independently of a target
// that may be starting, detached, or otherwise identity-incomplete.
func confirmedTestSender(t *testing.T, s *server, provider string) string {
	t.Helper()
	id := "test-sender-" + hostedstore.AgentHash([]byte(t.Name()))[:16]
	if s.catalog == nil {
		s.catalog = fixtureNamedCatalog(t, provider, "native-"+id, "API test sender")
	}
	sender, _, err := fixtureBeginSession(s.agentExecutions, id, id, provider, presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.agentExecutions.BindSession(sender.ID, "nano-"+id, "native-"+id, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	return sender.ID
}

func waitForAgentTurnStatus(t *testing.T, store *agentExecutionStore, operationID, want string) agentTurnRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	revision := int64(0)
	for {
		record, ok := fixtureWaitTurn(store, ctx, operationID, revision, time.Second)
		if !ok {
			t.Fatalf("operation %s disappeared waiting for %s", operationID, want)
		}
		if record.Status == want {
			return record
		}
		if record.Revision <= revision {
			t.Fatalf("operation %s stayed %s waiting for %s", operationID, record.Status, want)
		}
		revision = record.Revision
	}
}

var waitForTurnSendDone = waitTurnSendDone

// PUT {threadRef}/presentation: explicit-mode validation, live-session sync,
// and idempotency. The route resolves refs the same way as every scoped verb,
// so a nano session id addresses the thread too.
func TestHostedThreadPresentationEndpoint(t *testing.T) {
	store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "executions.sqlite"), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, _ := fixtureBeginSession(store, "caller-1", "request-hash", "codex", presentationShared)
	_ = store.BindSession(hosted.ID, "nano-1", "codex-native", "ready", presentationShared)
	sess := configuredFixtureSession(t, sessionConfig{ID: "nano-1", Provider: "codex", HostedThreadID: hosted.ID, PresentationMode: presentationShared, CreatedAt: time.Now(), Projection: newLiveProjection("codex"), AgentExecutions: store, QueuedTurns: mustQueuedTurnStore(t)}, "codex-native", "ready", "")
	s := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store}, map[string]*session{"nano-1": sess})

	put := func(ref, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+ref+"/presentation", strings.NewReader(body))
		applicationHandler(s).ServeHTTP(rec, req)
		return rec
	}

	if rec := put(hosted.ID, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("omitted mode = %d, want 400 (must never mean shared)", rec.Code)
	}
	if rec := put(hosted.ID, `{"presentationMode":"hidden"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode = %d", rec.Code)
	}

	rec := put(hosted.ID, `{"presentationMode":"background"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("flip = %d body %s", rec.Code, rec.Body.String())
	}
	var view struct {
		PresentationMode string `json:"presentationMode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.PresentationMode != presentationBackground {
		t.Fatalf("view mode = %q err=%v", view.PresentationMode, err)
	}
	if sess.Summary().PresentationMode != presentationBackground {
		t.Fatal("live session summary did not flip")
	}
	if got, ok := store.PresentationModeFor(hosted.ID); !ok || got != presentationBackground {
		t.Fatalf("execution cache mode = %q ok=%v", got, ok)
	}

	// Idempotent repeat via the nano session id ref, and flip back.
	if rec := put("nano-1", `{"presentationMode":"background"}`); rec.Code != http.StatusOK {
		t.Fatalf("idempotent put = %d", rec.Code)
	}
	if rec := put("nano-1", `{"presentationMode":"shared"}`); rec.Code != http.StatusOK {
		t.Fatalf("flip back = %d", rec.Code)
	}
	if sess.Summary().PresentationMode != presentationShared {
		t.Fatal("flip back did not reach the live session")
	}

	if rec := put(hosted.ID+"/extra", `{"presentationMode":"shared"}`); rec.Code == http.StatusOK {
		t.Fatal("non-empty tail accepted")
	}
	notFound := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(notFound, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/nht_missing/presentation", strings.NewReader(`{"presentationMode":"shared"}`)))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unknown ref = %d", notFound.Code)
	}
}

func TestHostedThreadSidebarPlacementEndpoint(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, err := fixtureBeginSession(store, "caller-1", "request-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	hub := sidebar.NewRuntimeHub()
	changes := hub.Subscribe()
	t.Cleanup(func() { hub.Unsubscribe(changes) })
	s := &server{Registry: newSessionRegistry(), agentExecutions: store, controlMetadata: control, runtimeEvents: hub}
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+hosted.ID+"/sidebar-placement", strings.NewReader(body))
		applicationHandler(s).ServeHTTP(rec, req)
		return rec
	}
	if rec := put(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("omitted placement = %d", rec.Code)
	}
	if rec := put(`{"sidebarPlacement":"floating"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid placement = %d", rec.Code)
	}
	rec := put(`{"sidebarPlacement":"standalone"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("standalone = %d body %s", rec.Code, rec.Body.String())
	}
	var view agentSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || view.SidebarPlacement != sidebarPlacementStandalone {
		t.Fatalf("view = %+v err=%v", view, err)
	}
	select {
	case delta := <-changes:
		if !delta.Refresh {
			t.Fatalf("runtime nudge = %+v, want structural refresh", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("placement write did not nudge open browsers")
	}
}

// The HTTP boundary owns explicit choices, durable audit/cache coherence,
// and the dispatcher must use each update without replaying the quiet period.
func TestHostedThreadSubscriptionsEndpoint(t *testing.T) {
	control, store := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(store, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithSubscriptions(store, "child", "child-hash", "claude", presentationShared, parent, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{Registry: newSessionRegistry(), agentExecutions: store, controlMetadata: control}
	request := func(method, ref, suffix, body string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		applicationHandler(s).ServeHTTP(out, httptest.NewRequest(method, "/api/v1/hosted-threads/"+ref+suffix, strings.NewReader(body)))
		return out
	}
	revisionAndAudit := func() (int64, int) {
		t.Helper()
		var revision int64
		var audits int
		if err := control.CanonicalDatabase().QueryRow(`SELECT revision FROM hosted_threads WHERE hosted_thread_id=?`, child.ID).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		if err := control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field='subscriptions'`, child.ID).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		return revision, audits
	}
	for _, body := range []string{`{}`, `null`, `{"inbox":"none"}`, `{"prompt":"send"}`, `{"inbox":"","prompt":"none"}`, `{"inbox":"none","prompt":"loud"}`, `{"inbox":true,"prompt":"none"}`} {
		if out := request(http.MethodPut, child.ID, "/subscriptions", body); out.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s: %d %s", body, out.Code, out.Body.String())
		}
	}
	if out := request(http.MethodPut, "nht_missing", "/subscriptions", `{"inbox":"none","prompt":"none"}`); out.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", out.Code)
	}
	before, auditsBefore := revisionAndAudit()
	put := func(choices subscriptions.Choices) {
		t.Helper()
		body, _ := json.Marshal(choices)
		out := request(http.MethodPut, "child", "/subscriptions", string(body))
		if out.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", out.Code, out.Body.String())
		}
		for _, response := range []*httptest.ResponseRecorder{out, request(http.MethodGet, child.ID, "", "")} {
			var view struct {
				Subscriptions subscriptions.Choices `json:"subscriptions"`
			}
			if response.Code != http.StatusOK {
				t.Fatalf("GET: %d %s", response.Code, response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || view.Subscriptions != choices {
				t.Fatalf("returned choices = %+v err=%v", view, err)
			}
		}
		cached, ok := store.Session(child.ID)
		if !ok || cached.SubscriptionInbox != (choices.Inbox == subscriptions.Send) || cached.SubscriptionPrompt != (choices.Prompt == subscriptions.Send) {
			t.Fatalf("cache choices = %+v", cached)
		}
	}
	promptOnly := subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.Send}
	put(promptOnly)
	after, auditsAfter := revisionAndAudit()
	if after != before+1 || auditsAfter != auditsBefore+1 {
		t.Fatalf("changed PUT: revisions %d/%d audit %d/%d", before, after, auditsBefore, auditsAfter)
	}
	auditResponse := request(http.MethodGet, child.ID, "/metadata/events?field=subscriptions", "")
	var auditPage metadataEventPage
	if auditResponse.Code != http.StatusOK {
		t.Fatalf("audit GET: %d %s", auditResponse.Code, auditResponse.Body.String())
	}
	if err := json.Unmarshal(auditResponse.Body.Bytes(), &auditPage); err != nil || len(auditPage.Events) != 1 {
		t.Fatalf("audit page=%+v err=%v", auditPage, err)
	}
	event := auditPage.Events[0]
	if event.Source != postureSourceOperator || event.Disposition != metadataDispositionConfirmed || event.Freshness != metadataFreshnessLive {
		t.Fatalf("audit provenance=%+v", event)
	}
	auditValue := string(event.Value)
	var audit struct {
		Old subscriptions.Choices `json:"old"`
		New subscriptions.Choices `json:"new"`
	}
	if err := json.Unmarshal([]byte(auditValue), &audit); err != nil || audit.Old != (subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}) || audit.New != promptOnly {
		t.Fatalf("audit=%s err=%v", auditValue, err)
	}
	put(promptOnly)
	sameRevision, sameAudits := revisionAndAudit()
	if sameRevision != after || sameAudits != auditsAfter {
		t.Fatal("same-value PUT changed revision or audit")
	}
	client := &recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{}}
	dispatcher := subscriptionDispatcherForTest(store, client)
	finish := func(cursor, turn string) {
		t.Helper()
		client.pages = map[string]interact.CompletedTurnPage{child.ID + "\x00" + cursor: {Items: []interact.CompletedTurn{{TurnID: turn, Outcome: agentTurnCompleted}}, NextCursor: turn}}
		dispatcher.Sweep(child.ID)
		got, _, err := subscriptions.Cursor(control.CanonicalDatabase(), child.ID)
		if err != nil || got != turn {
			t.Fatalf("cursor=%q want %q err=%v", got, turn, err)
		}
	}
	finish("", "first")
	if len(client.prompts) != 1 || len(client.messages) != 0 {
		t.Fatalf("next completion channels: %d prompts %d mail", len(client.prompts), len(client.messages))
	}
	put(subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.None})
	finish("first", "quiet-one")
	finish("quiet-one", "quiet-two")
	if len(client.prompts) != 1 || len(client.messages) != 0 {
		t.Fatal("disabled completions delivered")
	}
	put(subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None})
	finish("quiet-two", "resumed")
	if len(client.prompts) != 1 || len(client.messages) != 1 {
		t.Fatal("switching back on replayed or used stale choices")
	}
	reloaded, err := newControlAgentExecutionStore(control, "reload")
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reloaded.Session(child.ID)
	if !ok || !saved.SubscriptionInbox || saved.SubscriptionPrompt {
		t.Fatal("settings did not survive store reload")
	}
}

// Failure caught: a person's Discard ended an agent's prompt but not the
// operation that sent it, so the sender's operation stayed queued or running
// for good. Whoever ends the prompt, the operation ends interrupted, saying
// who ended it and whether the provider may have taken the prompt. A Discard
// whose operation write fails changes neither, so asking again ends both;
// ending the prompt alone left the operation open for good, since a second
// Discard found the prompt already ended. Boundary: the person's Discard route
// and the agent cancel route, with the operation read as its sender reads it.
func TestDiscardedPromptEndsTheOperationThatSentIt(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name         string
		uncertain    bool
		byCaller     bool
		source, code string
		ended        turns.DeliveryState
	}{
		{name: "a person discards a queued prompt", source: "operator", code: "queued_turn_canceled", ended: turns.Refused},
		{name: "a person discards a prompt whose delivery is unknown", uncertain: true, source: "operator", code: "prompt_discarded", ended: turns.Expired},
		{name: "the caller cancels its queued prompt", byCaller: true, source: "client", code: "queued_turn_canceled", ended: turns.Refused},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, target := interactTestServer(t, true)
			sess := s.SessionForHostedThread(target)
			op, _, err := fixtureBeginTurn(s.agentExecutions, target, "worker-ask", "hash-ask")
			if err != nil {
				t.Fatal(err)
			}
			action := op.TransportActionID
			if _, _, err := sess.PromptStore().Submit(ctx, turns.Submission{HostedThreadID: target, Provider: model.ProviderCodex, NativeThreadID: "native-target", ActionID: action,
				ProviderText: "ask", PresentationText: "ask", TextHash: sendTextHash("ask"), RequestHash: sendTextHash("ask"), Origin: turns.AgentOrigin}); err != nil {
				t.Fatal(err)
			}
			// Queued behind the thread's running turn, as the submit route
			// records it.
			if err := s.agentExecutions.MarkTurnDelivery(op.ID, "", agentTurnQueued, nil); err != nil {
				t.Fatal(err)
			}
			if test.uncertain {
				if _, claimed, err := sess.PromptStore().Claim(ctx, turns.ClaimRequest{HostedThreadID: target, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
					t.Fatalf("claim = %v, %v", claimed, err)
				}
				if _, err := sess.PromptStore().Uncertain(ctx, turns.Uncertainty{ActionID: action, ClaimToken: "claim", AttachmentEpoch: 1, Reason: "no answer"}); err != nil {
					t.Fatal(err)
				}
				if err := s.agentExecutions.MarkTurnDelivery(op.ID, "", agentTurnDeliveryUnknown, nil); err != nil {
					t.Fatal(err)
				}
			}
			discard := func() *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				if test.byCaller {
					request := httptest.NewRequest(http.MethodDelete, "/api/v1/turn-operations/"+op.ID, nil)
					request.SetPathValue("operationId", op.ID)
					s.handleTurnOperationCancel(rec, request)
				} else {
					sess.HandleTurnDeliveryCancel(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sess.ID()+"/turn-delivery/"+action, nil), action)
				}
				return rec
			}
			before, _, err := sess.PromptStore().Get(ctx, action)
			if err != nil {
				t.Fatal(err)
			}
			opBefore, _ := s.agentExecutions.Turn(op.ID)
			db := s.agentExecutions.Control().CanonicalDatabase()
			if _, err := db.Exec(`CREATE TRIGGER refuse_ending BEFORE UPDATE ON hosted_turn_operations WHEN NEW.status='interrupted' BEGIN SELECT RAISE(ABORT,'the operation write fails'); END`); err != nil {
				t.Fatal(err)
			}
			if rec := discard(); rec.Code != http.StatusInternalServerError {
				t.Fatalf("discard with a failing operation write = %d %s; want 500", rec.Code, rec.Body.String())
			}
			kept, _, err := sess.PromptStore().Get(ctx, action)
			if opKept, _ := s.agentExecutions.Turn(op.ID); err != nil || kept.Outbox.State != before.Outbox.State || opKept.Status != opBefore.Status {
				t.Fatalf("after a failed discard: prompt %s, operation %s, %v; want %s and %s unchanged", kept.Outbox.State, opKept.Status, err, before.Outbox.State, opBefore.Status)
			}
			if _, err := db.Exec(`DROP TRIGGER refuse_ending`); err != nil {
				t.Fatal(err)
			}
			if rec := discard(); rec.Code != http.StatusOK {
				t.Fatalf("discard asked again = %d %s", rec.Code, rec.Body.String())
			}
			ended, _ := s.agentExecutions.Turn(op.ID)
			if ended.Status != agentTurnInterrupted || ended.Error == nil || ended.Error.Source != test.source || ended.Error.Code != test.code {
				t.Fatalf("operation = %s %+v; want interrupted by %s with %s", ended.Status, ended.Error, test.source, test.code)
			}
			if after, _, err := sess.PromptStore().Get(ctx, action); err != nil || after.Outbox.State != test.ended {
				t.Fatalf("prompt = %s, %v; want %s", after.Outbox.State, err, test.ended)
			}
		})
	}
}
