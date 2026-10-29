package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestReconnectAuditMissingZeroTurnPersistsContainment(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.SetResumeError("no rollout found for thread missing-zero")
	fake.SetTurnStartError("thread not found: missing-zero")
	daemon := fake.daemon()
	root := t.TempDir()
	attemptPath, queuePath := filepath.Join(root, "attempts.json"), filepath.Join(root, "queue.json")
	attempts, err := newTestSendAttemptStore(t, attemptPath, "before")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newTestQueuedTurnStore(t, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	prompts := mustBindTransactionalTurnStore(t, queue, attempts)
	store, err := newAgentExecutionStore(filepath.Join(root, "operations.json"), "before")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, err := fixtureBeginSession(store, "missing-zero", "hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(sessions.Config{ID: "missing-zero", Provider: "codex", HostedThreadID: hosted.ID, AgentExecutions: store, Turns: prompts, QueuedTurns: queue, SendAttempts: attempts, Projection: newLiveProjection("codex", hosted.ID)})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID("missing-zero")
	t.Cleanup(sess.Close)
	if err := store.BindSession(hosted.ID, sess.ID(), sess.ThreadID(), "starting", presentationBackground); err != nil {
		t.Fatal(err)
	}
	request := sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.ThreadID(), BootReattach: newBootProviderControl(0), CodexZeroTurnResume: true}
	if err := sess.BeginProviderSpawn(request, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "ready" })
	operation, _, err := fixtureBeginTurn(store, hosted.ID, "orientation", sendTextHash("retained source text"))
	if err != nil {
		t.Fatal(err)
	}
	if response := postTurn(t, sess.Delivery(), context.Background(), operation.TransportActionID, "retained source text"); response.Code != http.StatusAccepted {
		t.Fatalf("orientation = %d %s", response.Code, response.Body.String())
	}
	waitTurnSendDone(t, sess, operation.TransportActionID)
	if sess.Summary().Status != "reconnect_failed" {
		t.Fatalf("status=%s", sess.Summary().Status)
	}
	attempt, _ := attempts.Get(operation.TransportActionID)
	if attempt.Status != submissionParked || attempt.Reason != "codex_native_thread_missing" {
		t.Fatalf("marker=%+v", attempt)
	}
	queued, _ := queue.Get(operation.TransportActionID)
	if queued.Text != "retained source text" || queued.State != submissionParked {
		t.Fatalf("retained=%+v", queued)
	}
	settled, _ := store.Turn(operation.ID)
	if settled.Status != agentTurnFailed || settled.ProviderTurnID != "" {
		t.Fatalf("operation=%+v", settled)
	}
	srv := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store}, map[string]*sessions.Session{sess.ID(): sess})
	for repeat := 0; repeat < 2; repeat++ {
		if state := fixtureOwners(srv).hostedViews().AvailabilityFor(hosted.ID, time.Now()).State; state == availabilityAvailable {
			t.Fatal("reconnecting thread was available to feature clients")
		}
		if response := postTurn(t, sess.Delivery(), context.Background(), fmt.Sprintf("later-%d", repeat), "do not deliver"); response.Code != http.StatusConflict {
			t.Fatalf("later admission=%d", response.Code)
		}
	}
	beforeResumes := fake.ResumeCount()
	fake.DropConnections()
	waitReconnectCondition(t, func() bool { return fake.ResumeCount() > beforeResumes && sess.Summary().Status == "reconnect_failed" })
	sess.Close()
	attemptsAfter, err := newTestSendAttemptStore(t, attemptPath, "after")
	if err != nil {
		t.Fatal(err)
	}
	queueAfter, err := newTestQueuedTurnStore(t, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	promptsAfter := mustBindTransactionalTurnStore(t, queueAfter, attemptsAfter)
	reopened := newSession(sessions.Config{ID: "reopened-zero", Provider: "codex", HostedThreadID: hosted.ID, AgentExecutions: store, Turns: promptsAfter, QueuedTurns: queueAfter, SendAttempts: attemptsAfter, Projection: newLiveProjection("codex", hosted.ID)})
	if err := reopened.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	reopened.ReserveNativeID(request.ThreadID)
	t.Cleanup(reopened.Close)
	if err := reopened.BeginProviderSpawn(request, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool {
		status := reopened.Summary().Status
		return status == sessionStatusExited || status == "reconnect_failed"
	})
	if reopened.ThreadID() != request.ThreadID || reopened.HostedThreadID() != hosted.ID {
		t.Fatal("reboot lost containment or identity")
	}
	retained, _ := queueAfter.Get(operation.TransportActionID)
	if retained.Text != "retained source text" {
		t.Fatal("reboot lost source text")
	}
	fake.Mu.Lock()
	defer fake.Mu.Unlock()
	if fake.TurnStarts != 1 {
		t.Fatalf("provider submits=%d", fake.TurnStarts)
	}
}

func TestReconnectAuditTerminalSettlementAndAvailability(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	store := newTestAgentExecutionStore(t, "audit")
	hosted, _, err := fixtureBeginSession(store, "terminal", "hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runtimeFixtureConfig(t, sessions.Config{ID: "deleted-rollout", Provider: "codex", HostedThreadID: hosted.ID, AgentExecutions: store, Projection: newLiveProjection("codex", hosted.ID)})
	sess := newSession(cfg)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID("deleted-rollout")
	t.Cleanup(sess.Close)
	fake.SetResumeResult(map[string]any{"thread": map[string]any{"id": sess.ThreadID()}})
	if err := sess.BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.ThreadID()}, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "ready" })
	seedAcceptedTransactionalTurn(t, sess.Delivery().PromptStore(), hosted.ID, model.ProviderCodex, sess.ThreadID(), "previous-turn", "past-turn", "past")
	turnStore, err := coreturns.OpenStore(context.Background(), cfg.QueuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := turnStore.Finished(context.Background(), coreturns.Finish{HostedThreadID: hosted.ID, Provider: string(model.ProviderCodex), NativeThreadID: sess.ThreadID(), TurnID: "past-turn", ActionID: "previous-turn", Outcome: string(model.OutcomeCompleted), Source: "test"}); err != nil {
		t.Fatal(err)
	}
	operation, _, err := fixtureBeginTurn(store, hosted.ID, "terminal-prompt", sendTextHash("retained"))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once, enteredOnce sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	fake.SetResumeError("no rollout found for thread deleted-rollout")
	fake.Mu.Lock()
	fake.OnResume = func(json.RawMessage) { enteredOnce.Do(func() { close(entered) }); <-release }
	fake.Mu.Unlock()
	fake.DropConnections()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect did not reach the provider")
	}
	if response := postTurn(t, sess.Delivery(), context.Background(), operation.TransportActionID, "retained"); response.Code != http.StatusAccepted {
		t.Fatalf("reconnecting admission = %d %s", response.Code, response.Body.String())
	}
	unblock()
	waitReconnectCondition(t, func() bool { record, _ := store.Turn(operation.ID); return record.Status == agentTurnFailed })
	record, _ := store.Turn(operation.ID)
	if record.Error == nil || record.Error.Code != "reconnect_failed" {
		t.Fatalf("terminal error: %+v", record.Error)
	}
	if response := postTurn(t, sess.Delivery(), context.Background(), "new", "do not queue"); response.Code != http.StatusConflict {
		t.Fatalf("terminal admission: %d", response.Code)
	}
	server := testServerWithSessions(&server{Registry: newSessionRegistry(), agentExecutions: store}, map[string]*sessions.Session{sess.ID(): sess})
	view := fixtureOwners(server).hostedViews().AvailabilityFor(hosted.ID, time.Now())
	if view.State != availabilityUnreachable || len(view.Reasons) != 1 || view.Reasons[0] != "reconnect_failed" {
		t.Fatalf("availability: %+v", view)
	}
}
