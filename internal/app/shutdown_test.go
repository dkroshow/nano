package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/httpapi/remoteauth"
)

// stoppingServer holds one ready session whose provider answers only when
// the test releases it.
func stoppingServer(t *testing.T) (*server, *session, *turns.Store, chan struct{}, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var sends atomic.Int32
	sess, store := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		sends.Add(1)
		<-release
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	})
	s := &server{sessions: map[string]*session{sess.id: sess}}
	sess.nanoStopping = &s.stopping
	return s, sess, store, release, &sends
}

func startDelivery(t *testing.T, sess *session, sends *atomic.Int32, actionID string) {
	t.Helper()
	go postTurn(t, sess, context.Background(), actionID, "in flight at the stop")
	deadline := time.Now().Add(5 * time.Second)
	for sends.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("delivery never reached the provider")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestStopWaitsBoundedlyForInFlightDeliveries catches a stop that cuts off a
// delivery the provider is about to accept, or one that waits forever for a
// delivery that never answers. Boundary: the server stop against a session
// whose provider call is blocked, with the prompt store as the record.
func TestStopWaitsBoundedlyForInFlightDeliveries(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		s, _, _, _, _ := stoppingServer(t)
		started := time.Now()
		if report := s.stop(time.Second, 2*time.Second); report != (stopReport{}) || time.Since(started) > 100*time.Millisecond {
			t.Fatalf("idle stop = %+v after %s", report, time.Since(started))
		}
	})
	t.Run("finishes within the wait", func(t *testing.T) {
		s, sess, store, release, sends := stoppingServer(t)
		startDelivery(t, sess, sends, "finishing")
		time.AfterFunc(50*time.Millisecond, func() { close(release) })
		report := s.stop(time.Second, 2*time.Second)
		if report != (stopReport{finished: 1}) {
			t.Fatalf("stop = %+v, want one finished delivery", report)
		}
		record, _, err := store.Get(context.Background(), "finishing")
		if err != nil || record.Outbox.State != turns.Accepted {
			t.Fatalf("the accepted outcome was not recorded before exit: %+v, %v", record.Outbox, err)
		}
	})
	t.Run("still running at the wait", func(t *testing.T) {
		s, sess, _, release, sends := stoppingServer(t)
		t.Cleanup(func() { close(release) })
		startDelivery(t, sess, sends, "unanswered")
		started := time.Now()
		report := s.stop(100*time.Millisecond, time.Second)
		if report != (stopReport{unfinished: 1}) || time.Since(started) > 500*time.Millisecond {
			t.Fatalf("stop = %+v after %s, want one unfinished delivery at the wait", report, time.Since(started))
		}
	})
}

// TestStopEndsByItsDeadlineWhenAStepBlocks catches a stop that outlives
// launchd's kill because a session lock is held. Boundary: the server stop,
// with the session's send lock held by another caller.
func TestStopEndsByItsDeadlineWhenAStepBlocks(t *testing.T) {
	s, sess, _, _, _ := stoppingServer(t)
	sess.sendMu.Lock()
	t.Cleanup(sess.sendMu.Unlock)
	started := time.Now()
	report := s.stop(time.Second, 200*time.Millisecond)
	if !report.deadlineReached || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("stop = %+v after %s, want the deadline", report, time.Since(started))
	}
	if stopDeliveryWait >= stopDeadline || stopDeadline >= 5*time.Second {
		t.Fatalf("stop bounds %s/%s do not end before launchd's 5 s kill", stopDeliveryWait, stopDeadline)
	}
}

// TestStoppingNanoQueuesInternalSendsForTheNextProcess catches a delivery that
// starts during the stop, which the exit would cut off and leave uncertain.
// Boundary: session admission and the prompt store, then a new session on the
// same store standing for the next process.
func TestStoppingNanoQueuesInternalSendsForTheNextProcess(t *testing.T) {
	s, sess, store, release, sends := stoppingServer(t)
	close(release)
	s.stopping.Store(true)
	op, status, reason, err := sess.queueOrStartTurnSendPrepared("during-stop", "sent while stopping", "", turnOriginAgent)
	if err != nil || op != nil || status != http.StatusAccepted || reason != "queued_for_next_turn" {
		t.Fatalf("send during stop = %v, %d, %q, %v; want queued", op, status, reason, err)
	}
	record, _, err := store.Get(context.Background(), "during-stop")
	if err != nil || record.Outbox.State != turns.Queued || sends.Load() != 0 {
		t.Fatalf("stored %+v (%v), provider calls %d; want queued and undelivered", record.Outbox, err, sends.Load())
	}

	projection := newLiveProjection("codex", "hosted-thread")
	projection.setNativeThreadID("native-thread")
	next := &session{
		id: "next-process", provider: "codex", hostedThreadID: "hosted-thread", threadID: "native-thread",
		projection: projection, turns: sess.turns, sendAttempts: sess.sendAttempts, queuedTurns: sess.queuedTurns,
		sendOps: map[string]*turnSendOperation{}, turnSender: sess.turnSender,
		lifecycleState: testLifecycleSnapshot("ready", false),
	}
	next.drainQueuedTurn()
	waitTurnSendDone(t, next, "during-stop")
	if sends.Load() != 1 {
		t.Fatalf("the next process delivered the queued prompt %d times, want once", sends.Load())
	}
}

// TestStoppingNanoRefusesWritesInsideTheSecurityGuards catches two failures
// at the mounted listeners during a stop: a write that is recorded or answered
// without a retry hint, and a stop answer that replaces the Host, Origin or
// remote-authentication refusal. Boundary: the handlers application.mount
// serves on both listeners.
func TestStoppingNanoRefusesWritesInsideTheSecurityGuards(t *testing.T) {
	authStore, err := remoteauth.OpenStore(filepath.Join(t.TempDir(), "remote-auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authStore.Close() })
	const remoteHost = "nano.example.ts.net:7690"
	s := &server{sessions: map[string]*session{}}
	app := &application{server: s, remoteAuth: remoteauth.New(authStore, remoteHost)}
	local, remote := app.mount()
	s.stopping.Store(true)

	for _, test := range []struct {
		name, method, host, origin string
		handler                    http.Handler
		want                       int
	}{
		{"local write", http.MethodPost, "127.0.0.1:7690", "", local, http.StatusServiceUnavailable},
		{"local read", http.MethodGet, "127.0.0.1:7690", "", local, http.StatusOK},
		{"cross-site write", http.MethodPost, "127.0.0.1:7690", "https://attacker.example", local, http.StatusForbidden},
		{"untrusted Host", http.MethodPost, "attacker.example:7690", "", local, http.StatusMisdirectedRequest},
		{"unauthenticated remote write", http.MethodPost, remoteHost, "", remote, http.StatusUnauthorized},
		{"remote wrong Host", http.MethodPost, "unexpected.invalid", "", remote, http.StatusMisdirectedRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := "/api/v1/build"
			if test.method == http.MethodPost {
				path = "/api/v1/hosted-threads/nht_stop/turns"
			}
			request := httptest.NewRequest(test.method, "http://localhost:7690"+path, strings.NewReader(`{"clientActionId":"a","prompt":"p"}`))
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			test.handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if test.want != http.StatusServiceUnavailable {
				return
			}
			var body struct {
				Error struct{ Code string } `json:"error"`
			}
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != "restarting" || response.Header().Get("Retry-After") != "10" {
				t.Fatalf("stop answer = %q, Retry-After %q; want code restarting, Retry-After 10", response.Body.String(), response.Header().Get("Retry-After"))
			}
		})
	}
}

// TestStoppingNanoRefusesRequireIdleSubmissions catches a heartbeat or
// observer prompt that a stop queues instead of refusing, which breaks the
// requireIdle promise that a refusal creates no operation. Boundary: the
// hosted turn API, called in process as those features call it.
func TestStoppingNanoRefusesRequireIdleSubmissions(t *testing.T) {
	s, _, _, _, target, prompts := stage0Server(t)
	s.sessionForHostedThread(target.ID).nanoStopping = &s.stopping
	s.stopping.Store(true)
	body, _ := json.Marshal(map[string]any{
		"prompt": "continue", "clientActionId": "nano:heartbeat:stop:1", "origin": "system", "requireIdle": true,
	})
	recorder := httptest.NewRecorder()
	s.handleAgentTurnSubmit(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+target.ID+"/turns", strings.NewReader(string(body))), target.ID)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "notReady") {
		t.Fatalf("requireIdle during stop = %d %s, want 409 notReady", recorder.Code, recorder.Body.String())
	}
	if _, found := s.agentExecutions.turnForAction(target.ID, "nano:heartbeat:stop:1"); found {
		t.Fatal("a refused requireIdle submission created an operation")
	}
	select {
	case delivered := <-prompts:
		t.Fatalf("delivered %q during the stop", delivered)
	case <-time.After(100 * time.Millisecond):
	}
}
