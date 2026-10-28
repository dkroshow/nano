package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func reconnectTestSession(t *testing.T, daemon *codexDaemon, threadID string) *session {
	t.Helper()
	dir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(dir, "attempts.json"), "audit")
	if err != nil {
		t.Fatal(err)
	}
	mustBindTransactionalTurnStore(t, queue, attempts)
	return reconnectTestSessionWithStores(t, daemon, threadID, queue, attempts)
}

func reconnectTestSessionWithStores(t *testing.T, daemon *codexDaemon, threadID string, queue *queuedTurnStore, attempts *sendAttemptStore) *session {
	t.Helper()
	sess := &session{id: threadID,
		provider:       "codex",
		threadID:       threadID,
		hostedThreadID: "hosted-" + threadID,
		state:          map[string]json.RawMessage{},
		subs:           map[chan wireEvent]struct{}{},
		projection:     newLiveProjection("codex"),
		sendOps:        map[string]*turnSendOperation{},
		turns:          queue.turns,
		queuedTurns:    queue,
		sendAttempts:   attempts,
		codex:          daemon,
		lifecycleState: testLifecycleSnapshot("ready", false)}
	if err := daemon.claimThread(threadID, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sess.close()
		waitReconnectCondition(t, func() bool {
			daemon.mu.Lock()
			defer daemon.mu.Unlock()
			return !daemon.reconnecting[sess.id]
		})
	})
	return sess
}

func reconnectTestFleet(t *testing.T, daemon *codexDaemon, prefix string, count int) []*session {
	t.Helper()
	// A production fleet shares one canonical turn database and its stores.
	db, path, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(db, path, testPromptStore(t, db))
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newSendAttemptStore(db, path, "audit", testPromptStore(t, db))
	if err != nil {
		t.Fatal(err)
	}
	mustBindTransactionalTurnStore(t, queue, attempts)
	sessions := make([]*session, count)
	for index := range sessions {
		sessions[index] = reconnectTestSessionWithStores(t, daemon, fmt.Sprintf("%s-%03d", prefix, index), queue, attempts)
	}
	return sessions
}

func TestReconnectAuditPersistentFleetParksAndRearms(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "rearmed-turn"}})
	daemon := fake.daemon()
	daemon.resumeTimeout, daemon.reconnectBackoff = 100*time.Millisecond, time.Millisecond
	if err := daemon.ensure(); err != nil {
		t.Fatal(err)
	}
	sessions := reconnectTestFleet(t, daemon, "fleet", 123)
	for index := range sessions {
		if index < 6 {
			fake.resumeDelays[sessions[index].threadID] = time.Second
		}
	}
	fake.dropConnections()
	waitReconnectCondition(t, func() bool {
		for index, sess := range sessions {
			want := "ready"
			if index < 6 {
				want = "reconnect_parked"
			}
			if sess.summary().Status != want {
				return false
			}
		}
		return true
	})
	fake.mu.Lock()
	for index, sess := range sessions {
		want := 1
		if index < 6 {
			want = codexReconnectAttempts
		}
		count := fake.resumeCounts[sess.threadID]
		if (index < 6 && count != want) || (index >= 6 && (count < 1 || count > codexReconnectAttempts)) {
			t.Errorf("%s attempts=%d want=%d", sess.threadID, fake.resumeCounts[sess.threadID], want)
		}
	}
	fake.resumeDelays[sessions[0].threadID] = 0
	fake.mu.Unlock()
	for repeat := 0; repeat < 2; repeat++ {
		response := postTurn(t, sessions[0], context.Background(), "rearm", "try after parking")
		if response.Code != http.StatusAccepted && response.Code != http.StatusOK {
			t.Fatalf("parked prompt: %d %s", response.Code, response.Body.String())
		}
	}
	waitTurnSendDone(t, sessions[0], "rearm")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.resumeCounts[sessions[0].threadID] != codexReconnectAttempts+1 || fake.turnStarts != 1 {
		t.Fatalf("rearm duplicates: resumes=%d starts=%d", fake.resumeCounts[sessions[0].threadID], fake.turnStarts)
	}
}

func TestReconnectAuditZeroTurnReadyAndSingleDelivery(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "zero-turn-first"}})
	daemon := fake.daemon()
	sess := reconnectTestSession(t, daemon, "zero-turn")
	sess.codexZeroTurnEligible = true
	sess.setStatus("reconnecting")
	fake.setResumeErrorSequence("no rollout found for thread zero-turn", "")
	if response := postTurn(t, sess, context.Background(), "first", "first prompt"); response.Code != http.StatusAccepted {
		t.Fatalf("queue: %s", response.Body.String())
	}
	waitTurnSendDone(t, sess, "first")
	waitReconnectCondition(t, func() bool { return !sess.codexZeroTurnRecoveryNeeded() })
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.turnStarts != 1 || sess.summary().Status != "ready" {
		t.Fatalf("zero-turn recovery: starts=%d status=%s", fake.turnStarts, sess.summary().Status)
	}
	attempt, _ := sess.sendAttempts.get("first")
	if attempt.Status != sendAttemptAccepted || attempt.Reason == codexNativeMissing {
		t.Fatalf("daemon-memory first delivery: %+v", attempt)
	}
}

func TestReconnectAuditMissingZeroTurnPersistsContainment(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setResumeError("no rollout found for thread missing-zero")
	fake.setTurnStartError("thread not found: missing-zero")
	daemon := fake.daemon()
	sess := reconnectTestSession(t, daemon, "missing-zero")
	root := t.TempDir()
	attemptPath, queuePath := filepath.Join(root, "attempts.json"), filepath.Join(root, "queue.json")
	var err error
	sess.sendAttempts, err = newTestSendAttemptStore(t, attemptPath, "before")
	if err != nil {
		t.Fatal(err)
	}
	sess.queuedTurns, err = newTestQueuedTurnStore(t, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	sess.turns = mustBindTransactionalTurnStore(t, sess.queuedTurns, sess.sendAttempts)
	store, err := newAgentExecutionStore(filepath.Join(root, "operations.json"), "before")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, err := store.beginSession("missing-zero", "hash", "codex", presentationBackground)
	if err != nil {
		t.Fatal(err)
	}
	sess.hostedThreadID, sess.agentExecutions = hosted.ID, store
	if err := store.bindSession(hosted.ID, sess.id, sess.threadID, "starting", presentationBackground); err != nil {
		t.Fatal(err)
	}
	sess.setStatus("starting")
	request := spawnRequest{Provider: "codex", Mode: "resume", ThreadID: sess.threadID, bootReattach: &bootProviderControl{}, codexZeroTurnResume: true}
	if err := startCodexSession(sess, daemon, request); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return sess.summary().Status == "ready" })
	operation, _, err := store.beginTurn(hosted.ID, "orientation", sendTextHash("retained source text"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sess.queueOrStartTurnSend(operation.TransportActionID, "retained source text"); err != nil {
		t.Fatal(err)
	}
	waitTurnSendDone(t, sess, operation.TransportActionID)
	if sess.summary().Status != "reconnect_failed" {
		t.Fatalf("status=%s", sess.summary().Status)
	}
	attempt, _ := sess.sendAttempts.get(operation.TransportActionID)
	if attempt.Status != submissionParked || attempt.Reason != codexNativeMissing {
		t.Fatalf("marker=%+v", attempt)
	}
	queued, _ := sess.queuedTurns.get(operation.TransportActionID)
	if queued.Text != "retained source text" || queued.State != submissionParked {
		t.Fatalf("retained=%+v", queued)
	}
	settled, _ := store.turn(operation.ID)
	if settled.Status != agentTurnFailed || settled.ProviderTurnID != "" {
		t.Fatalf("operation=%+v", settled)
	}
	srv := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}}, agentExecutions: store}
	for repeat := 0; repeat < 2; repeat++ {
		if state := srv.availabilityFor(hosted.ID, time.Now()).State; state == availabilityAvailable {
			t.Fatalf("reconnecting thread was available to feature clients")
		}
		if response := postTurn(t, sess, context.Background(), fmt.Sprintf("later-%d", repeat), "do not deliver"); response.Code != http.StatusConflict {
			t.Fatalf("later admission=%d", response.Code)
		}
	}
	beforeResumes := fake.resumeCount()
	fake.dropConnections()
	waitReconnectCondition(t, func() bool { return fake.resumeCount() > beforeResumes && sess.summary().Status == "reconnect_failed" })
	sess.close()
	waitReconnectCondition(t, func() bool { daemon.mu.Lock(); defer daemon.mu.Unlock(); return !daemon.reconnecting[sess.id] })
	reopened := reconnectTestSession(t, daemon, request.ThreadID)
	reopened.hostedThreadID, reopened.agentExecutions = hosted.ID, store
	reopened.sendAttempts, err = newTestSendAttemptStore(t, attemptPath, "after")
	if err != nil {
		t.Fatal(err)
	}
	reopened.queuedTurns, err = newTestQueuedTurnStore(t, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	reopened.turns = mustBindTransactionalTurnStore(t, reopened.queuedTurns, reopened.sendAttempts)
	reopened.setStatus("starting")
	if err := startCodexSession(reopened, daemon, request); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool {
		status := reopened.summary().Status
		return status == sessionStatusExited || status == "reconnect_failed"
	})
	if reopened.codexMayBeZeroTurn() || reopened.currentThreadID() != request.ThreadID || reopened.hostedThreadID != hosted.ID {
		t.Fatal("reboot lost containment or identity")
	}
	retained, _ := reopened.queuedTurns.get(operation.TransportActionID)
	if retained.Text != "retained source text" {
		t.Fatal("reboot lost source text")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.turnStarts != 1 {
		t.Fatalf("provider submits=%d", fake.turnStarts)
	}
}

func TestReconnectAuditNativeMissingRequiresExactEvidence(t *testing.T) {
	for _, scenario := range []string{"exact", "wrong-native", "untyped", "not-fallback", "accepted-history"} {
		t.Run(scenario, func(t *testing.T) {
			fake := newFakeAppServer(t)
			sess := reconnectTestSession(t, fake.daemon(), "evidence-thread")
			sess.codexZeroTurnEligible, sess.codexZeroTurnResume = true, true
			var deliveryErr error = &protocol.RPCError{Code: -32600, Message: "thread not found: evidence-thread"}
			switch scenario {
			case "wrong-native":
				deliveryErr = &protocol.RPCError{Code: -32600, Message: "thread not found: other"}
			case "untyped":
				deliveryErr = errors.New("thread not found: evidence-thread")
			case "not-fallback":
				sess.codexZeroTurnResume = false
			case "accepted-history":
				if err := sess.sendAttempts.put(sendAttemptRecord{ClientActionID: "past", Provider: "codex", NativeThreadID: sess.threadID, Status: sendAttemptAccepted}); err != nil {
					t.Fatal(err)
				}
			}
			if got := sess.codexZeroTurnNativeMissing("current", deliveryErr); got != (scenario == "exact") {
				t.Fatalf("classified=%v", got)
			}
		})
	}
}

func TestReconnectAuditTerminalSettlementAndAvailability(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	sess := reconnectTestSession(t, daemon, "deleted-rollout")
	sess.codexZeroTurnEligible = true
	seedAcceptedTransactionalTurn(t, sess.turns, sess.hostedThreadID, model.ProviderCodex, sess.threadID, "previous-turn", "past-turn", "past")
	turnStore, err := coreturns.OpenStore(context.Background(), sess.queuedTurns.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := turnStore.Finished(context.Background(), coreturns.Finish{
		HostedThreadID: sess.hostedThreadID, Provider: string(model.ProviderCodex), NativeThreadID: sess.threadID,
		TurnID: "past-turn", ActionID: "previous-turn", Outcome: string(model.OutcomeCompleted), Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	store, err := newAgentExecutionStore(filepath.Join(t.TempDir(), "executions.json"), "audit")
	if err != nil {
		t.Fatal(err)
	}
	hosted, _, _ := store.beginSession("terminal", "hash", "codex", presentationBackground)
	sess.hostedThreadID, sess.agentExecutions = hosted.ID, store
	_ = store.bindSession(hosted.ID, sess.id, sess.threadID, "reconnecting", presentationBackground)
	operation, _, err := store.beginTurn(hosted.ID, "terminal-prompt", sendTextHash("retained"))
	if err != nil {
		t.Fatal(err)
	}
	fake.setResumeError("no rollout found for thread deleted-rollout")
	sess.setStatus("reconnecting")
	if _, _, _, err := sess.queueOrStartTurnSend(operation.TransportActionID, "retained"); err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool {
		record, _ := store.turn(operation.ID)
		return record.Status == agentTurnFailed
	})
	record, _ := store.turn(operation.ID)
	if record.Error == nil || record.Error.Code != "reconnect_failed" {
		t.Fatalf("terminal error: %+v", record.Error)
	}
	if response := postTurn(t, sess, context.Background(), "new", "do not queue"); response.Code != http.StatusConflict {
		t.Fatalf("terminal admission: %d", response.Code)
	}
	server := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}}, agentExecutions: store}
	view := server.availabilityFor(hosted.ID, time.Now())
	if view.State != availabilityUnreachable || len(view.Reasons) != 1 || view.Reasons[0] != "reconnect_failed" {
		t.Fatalf("availability: %+v", view)
	}
}

func TestReconnectAuditTransportReplacementDuringBackoff(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	daemon.reconnectBackoff = time.Second
	sess := reconnectTestSession(t, daemon, "replace")
	fake.setResumeError("temporarily busy")
	sess.setStatus("reconnecting")
	daemon.requestSessionRecovery(sess)
	waitReconnectCondition(t, func() bool { return fake.resumeCount() == 1 })
	fake.setResumeError("")
	fake.dropConnections()
	waitReconnectCondition(t, func() bool { return sess.summary().Status == "ready" && fake.resumeCount() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if fake.resumeCount() != 2 {
		t.Fatalf("replacement resumed %d times", fake.resumeCount())
	}
}

func TestReconnectAuditZeroTurnWaitsForSlotWithoutAttemptTimeout(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	sess := reconnectTestSession(t, daemon, "saturated-zero")
	sess.codexZeroTurnResume = true
	for slot := 0; slot < codexResumeConcurrency; slot++ {
		daemon.resumeSlots <- struct{}{}
	}
	sess.startCodexZeroTurnRecovery()
	time.Sleep(codexZeroTurnRecoveryAttemptTimeout + 100*time.Millisecond)
	sess.stateMu.Lock()
	running := sess.codexZeroTurnRecoveryRunning
	sess.stateMu.Unlock()
	if !running {
		t.Error("slot wait ended the zero-turn recovery task before any RPC")
	}
	if fake.resumeCount() != 0 {
		t.Fatal("resume ran without a slot")
	}
	for slot := 0; slot < codexResumeConcurrency; slot++ {
		<-daemon.resumeSlots
	}
	waitReconnectCondition(t, func() bool { return !sess.codexZeroTurnRecoveryNeeded() })
	if fake.resumeCount() != 1 {
		t.Fatalf("slot wait consumed attempts: %d", fake.resumeCount())
	}
}

func TestReconnectAuditRestartJournalExactlyOnce(t *testing.T) {
	for _, status := range []string{sendAttemptQueued, sendAttemptSending} {
		t.Run(status, func(t *testing.T) {
			fake := newFakeAppServer(t)
			fake.setTurnStartResult(map[string]any{"turn": map[string]any{"id": "journal-turn"}})
			daemon := fake.daemon()
			daemon.resumeTimeout = 25 * time.Millisecond
			sess := reconnectTestSession(t, daemon, "journal-"+status)
			root := t.TempDir()
			attemptPath, queuePath := filepath.Join(root, "attempts.json"), filepath.Join(root, "queue.json")
			attempts, err := newTestSendAttemptStore(t, attemptPath, "before-restart")
			if err != nil {
				t.Fatal(err)
			}
			queue, err := newTestQueuedTurnStore(t, queuePath)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = queue.putSubmission(queuedTurnRecord{ClientActionID: "journal-action", Provider: "codex",
				NativeThreadID: sess.threadID, HostedThreadID: sess.hostedThreadID, Text: "retained prompt",
				TextHash: sendTextHash("retained prompt"), RequestHash: sendTextHash("retained prompt"), State: submissionQueued})
			if err != nil {
				t.Fatal(err)
			}
			if err := attempts.put(sendAttemptRecord{ClientActionID: "journal-action", Provider: "codex", NativeThreadID: sess.threadID,
				TextHash: sendTextHash("retained prompt"), Status: status}); err != nil {
				t.Fatal(err)
			}
			sess.sendAttempts, err = newTestSendAttemptStore(t, attemptPath, "after-restart")
			if err != nil {
				t.Fatal(err)
			}
			sess.queuedTurns, err = newTestQueuedTurnStore(t, queuePath)
			if err != nil {
				t.Fatal(err)
			}
			sess.turns = mustBindTransactionalTurnStore(t, sess.queuedTurns, sess.sendAttempts)
			fake.setReplyDelay("thread/resume", 100*time.Millisecond)
			sess.setStatus("starting")
			if err := startCodexSession(sess, daemon, spawnRequest{Provider: "codex", Mode: "resume", ThreadID: sess.threadID, bootReattach: &bootProviderControl{}}); err != nil {
				t.Fatal(err)
			}
			waitReconnectCondition(t, func() bool { return fake.resumeCount() >= 1 })
			fake.setReplyDelay("thread/resume", 0)
			waitReconnectCondition(t, func() bool { return fake.resumeCount() >= 2 && sess.summary().Status == "ready" })
			if status == sendAttemptQueued {
				waitTurnSendDone(t, sess, "journal-action")
				fake.emitThreadNotification(sess.threadID, map[string]any{"method": "item/completed", "params": map[string]any{
					// Codex echoes the client id Nano sent with the prompt.
					"threadId": sess.threadID, "turnId": "journal-turn", "item": map[string]any{"id": "user-journal", "type": "userMessage",
						"clientId": submissionEventID(sess.hostedThreadID, "journal-action"), "content": []map[string]any{{"type": "inputText", "text": "retained prompt"}}}}})
				waitReconnectCondition(t, func() bool {
					attempt, _ := sess.sendAttempts.get("journal-action")
					return attempt.Status == sendAttemptAccepted && attempt.ProviderTurnID == "journal-turn"
				})
			}
			for repeat := 0; repeat < 3; repeat++ {
				sess.kickQueuedTurnDrain()
			}
			time.Sleep(100 * time.Millisecond)
			fake.mu.Lock()
			starts := fake.turnStarts
			fake.mu.Unlock()
			want := 0
			if status == sendAttemptQueued {
				want = 1
			}
			if starts != want {
				t.Fatalf("%s journal: starts=%d want=%d", status, starts, want)
			}
		})
	}
}
