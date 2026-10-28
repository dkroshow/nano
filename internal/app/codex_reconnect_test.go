package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func waitReconnectCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reconnect condition did not settle")
}

func TestCodexReconnectFleetStorm(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	daemon.resumeTimeout = 200 * time.Millisecond
	if err := daemon.ensure(); err != nil {
		t.Fatal(err)
	}
	sessions := reconnectTestFleet(t, daemon, "storm", 123)
	fake.setReplyDelay("thread/resume", time.Second)
	fake.dropConnections()
	waitReconnectCondition(t, func() bool { return fake.resumeCount() >= codexResumeConcurrency })
	time.Sleep(30 * time.Millisecond)
	if got := fake.resumeCount(); got != codexResumeConcurrency {
		t.Fatalf("restart launched %d resumes before any response; limit %d", got, codexResumeConcurrency)
	}
	fake.setReplyDelay("thread/resume", 0)
	waitReconnectCondition(t, func() bool {
		for _, sess := range sessions {
			if sess.summary().Status != "ready" {
				return false
			}
		}
		return fake.resumeCount() >= len(sessions)+codexResumeConcurrency
	})
	for index, sess := range sessions {
		want := fmt.Sprintf("storm-%03d", index)
		if sess.currentThreadID() != want || sess.hostedThreadID != "hosted-"+want {
			t.Fatalf("identity changed: %+v", sess.summary())
		}
	}
}

func TestCodexReconnectPromptRetriesAndDrainsOnce(t *testing.T) {
	fake := newFakeAppServer(t)
	server := newResumeTestServer(t, fake.wsURL())
	server.codex.resumeTimeout = 30 * time.Millisecond
	var err error
	server.sendAttempts, err = newTestSendAttemptStore(t, filepath.Join(t.TempDir(), "attempts.json"), "reconnect-test")
	if err != nil {
		t.Fatal(err)
	}
	server.queuedTurns = mustQueuedTurnStore(t)
	server.turns = mustBindTransactionalTurnStore(t, server.queuedTurns, server.sendAttempts)
	response := postResume(t, server, "queued-recovery")
	var summary sessionSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, server, summary.ID, "ready")
	sess := server.sessions[summary.ID]
	var deliveries atomic.Int32
	sess.turnSender = func(context.Context, string) (json.RawMessage, error) {
		sequence := deliveries.Add(1)
		return json.RawMessage(fmt.Sprintf(`{"turn":{"id":"recovered-turn-%d"}}`, sequence)), nil
	}
	sess.setStatus("reconnecting")
	fake.setReplyDelay("thread/resume", 100*time.Millisecond)
	for index := 0; index < 2; index++ {
		response := postTurn(t, sess, context.Background(), "recovery-action", "preserve this prompt")
		if response.Code != http.StatusAccepted {
			t.Fatalf("queue failed: %d %s", response.Code, response.Body.String())
		}
	}
	if response := postTurn(t, sess, context.Background(), "second-recovery-action", "preserve the second prompt"); response.Code != http.StatusAccepted {
		t.Fatalf("second queue failed: %d %s", response.Code, response.Body.String())
	}
	waitReconnectCondition(t, func() bool { return fake.resumeCount() >= 2 })
	fake.setReplyDelay("thread/resume", 0)
	waitTurnSendDone(t, sess, "recovery-action")
	if got := fake.resumeCount(); got != 3 {
		t.Fatalf("resume count = %d; expected initial, timeout, retry", got)
	}
	for index := 0; index < 5; index++ {
		sess.kickQueuedTurnDrain()
		server.codex.requestSessionRecovery(sess)
	}
	time.Sleep(50 * time.Millisecond)
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("provider deliveries = %d, want exactly one", got)
	}
	if queued, ok := sess.queuedTurns.Get("recovery-action"); !ok || queued.State != submissionAccepted {
		t.Fatalf("submission lost or not accepted: %+v", queued)
	}
	sess.setTurnActive(true, "recovered-turn-1", time.Now().UnixMilli(), true)
	observeTestTurnCompletion(t, sess, "recovered-turn-1")
	sess.setTurnActive(false, "", 0, true)
	waitTurnSendDone(t, sess, "second-recovery-action")
	if got := deliveries.Load(); got != 2 {
		t.Fatalf("two queued prompts produced %d deliveries", got)
	}
	if queued, ok := sess.queuedTurns.Get("second-recovery-action"); !ok || queued.State != submissionAccepted {
		t.Fatalf("second submission lost or not accepted: %+v", queued)
	}
}

func TestCodexReconnectNoRolloutStopsWithoutLosingIdentity(t *testing.T) {
	fake := newFakeAppServer(t)
	server := newResumeTestServer(t, fake.wsURL())
	response := postResume(t, server, "missing-rollout")
	var summary sessionSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, server, summary.ID, "ready")
	sess := server.sessions[summary.ID]
	fake.setResumeError("no rollout found for thread id missing-rollout")
	sess.setStatus("reconnecting")
	server.codex.requestSessionRecovery(sess)
	waitReconnectCondition(t, func() bool {
		server.codex.mu.Lock()
		defer server.codex.mu.Unlock()
		return !server.codex.reconnecting[sess.id]
	})
	time.Sleep(300 * time.Millisecond)
	if fake.resumeCount() != 2 || sess.currentThreadID() != "missing-rollout" || sess.summary().Status != "reconnect_failed" {
		t.Fatal("missing rollout was retried automatically or changed identity/status")
	}
	fake.setResumeError("")
	server.codex.reconnectSessions()
	// Confirmed native absence stays unreachable for this attachment. A new
	// explicit resume must allocate a new durable epoch before readiness.
	time.Sleep(100 * time.Millisecond)
	if got := sess.summary().Status; got != "reconnect_failed" {
		t.Fatalf("late ready reopened unreachable attachment: %s", got)
	}
}

func TestCodexReconnectBootTimeoutRecovers(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setReplyDelay("thread/resume", 100*time.Millisecond)
	server := newResumeTestServer(t, fake.wsURL())
	server.codex.resumeTimeout = 30 * time.Millisecond
	seedCanonicalResumeOwner(t, server, "codex", "boot-timeout", server.defaults.cwd)
	sess, err := server.spawnSession(spawnRequest{providerRequest: providerRequest{Provider: "codex", Mode: "resume", ThreadID: "boot-timeout", bootReattach: &bootProviderControl{}}})
	if err != nil {
		t.Fatal(err)
	}
	waitReconnectCondition(t, func() bool { return fake.resumeCount() >= 1 })
	fake.setReplyDelay("thread/resume", 0)
	waitForSessionStatus(t, server, sess.id, "ready")
	if fake.resumeCount() < 2 || sess.currentThreadID() != "boot-timeout" {
		t.Fatal("boot timeout did not retry the original thread")
	}
}

func TestCodexReconnectClosedSessionStopsRetrying(t *testing.T) {
	fake := newFakeAppServer(t)
	server := newResumeTestServer(t, fake.wsURL())
	response := postResume(t, server, "closed-retry")
	var summary sessionSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, server, summary.ID, "ready")
	sess := server.sessions[summary.ID]
	fake.setResumeError("provider temporarily busy")
	sess.setStatus("reconnecting")
	server.codex.requestSessionRecovery(sess)
	waitReconnectCondition(t, func() bool { return fake.resumeCount() == 2 })
	sess.close()
	waitReconnectCondition(t, func() bool {
		server.codex.mu.Lock()
		defer server.codex.mu.Unlock()
		return !server.codex.reconnecting[sess.id]
	})
	if fake.resumeCount() != 2 || sess.summary().Status != sessionStatusExited {
		t.Fatal("closed session was retried or resurrected")
	}
}
