package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

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
		projection:     newDeliveryTestProjection("codex"),
		sendOps:        map[string]*turnSendOperation{},
		turns:          queue.TransactionStore(),
		queuedTurns:    queue,
		sendAttempts:   attempts,
		codex:          daemon,
		lifecycleState: testLifecycleSnapshot("ready", false)}
	if err := daemon.claimThread(threadID, sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sess.Close()
		waitReconnectCondition(t, func() bool {
			daemon.mu.Lock()
			defer daemon.mu.Unlock()
			return !daemon.reconnecting[sess.ID()]
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
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "rearmed-turn"}})
	daemon := fake.daemon()
	daemon.resumeTimeout, daemon.reconnectBackoff = 100*time.Millisecond, time.Millisecond
	if err := daemon.ensure(); err != nil {
		t.Fatal(err)
	}
	sessions := reconnectTestFleet(t, daemon, "fleet", 123)
	for index := range sessions {
		if index < 6 {
			fake.ResumeDelays[sessions[index].ThreadID()] = time.Second
		}
	}
	fake.DropConnections()
	waitReconnectCondition(t, func() bool {
		for index, sess := range sessions {
			want := "ready"
			if index < 6 {
				want = "reconnect_parked"
			}
			if sess.Summary().Status != want {
				return false
			}
		}
		return true
	})
	fake.Mu.Lock()
	for index, sess := range sessions {
		want := 1
		if index < 6 {
			want = codexReconnectAttempts
		}
		count := fake.ResumeCounts[sess.ThreadID()]
		if (index < 6 && count != want) || (index >= 6 && (count < 1 || count > codexReconnectAttempts)) {
			t.Errorf("%s attempts=%d want=%d", sess.ThreadID(), fake.ResumeCounts[sess.ThreadID()], want)
		}
	}
	fake.ResumeDelays[sessions[0].ThreadID()] = 0
	fake.Mu.Unlock()
	for repeat := 0; repeat < 2; repeat++ {
		response := postTurn(t, sessions[0], context.Background(), "rearm", "try after parking")
		if response.Code != http.StatusAccepted && response.Code != http.StatusOK {
			t.Fatalf("parked prompt: %d %s", response.Code, response.Body.String())
		}
	}
	waitTurnSendDone(t, sessions[0], "rearm")
	fake.Mu.Lock()
	defer fake.Mu.Unlock()
	if fake.ResumeCounts[sessions[0].ThreadID()] != codexReconnectAttempts+1 || fake.TurnStarts != 1 {
		t.Fatalf("rearm duplicates: resumes=%d starts=%d", fake.ResumeCounts[sessions[0].ThreadID()], fake.TurnStarts)
	}
}

func TestReconnectAuditZeroTurnReadyAndSingleDelivery(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "zero-turn-first"}})
	daemon := fake.daemon()
	sess := reconnectTestSession(t, daemon, "zero-turn")
	sess.codexZeroTurnEligible = true
	sess.setStatus("reconnecting")
	fake.SetResumeErrorSequence("no rollout found for thread zero-turn", "")
	if response := postTurn(t, sess, context.Background(), "first", "first prompt"); response.Code != http.StatusAccepted {
		t.Fatalf("queue: %s", response.Body.String())
	}
	waitTurnSendDone(t, sess, "first")
	waitReconnectCondition(t, func() bool { return !sess.codexZeroTurnRecoveryNeeded() })
	fake.Mu.Lock()
	defer fake.Mu.Unlock()
	if fake.TurnStarts != 1 || sess.Summary().Status != "ready" {
		t.Fatalf("zero-turn recovery: starts=%d status=%s", fake.TurnStarts, sess.Summary().Status)
	}
	attempt, _ := sess.sendAttempts.Get("first")
	if attempt.Status != sendAttemptAccepted || attempt.Reason == codexNativeMissing {
		t.Fatalf("daemon-memory first delivery: %+v", attempt)
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
				if err := JournalPut(sess.sendAttempts.(*sendAttemptStore), sendAttemptRecord{ClientActionID: "past", Provider: "codex", NativeThreadID: sess.ThreadID(), Status: sendAttemptAccepted}); err != nil {
					t.Fatal(err)
				}
			}
			if got := sess.codexZeroTurnNativeMissing("current", deliveryErr); got != (scenario == "exact") {
				t.Fatalf("classified=%v", got)
			}
		})
	}
}

func TestReconnectAuditTransportReplacementDuringBackoff(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	daemon.reconnectBackoff = time.Second
	sess := reconnectTestSession(t, daemon, "replace")
	fake.SetResumeError("temporarily busy")
	sess.setStatus("reconnecting")
	daemon.requestSessionRecovery(sess)
	waitReconnectCondition(t, func() bool { return fake.ResumeCount() == 1 })
	fake.SetResumeError("")
	fake.DropConnections()
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "ready" && fake.ResumeCount() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if fake.ResumeCount() != 2 {
		t.Fatalf("replacement resumed %d times", fake.ResumeCount())
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
	if fake.ResumeCount() != 0 {
		t.Fatal("resume ran without a slot")
	}
	for slot := 0; slot < codexResumeConcurrency; slot++ {
		<-daemon.resumeSlots
	}
	waitReconnectCondition(t, func() bool { return !sess.codexZeroTurnRecoveryNeeded() })
	if fake.ResumeCount() != 1 {
		t.Fatalf("slot wait consumed attempts: %d", fake.ResumeCount())
	}
}

func TestReconnectAuditRestartJournalExactlyOnce(t *testing.T) {
	for _, status := range []string{sendAttemptQueued, sendAttemptSending} {
		t.Run(status, func(t *testing.T) {
			fake := newFakeAppServer(t)
			fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "journal-turn"}})
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
			_, _, err = QueuePutSubmission(queue, queuedTurnRecord{ClientActionID: "journal-action", Provider: "codex",
				NativeThreadID: sess.ThreadID(), HostedThreadID: sess.HostedThreadID(), Text: "retained prompt",
				TextHash: sendTextHash("retained prompt"), RequestHash: sendTextHash("retained prompt"), State: submissionQueued})
			if err != nil {
				t.Fatal(err)
			}
			if err := JournalPut(attempts, sendAttemptRecord{ClientActionID: "journal-action", Provider: "codex", NativeThreadID: sess.ThreadID(),
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
			fake.SetReplyDelay("thread/resume", 100*time.Millisecond)
			sess.setStatus("starting")
			if err := startCodexSession(sess, daemon, providerRequest{Provider: "codex", Mode: "resume", ThreadID: sess.ThreadID(), BootReattach: &bootProviderControl{}}); err != nil {
				t.Fatal(err)
			}
			waitReconnectCondition(t, func() bool { return fake.ResumeCount() >= 1 })
			fake.SetReplyDelay("thread/resume", 0)
			waitReconnectCondition(t, func() bool { return fake.ResumeCount() >= 2 && sess.Summary().Status == "ready" })
			if status == sendAttemptQueued {
				waitTurnSendDone(t, sess, "journal-action")
				fake.EmitThreadNotification(sess.ThreadID(), map[string]any{"method": "item/completed", "params": map[string]any{
					// Codex echoes the client id Nano sent with the prompt.
					"threadId": sess.ThreadID(), "turnId": "journal-turn", "item": map[string]any{"id": "user-journal", "type": "userMessage",
						"clientId": submissionEventID(sess.HostedThreadID(), "journal-action"), "content": []map[string]any{{"type": "inputText", "text": "retained prompt"}}}}})
				waitReconnectCondition(t, func() bool {
					attempt, _ := sess.sendAttempts.Get("journal-action")
					return attempt.Status == sendAttemptAccepted && attempt.ProviderTurnID == "journal-turn"
				})
			}
			for repeat := 0; repeat < 3; repeat++ {
				sess.kickQueuedTurnDrain()
			}
			time.Sleep(100 * time.Millisecond)
			fake.Mu.Lock()
			starts := fake.TurnStarts
			fake.Mu.Unlock()
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
