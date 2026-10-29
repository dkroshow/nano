package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodexReconnectFleetStorm(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	daemon.resumeTimeout = 200 * time.Millisecond
	if err := daemon.ensure(); err != nil {
		t.Fatal(err)
	}
	sessions := reconnectTestFleet(t, daemon, "storm", 123)
	fake.SetReplyDelay("thread/resume", time.Second)
	fake.DropConnections()
	waitReconnectCondition(t, func() bool { return fake.ResumeCount() >= codexResumeConcurrency })
	time.Sleep(30 * time.Millisecond)
	if got := fake.ResumeCount(); got != codexResumeConcurrency {
		t.Fatalf("restart launched %d resumes before any response; limit %d", got, codexResumeConcurrency)
	}
	fake.SetReplyDelay("thread/resume", 0)
	waitReconnectCondition(t, func() bool {
		for _, sess := range sessions {
			if sess.Summary().Status != "ready" {
				return false
			}
		}
		return fake.ResumeCount() >= len(sessions)+codexResumeConcurrency
	})
	for index, sess := range sessions {
		want := fmt.Sprintf("storm-%03d", index)
		if sess.stateOwner.ThreadID() != want || sess.HostedThreadID() != "hosted-"+want {
			t.Fatalf("identity changed: %+v", sess.Summary())
		}
	}
}

func TestCodexReconnectPromptRetriesAndDrainsOnce(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	daemon.resumeTimeout = 30 * time.Millisecond
	sess := resumeReconnectFixture(t, daemon, "queued-recovery", false)
	waitForClaudeStatus(t, sess, "ready")
	var deliveries atomic.Int32
	sess.delivery.turnSender = func(context.Context, string) (json.RawMessage, error) {
		sequence := deliveries.Add(1)
		return json.RawMessage(fmt.Sprintf(`{"turn":{"id":"recovered-turn-%d"}}`, sequence)), nil
	}
	sess.setStatus("reconnecting")
	fake.SetReplyDelay("thread/resume", 100*time.Millisecond)
	for index := 0; index < 2; index++ {
		response := postTurn(t, sess.Delivery(), context.Background(), "recovery-action", "preserve this prompt")
		if response.Code != http.StatusAccepted {
			t.Fatalf("queue failed: %d %s", response.Code, response.Body.String())
		}
	}
	if response := postTurn(t, sess.Delivery(), context.Background(), "second-recovery-action", "preserve the second prompt"); response.Code != http.StatusAccepted {
		t.Fatalf("second queue failed: %d %s", response.Code, response.Body.String())
	}
	waitReconnectCondition(t, func() bool { return fake.ResumeCount() >= 2 })
	fake.SetReplyDelay("thread/resume", 0)
	waitTurnSendDone(t, sess, "recovery-action")
	if got := fake.ResumeCount(); got != 3 {
		t.Fatalf("resume count = %d; expected initial, timeout, retry", got)
	}
	for index := 0; index < 5; index++ {
		sess.delivery.kickQueuedTurnDrain()
		daemon.requestSessionRecovery(sess)
	}
	time.Sleep(50 * time.Millisecond)
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("provider deliveries = %d, want exactly one", got)
	}
	if queued, ok := sess.delivery.queuedTurns.Get("recovery-action"); !ok || queued.State != submissionAccepted {
		t.Fatalf("submission lost or not accepted: %+v", queued)
	}
	sess.stateOwner.setTurnActive(true, "recovered-turn-1", time.Now().UnixMilli(), true)
	observeTestTurnCompletion(t, sess, "recovered-turn-1")
	sess.stateOwner.setTurnActive(false, "", 0, true)
	waitTurnSendDone(t, sess, "second-recovery-action")
	if got := deliveries.Load(); got != 2 {
		t.Fatalf("two queued prompts produced %d deliveries", got)
	}
	if queued, ok := sess.delivery.queuedTurns.Get("second-recovery-action"); !ok || queued.State != submissionAccepted {
		t.Fatalf("second submission lost or not accepted: %+v", queued)
	}
}

func TestCodexReconnectNoRolloutStopsWithoutLosingIdentity(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	sess := resumeReconnectFixture(t, daemon, "missing-rollout", false)
	waitForClaudeStatus(t, sess, "ready")
	fake.SetResumeError("no rollout found for thread id missing-rollout")
	sess.setStatus("reconnecting")
	daemon.requestSessionRecovery(sess)
	waitReconnectCondition(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		return !daemon.reconnecting[sess.ID()]
	})
	time.Sleep(300 * time.Millisecond)
	if fake.ResumeCount() != 2 || sess.stateOwner.ThreadID() != "missing-rollout" || sess.Summary().Status != "reconnect_failed" {
		t.Fatal("missing rollout was retried automatically or changed identity/status")
	}
	fake.SetResumeError("")
	daemon.reconnectSessions()
	// Confirmed native absence stays unreachable for this attachment. A new
	// explicit resume must allocate a new durable epoch before readiness.
	time.Sleep(100 * time.Millisecond)
	if got := sess.Summary().Status; got != "reconnect_failed" {
		t.Fatalf("late ready reopened unreachable attachment: %s", got)
	}
}

func TestCodexReconnectBootTimeoutRecovers(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.SetReplyDelay("thread/resume", 100*time.Millisecond)
	daemon := fake.daemon()
	daemon.resumeTimeout = 30 * time.Millisecond
	sess := resumeReconnectFixture(t, daemon, "boot-timeout", true)
	waitReconnectCondition(t, func() bool { return fake.ResumeCount() >= 1 })
	fake.SetReplyDelay("thread/resume", 0)
	waitForClaudeStatus(t, sess, "ready")
	if fake.ResumeCount() < 2 || sess.stateOwner.ThreadID() != "boot-timeout" {
		t.Fatal("boot timeout did not retry the original thread")
	}
}

func TestCodexReconnectClosedSessionStopsRetrying(t *testing.T) {
	fake := newFakeAppServer(t)
	daemon := fake.daemon()
	sess := resumeReconnectFixture(t, daemon, "closed-retry", false)
	waitForClaudeStatus(t, sess, "ready")
	fake.SetResumeError("provider temporarily busy")
	sess.setStatus("reconnecting")
	daemon.requestSessionRecovery(sess)
	waitReconnectCondition(t, func() bool { return fake.ResumeCount() == 2 })
	sess.Close()
	waitReconnectCondition(t, func() bool {
		daemon.mu.Lock()
		defer daemon.mu.Unlock()
		return !daemon.reconnecting[sess.ID()]
	})
	if fake.ResumeCount() != 2 || sess.Summary().Status != StatusExited {
		t.Fatal("closed session was retried or resurrected")
	}
}
func resumeReconnectFixture(t *testing.T, daemon *CodexDaemon, native string, boot bool) *Session {
	t.Helper()
	sess := reconnectTestSession(t, daemon, native)
	sess.setStatus("starting")
	request := ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: native}
	if boot {
		request.BootReattach = &BootControl{}
	}
	if err := sess.runtime.BeginProviderSpawn(request, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	return sess
}
