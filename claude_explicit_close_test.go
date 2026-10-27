package main

import (
	"context"
	"encoding/json"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeForgetUnconfirmedIsExplicitAndGenerationScoped(t *testing.T) {
	for _, scenario := range []string{"forget", "missing_identity", "replacement", "resumed", "bad_provider", "missing_capture"} {
		t.Run(scenario, func(t *testing.T) {
			store := newTestStore(t)
			record := attachmentRecord{Provider: "claude", NativeThreadID: "native-1", ShimSessionID: "old-process", AttachedAt: "generation-1"}
			if scenario == "missing_identity" {
				record.ShimSessionID = ""
			}
			store.remember(record)
			s := &server{sessions: map[string]*session{}, attachments: store}
			query := url.Values{"provider": {"claude"}, "forget": {"unconfirmed"}, "expectedShimSessionId": {record.ShimSessionID}, "expectedAttachedAt": {record.AttachedAt}}
			switch scenario {
			case "replacement":
				record.ShimSessionID = "new-process"
				store.remember(record)
			case "resumed":
				s.sessions["live"] = &session{id: "live", provider: "claude", threadID: "native-1", lifecycleState: testLifecycleSnapshot("ready", false)}
			case "bad_provider":
				query.Set("provider", "codex")
			case "missing_capture":
				query.Del("expectedShimSessionId")
			}
			w := httptest.NewRecorder()
			applicationHandler(s).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/attachments/native-1?"+query.Encode(), nil))
			_, retained := store.get("claude", "native-1")
			if scenario == "forget" || scenario == "missing_identity" {
				if w.Code != 200 || retained || !strings.Contains(w.Body.String(), `"processState":"unknown"`) {
					t.Fatalf("forget=%d %s retained=%v", w.Code, w.Body, retained)
				}
			} else if w.Code < 400 || !retained {
				t.Fatalf("unsafe forget=%d %s retained=%v", w.Code, w.Body, retained)
			}
		})
	}
}

func TestClaudeExplicitDetachedCloseRequiresAcknowledgement(t *testing.T) {
	for _, outcome := range []string{"success", "not_found", "disconnect", "missing_identity"} {
		t.Run(outcome, func(t *testing.T) {
			shim := newFakeShim(t, func(c *shimConn) {
				call, ok := c.next()
				if !ok {
					return
				}
				if outcome == "not_found" {
					c.replyError(call, shimCodeSessionNotFound, "session_not_found")
				} else if outcome != "disconnect" {
					c.reply(call, map[string]any{})
				}
			})
			store := newTestStore(t)
			attachment := attachmentRecord{Provider: "claude", NativeThreadID: "native-1", ShimSessionID: "exact-process"}
			if outcome == "missing_identity" {
				attachment.ShimSessionID = ""
			}
			store.remember(attachment)
			s := &server{sessions: map[string]*session{}, attachments: store}
			s.defaults.claudeShimWS = shim.wsURL()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/api/attachments/native-1?provider=claude", nil)
			applicationHandler(s).ServeHTTP(w, r)
			_, retained := store.get("claude", "native-1")
			if outcome == "success" {
				if w.Code != 200 || retained {
					t.Fatalf("close = %d %s retained=%v", w.Code, w.Body, retained)
				}
			} else if w.Code < 400 || !retained {
				t.Fatalf("uncertain close = %d %s retained=%v", w.Code, w.Body, retained)
			}
			calls := shim.callsOf(shimMethodClose)
			if outcome != "missing_identity" {
				if len(calls) != 1 {
					t.Fatalf("close calls=%v", calls)
				}
				var params map[string]string
				_ = json.Unmarshal(calls[0].Params, &params)
				if len(params) != 1 || params["shimSessionId"] != "exact-process" {
					t.Fatalf("close identity=%v", params)
				}
			}
			for _, method := range []string{shimMethodAttach, shimMethodStart, shimMethodInput} {
				if len(shim.callsOf(method)) != 0 {
					t.Fatalf("close sent %s", method)
				}
			}
		})
	}
}

func TestClaudeExplicitCloseWaitsBeyondShimGrace(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.next()
		if !ok {
			return
		}
		time.Sleep(3100 * time.Millisecond)
		c.reply(call, map[string]any{})
	})
	if err := closeDetachedClaude(shim.wsURL(), "slow-process"); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeExplicitClosePreservesReplacement(t *testing.T) {
	store := newTestStore(t)
	old := attachmentRecord{Provider: "claude", NativeThreadID: "native-1", ShimSessionID: "old-process"}
	store.remember(old)
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.next()
		if !ok {
			return
		}
		replacement := old
		replacement.ShimSessionID = "replacement"
		store.remember(replacement)
		c.reply(call, map[string]any{})
	})
	s := &server{attachments: store}
	s.defaults.claudeShimWS = shim.wsURL()
	if err := s.closeDetachedAttachment(old); err == nil {
		t.Fatal("replacement was not detected")
	}
	current, ok := store.get("claude", "native-1")
	if !ok || current.ShimSessionID != "replacement" {
		t.Fatalf("replacement lost: %+v", current)
	}
}

func TestClaudeExplicitHostedFailurePreservesOperations(t *testing.T) {
	for _, endpoint := range []string{"close", "delete", "family", "attachment"} {
		t.Run(endpoint, func(t *testing.T) {
			s, store, _ := newBulkCloseTestServer(t)
			hosted, _, err := store.beginSession("close-test", "hash", "claude", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.bindSession(hosted.ID, "old-nano", "native-1", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := store.beginTurn(hosted.ID, "queued-action", "hash")
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.session(hosted.ID)
			s.queuedTurns = mustQueuedTurnStore(t)
			s.queuedTurns.turns, err = turns.OpenStore(context.Background(), s.queuedTurns.db)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.queuedTurns.turns.Submit(context.Background(), turns.Submission{
				ActionID: "still-queued", Provider: "claude", HostedThreadID: hosted.ID,
				NativeThreadID: "native-1", ProviderText: "not sent", TextHash: "hash", RequestHash: "hash",
			}); err != nil {
				t.Fatal(err)
			}
			s.attachments = newTestStore(t)
			s.attachments.remember(attachmentRecord{Provider: "claude", NativeThreadID: "native-1", HostedThreadID: hosted.ID, ShimSessionID: "exact-process"})
			var reject atomic.Bool
			reject.Store(true)
			shim := newFakeShim(t, func(c *shimConn) {
				call, ok := c.next()
				if !ok {
					return
				}
				if reject.Load() {
					c.replyError(call, -32099, "stop refused")
				} else {
					c.reply(call, map[string]any{})
				}
			})
			s.defaults.claudeShimWS = shim.wsURL()
			request := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				method, path, body := http.MethodPost, "/api/v1/hosted-threads/"+hosted.ID+"/close", ""
				switch endpoint {
				case "delete":
					method, path = http.MethodDelete, "/api/v1/hosted-threads/"+hosted.ID
				case "family":
					path, body = "/api/v1/hosted-threads/"+hosted.ID+"/family/close", `{"starredPolicy":"preserve"}`
				case "attachment":
					method, path = http.MethodDelete, "/api/attachments/native-1?provider=claude"
				}
				applicationHandler(s).ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
				return w
			}
			w := request()
			if w.Code < 400 && w.Code != 207 {
				t.Fatalf("failed stop reported success: %d %s", w.Code, w.Body)
			}
			after, ok := store.session(hosted.ID)
			if !ok || after.Status != before.Status {
				t.Fatalf("failed stop settled session: %+v", after)
			}
			turn, _ := store.turn(op.ID)
			if turn.Status != op.Status {
				t.Fatalf("failed stop settled operation: %+v", turn)
			}
			if _, ok := s.attachments.get("claude", "native-1"); !ok {
				t.Fatal("failed stop erased attachment")
			}
			if _, ok := s.queuedTurns.first("claude", "native-1", hosted.ID); !ok {
				t.Fatal("failed stop removed queued prompt")
			}
			reject.Store(false)
			w = request()
			if w.Code != 200 {
				t.Fatalf("retry failed: %d %s", w.Code, w.Body)
			}
			if _, ok := s.attachments.get("claude", "native-1"); ok {
				t.Fatal("successful retry retained attachment")
			}
			if _, ok := s.queuedTurns.first("claude", "native-1", hosted.ID); ok {
				t.Fatal("successful retry left queued prompt")
			}
		})
	}
}

// Model the real shim's take-before-reap interval. A concurrent raw close
// sees not_found while the first process is demonstrably not reaped.
func TestClaudeExplicitNotFoundBeforeReapIsUncertain(t *testing.T) {
	started, reap := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.next()
		if !ok {
			return
		}
		if count.Add(1) == 1 {
			close(started)
			<-reap
			c.reply(call, map[string]any{})
		} else {
			c.replyError(call, shimCodeSessionNotFound, "session_not_found")
		}
	})
	first := make(chan error, 1)
	go func() { first <- closeDetachedClaude(shim.wsURL(), "same-process") }()
	<-started
	err := closeDetachedClaude(shim.wsURL(), "same-process")
	close(reap)
	if err == nil {
		t.Error("registry absence incorrectly proved process exit")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestClaudeExplicitConcurrentClosesSendOneStop(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	shim := newFakeShim(t, func(c *shimConn) {
		call, ok := c.next()
		if !ok {
			return
		}
		close(started)
		<-finish
		c.reply(call, map[string]any{})
	})
	s := &server{sessions: map[string]*session{}, attachments: newTestStore(t)}
	s.defaults.claudeShimWS = shim.wsURL()
	s.attachments.remember(attachmentRecord{Provider: "claude", NativeThreadID: "cs_provisional", ShimSessionID: "cs_provisional"})
	results := make(chan int, 2)
	request := func() {
		w := httptest.NewRecorder()
		applicationHandler(s).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/attachments/cs_provisional?provider=claude", nil))
		results <- w.Code
	}
	go request()
	<-started
	go request()
	close(finish)
	for range 2 {
		if code := <-results; code != 200 {
			t.Fatalf("close status %d", code)
		}
	}
	if calls := shim.callsOf(shimMethodClose); len(calls) != 1 {
		t.Fatalf("duplicate stops: %v", calls)
	}
}

func TestClaudeExplicitStartingCloseDoesNotClaimSuccess(t *testing.T) {
	sess, _ := claudeTestSession(t)
	s := &server{sessions: map[string]*session{sess.id: sess}}
	w := httptest.NewRecorder()
	s.handleSessionClose(w, httptest.NewRequest(http.MethodDelete, "/", nil), sess)
	if w.Code != 502 || s.sessions[sess.id] != sess || sess.closed {
		t.Fatalf("starting close lost in-flight attachment: %d %s", w.Code, w.Body)
	}
}

func TestClaudeExplicitLiveFailureKeepsRetryIdentity(t *testing.T) {
	shim := newFakeShim(t, func(c *shimConn) {
		for {
			call, ok := c.next()
			if !ok {
				return
			}
			if call.Method == shimMethodClose {
				c.replyError(call, -32099, "stop refused")
			} else {
				c.reply(call, map[string]any{"shimSessionId": "shim-1"})
			}
		}
	})
	sess, _ := claudeTestSession(t)
	store := newTestAgentExecutionStore(t, "close-test")
	hosted, _, _ := store.beginSession("live-close", "hash", "claude", presentationShared)
	sess.hostedThreadID, sess.agentExecutions = hosted.ID, store
	if _, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.wsURL())); err != nil {
		t.Fatal(err)
	}
	_ = store.bindSession(hosted.ID, sess.id, sess.currentThreadID(), "ready", presentationShared)
	op, _, _ := store.beginTurn(hosted.ID, "pending", "hash")
	attachments := newTestStore(t)
	attachments.remember(attachmentRecord{Provider: "claude", NativeThreadID: sess.currentThreadID(), HostedThreadID: hosted.ID, ShimSessionID: "shim-1"})
	s := &server{sessions: map[string]*session{sess.id: sess}, attachments: attachments, agentExecutions: store}
	w := httptest.NewRecorder()
	s.handleSessionClose(w, httptest.NewRequest(http.MethodDelete, "/", nil), sess)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("close = %d %s", w.Code, w.Body)
	}
	if _, ok := attachments.get("claude", "shim-1"); !ok {
		t.Fatal("retry identity lost")
	}
	if s.sessions[sess.id] != nil {
		t.Fatal("failed local connection retained as live")
	}
	turn, _ := store.turn(op.ID)
	if turn.Status != op.Status {
		t.Fatalf("failed close settled turn: %+v", turn)
	}
	record, _ := store.session(hosted.ID)
	if record.Status != "ready" {
		t.Fatalf("failed close settled hosted record: %+v", record)
	}
}

func TestClaudeInternalCloseSettlesOnlyAfterAcknowledgement(t *testing.T) {
	for _, success := range []bool{false, true} {
		name := "refused"
		if success {
			name = "confirmed"
		}
		t.Run(name, func(t *testing.T) {
			shim := newFakeShim(t, func(c *shimConn) {
				for {
					call, ok := c.next()
					if !ok {
						return
					}
					if call.Method == shimMethodClose && !success {
						c.replyError(call, -32099, "stop refused")
					} else {
						c.reply(call, map[string]any{"shimSessionId": "shim-1"})
					}
				}
			})
			sess, _ := claudeTestSession(t)
			store := newTestAgentExecutionStore(t, "internal-close")
			hosted, _, err := store.beginSession("internal-close", "hash", "claude", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			sess.hostedThreadID, sess.agentExecutions = hosted.ID, store
			if _, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.wsURL())); err != nil {
				t.Fatal(err)
			}
			if err := store.bindSession(hosted.ID, sess.id, sess.currentThreadID(), "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := store.beginTurn(hosted.ID, "pending", "hash")
			if err != nil {
				t.Fatal(err)
			}
			sess.close()
			record, _ := store.session(hosted.ID)
			turn, _ := store.turn(op.ID)
			if success {
				if record.Status != sessionStatusExited || turn.Status != agentTurnInterrupted {
					t.Fatalf("confirmed internal close failed to settle: %+v %+v", record, turn)
				}
			} else if record.Status != "ready" || turn.Status != op.Status {
				t.Fatalf("unconfirmed internal close settled: %+v %+v", record, turn)
			}
		})
	}
}
