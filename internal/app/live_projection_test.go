package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type liveFixtureLine struct {
	Dir string          `json:"dir"`
	Msg json.RawMessage `json:"msg"`
}

func TestCompletedProviderUserMessageDoesNotSettleThePrompt(t *testing.T) {
	dir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newTestSendAttemptStore(t, filepath.Join(dir, "send-attempts.json"), "run-one")
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, store)
	seedAcceptedTransactionalTurn(t, turnStore, "hosted-thread", model.ProviderCodex, "native-thread", "action-wire-confirm", "turn-1", "ship it now")
	projection := newLiveProjection("codex", "hosted-thread")
	f := newProviderRuntimeFixture(t, sessions.Config{ID: "session-wire-confirm", Provider: "codex", HostedThreadID: "hosted-thread", Projection: projection, Turns: turnStore, QueuedTurns: queue, SendAttempts: store}, "native-thread", "")

	started := []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"provider-user-1","type":"userMessage","content":[{"type":"text","text":"ship it now"}]}}}`)
	completed := []byte(`{"method":"item/completed","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"provider-user-1","type":"userMessage","content":[{"type":"text","text":"ship it now"}]}}}`)
	agent := []byte(`{"method":"item/started","params":{"threadId":"native-thread","turnId":"turn-1","item":{"id":"agent-1","type":"agentMessage","text":"Working"}}}`)

	f.emitAndWait(t, started)
	if record, _ := store.Get("action-wire-confirm"); record.Confirmed {
		t.Fatalf("item/started confirmed the send before the provider completed its user item: %#v", record)
	}
	f.emitAndWait(t, completed)
	if record, ok := store.Get("action-wire-confirm"); !ok || record.Confirmed || record.Status != sendAttemptAccepted {
		t.Fatalf("completed provider user item settled accepted prompt: %#v, found=%v", record, ok)
	}
	f.emitAndWait(t, agent)
	if record, ok := store.Get("action-wire-confirm"); !ok || record.Confirmed {
		t.Fatalf("agent activity settled the accepted prompt: %#v, found=%v", record, ok)
	}
}

func TestLiveProjectionCatalogChangeResolvesNativeSessionIdentity(t *testing.T) {
	started := time.Now().Add(-time.Minute).UnixMilli()
	sess, _ := durableTurnTestSession(t, "turn-1", started)
	sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (liveview.DurableTurnEvidence, bool) {
		return liveview.DurableTurnEvidence{Turn: liveview.Turn{ID: turnID, Status: "completed", CompletedAtMs: started + 1}}, turnID == "turn-1"
	}, nil, nil, nil, nil)
	s := testServerWithSessions(&server{catalog: newCatalogStoreFixture(t).source, Registry: newSessionRegistry()}, map[string]*sessions.Session{sess.ID(): sess})
	fixtureOwners(

		// Transcript notifications use the catalog's nth_ identity, while hosted
		// sessions retain the provider-native thread identity.
		s).attachmentCoordinator().ConfirmDurableThread(context.Background(), threadindex.TranscriptChange{
		ThreadID: "nth_1", Provider: "codex", NativeThreadID: "native-thread",
	})

	if summary := sess.Summary(); summary.TurnActive {
		t.Fatalf("catalog notification did not reach the session's open turn: %#v", summary)
	}
}

// A small parser-recovery backlog must leave hosted control responsive and
// reconcile every attached session once, plus a change received in flight.
// The index package separately tests the 5,000-change dispatcher backlog.
func TestTranscriptChangeParserRecoveryLoadKeepsControlPromptAndIdempotent(t *testing.T) {
	const (
		attachedSessions   = 100
		changedTranscripts = attachedSessions
	)
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
	hosted, _, err := fixtureBeginSession(executions, "load-target", "request-hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(hosted.ID, "session-99", "native-99", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}

	server := &server{
		Registry: newSessionRegistry(), agentExecutions: executions,
		sendAttempts: attempts, queuedTurns: queue, turns: turnStore,
	}
	blocked := make(chan struct{})
	started := make(chan struct{}, transcriptChangeWorkerCount)
	var durableCalls atomic.Int32
	var providerCalls atomic.Int32
	for index := 0; index < attachedSessions; index++ {
		id := fmt.Sprintf("session-%d", index)
		nativeID := fmt.Sprintf("native-%d", index)
		cfg := sessions.Config{ID: id, Provider: "codex", Projection: newLiveProjection("codex")}
		if id == "session-99" {
			cfg.HostedThreadID = hosted.ID
			cfg.PresentationMode = presentationBackground
			cfg.AgentExecutions = executions
			cfg.Turns = turnStore
			cfg.SendAttempts = attempts
			cfg.QueuedTurns = queue
		}
		f := newProviderRuntimeFixture(t, cfg, nativeID, "", func(f *providerRuntimeFixture) {
			if id != "session-99" {
				f.resumeResult = map[string]any{"thread": map[string]any{"id": nativeID, "turns": []any{map[string]any{"id": "open-turn", "status": "inProgress"}}}}
			}
			if id == "session-99" {
				f.send = func(context.Context, string) (json.RawMessage, error) {
					providerCalls.Add(1)
					return json.RawMessage(`{"turn":{"id":"provider-load-turn"}}`), nil
				}
			}
		})
		reader := func(context.Context, string) (liveview.DurableTurnEvidence, bool) {
			durableCalls.Add(1)
			return liveview.DurableTurnEvidence{}, false
		}
		if index < transcriptChangeWorkerCount {
			reader = func(ctx context.Context, _ string) (liveview.DurableTurnEvidence, bool) {
				started <- struct{}{}
				select {
				case <-blocked:
				case <-ctx.Done():
				}
				durableCalls.Add(1)
				return liveview.DurableTurnEvidence{}, false
			}
		}
		f.session.ConfigureSavedReaders(reader, nil, nil, nil, nil)
		server.RegisterSession(f.session)
	}
	target := server.LookupSession("session-99")

	nativeIDs := make([]string, changedTranscripts)
	for i := range nativeIDs {
		nativeIDs[i] = fmt.Sprintf("native-%d", i)
	}
	service, paths := queuedCatalogFixture(t, "codex", nativeIDs)
	var callbacks atomic.Int32

	service.SetTranscriptChanged(func(ctx context.Context, change threadindex.TranscriptChange) {
		callbacks.Add(1)
		fixtureOwners(server).attachmentCoordinator().ConfirmDurableThread(ctx, change)
	})
	for worker := 0; worker < transcriptChangeWorkerCount; worker++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("transcript workers did not start")
		}
	}
	// One update lands while the first session is in flight and must run again.
	appendCallbackFixture(t, service, paths[0])
	waitForCatalog(t, func() bool { status := service.IngestStatus(); return status.Queued == 0 && status.InFlight == 0 })
	if status, err := service.IndexStatus(context.Background()); err != nil || status.TranscriptChangeBacklog != changedTranscripts || status.TranscriptChangeInFlight != transcriptChangeWorkerCount {
		t.Fatalf("storm status = %#v, %v", status, err)
	}

	assertPrompt := func(name string, call func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		startedAt := time.Now()
		rec := call()
		if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
			t.Fatalf("%s took %s during transcript storm", name, elapsed)
		}
		return rec
	}
	if rec := assertPrompt("hosted thread GET", func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/"+hosted.ID, nil))
		return rec
	}); rec.Code != http.StatusOK {
		t.Fatalf("hosted thread GET = %d: %s", rec.Code, rec.Body.String())
	}
	sender := confirmedTestSender(t, server, "codex")
	body, err := json.Marshal(map[string]any{
		"clientActionId": "load-require-idle", "prompt": "run once", "requireIdle": true, "senderThreadRef": sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	submit := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		applicationHandler(fixtureOwners(server)).ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/hosted-threads/"+hosted.ID+"/turns", strings.NewReader(string(body))))
		return rec
	}
	first := assertPrompt("first requireIdle", submit)
	second := assertPrompt("idempotent requireIdle", submit)
	if first.Code != http.StatusAccepted || second.Code != http.StatusOK {
		t.Fatalf("requireIdle statuses = %d/%d: %s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	var firstView, secondView hostedmodel.TurnView
	if err := json.Unmarshal(first.Body.Bytes(), &firstView); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondView); err != nil {
		t.Fatal(err)
	}
	if firstView.ID == "" || secondView.ID != firstView.ID {
		t.Fatalf("idempotent operation ids = %q / %q", firstView.ID, secondView.ID)
	}
	initialOperation, ok := fixtureTurnForAction(executions, hosted.ID, "load-require-idle")
	if !ok {
		t.Fatal("canonical operation is missing")
	}
	waitForTurnSendDone(t, target, initialOperation.TransportActionID)
	if operation, ok := fixtureTurnForAction(executions, hosted.ID, "load-require-idle"); !ok || operation.ID != firstView.ID || operation.ProviderTurnID != "provider-load-turn" {
		t.Fatalf("canonical operation = %#v, found=%v", operation, ok)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("provider turns = %d, want exactly one", providerCalls.Load())
	}

	close(blocked)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := service.IndexStatus(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		backlog, inFlight := status.TranscriptChangeBacklog, status.TranscriptChangeInFlight
		if backlog == 0 && inFlight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript recovery did not drain: backlog=%d in-flight=%d", backlog, inFlight)
		}
		time.Sleep(time.Millisecond)
	}
	if callbacks.Load() != changedTranscripts+1 {
		t.Fatalf("callbacks = %d, want %d (one in-flight replay)", callbacks.Load(), changedTranscripts+1)
	}
	// Every session reads its open turn once, the prompt target included (its
	// prompt's turn is open when its change runs); the first session runs twice.
	if durableCalls.Load() != attachedSessions+1 {
		t.Fatalf("attached session reconciliations = %d, want %d", durableCalls.Load(), attachedSessions+1)
	}
}

type savedHistoryCatalog struct {
	*fakeTranscriptCatalog
}

func (c *savedHistoryCatalog) ResolveThreadReference(_ context.Context, reference string) (string, error) {
	return "nth-owner", nil
}

func TestConversationHistoryReadsSavedRowsWithoutACut(t *testing.T) {
	nativeThreadID := "native-thread"
	p := newLiveProjection("codex", "hosted-thread")
	fake := &savedHistoryCatalog{fakeTranscriptCatalog: &fakeTranscriptCatalog{response: threadindex.TranscriptResponse{
		ThreadID: "nth-owner",
		Items:    []threadindex.TranscriptItem{{ID: "durable-0", OwnerItemID: "retained-0", ProjectedSeq: 1, TurnID: "turn-0"}},
		Turns:    []threadindex.TranscriptTurn{{ID: "turn-0", FinalItemID: "durable-0"}},
		Page:     threadindex.TranscriptPageInfo{StartSeq: 1, EndSeq: 1},
	}}}
	sess := newFixtureSession(sessions.Config{Provider: "codex", Projection: p}, nativeThreadID)
	recorder := httptest.NewRecorder()
	threadindex.NewHandlers(fake).HandleConversationHistory(recorder,
		httptest.NewRequest(http.MethodGet, "/api/v1/sessions/session/conversation/history", nil), sess.State().ThreadID())
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response threadindex.TranscriptResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if fake.query.BeforeNativeTurn != "" || fake.query.BeforeTurn != 0 || fake.query.TurnLimit != 0 {
		t.Fatalf("saved history query = %#v", fake.query)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "durable-0" {
		t.Fatalf("history items = %#v", response.Items)
	}
	if len(response.Turns) != 1 || response.Turns[0].ID != "turn-0" || response.Turns[0].FinalItemID != "durable-0" {
		t.Fatalf("history turns = %#v", response.Turns)
	}
	if response.Page.StartSeq != 1 || response.Page.EndSeq != 1 {
		t.Fatalf("history page = %#v", response.Page)
	}
}

func durableHousekeepingWindow(count int) []liveview.Item {
	rows := make([]liveview.Item, 0, count)
	for i := 0; i < count; i++ {
		rows = append(rows, liveview.Item{
			ID:            fmt.Sprintf("durable:noise-%d", i),
			DurableItemID: fmt.Sprintf("row-noise-%d", i),
			ProjectedSeq:  int64(i + 1),
			Type:          "provider_event",
			Text:          "Provider housekeeping record",
		})
	}
	return rows
}

// reconcileWithSavedTurn runs turn repair against one saved turn, which the
// read returns only for its own exact id.
func reconcileWithSavedTurn(sess *sessions.Session, saved liveview.DurableTurnEvidence) {
	sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (liveview.DurableTurnEvidence, bool) {
		return saved, turnID == saved.Turn.ID
	}, nil, nil, nil, nil)
	sess.ReconcileDurableTurn(context.Background())
}

func durableTurnTestSession(t *testing.T, turnID string, startedAtMs int64, drop ...bool) (*sessions.Session, *sessions.LiveProjection) {
	p := newLiveProjectionWithDeltaSetting("codex", len(drop) > 0 && drop[0])
	f := newProviderRuntimeFixture(t, sessions.Config{ID: "session-1", Provider: "codex", HostedThreadID: "hosted", Projection: p}, "native-thread", "", func(f *providerRuntimeFixture) {
		f.resumeResult = map[string]any{"thread": map[string]any{"id": "native-thread", "turns": []any{map[string]any{"id": turnID, "status": "inProgress", "startedAt": startedAtMs / 1000}}}}
	})
	return f.session, p
}
