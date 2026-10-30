package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
)

func TestClaudeForgetUnconfirmedIsExplicitAndGenerationScoped(t *testing.T) {
	for _, scenario := range []string{"forget", "missing_identity", "replacement", "resumed", "bad_provider", "missing_capture"} {
		t.Run(scenario, func(t *testing.T) {
			store := newTestStore(t)
			record := hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-1", ShimSessionID: "old-process", AttachedAt: "generation-1"}
			if scenario == "missing_identity" {
				record.ShimSessionID = ""
			}
			store.Remember(record)
			s := &server{Registry: newSessionRegistry(), attachments: store}
			query := url.Values{"provider": {"claude"}, "forget": {"unconfirmed"}, "expectedShimSessionId": {record.ShimSessionID}, "expectedAttachedAt": {record.AttachedAt}}
			switch scenario {
			case "replacement":
				record.ShimSessionID = "new-process"
				store.Remember(record)
			case "resumed":
				s.RegisterSession(configuredFixtureSession(t, sessions.Config{ID: "live", Provider: "claude"}, "native-1", "ready", "", false))
			case "bad_provider":
				query.Set("provider", "codex")
			case "missing_capture":
				query.Del("expectedShimSessionId")
			}
			w := httptest.NewRecorder()
			applicationHandler(fixtureOwners(s)).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/attachments/native-1?"+query.Encode(), nil))
			_, retained := store.Get("claude", "native-1")
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
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				call, ok := c.Next()
				if !ok {
					return
				}
				if outcome == "not_found" {
					c.ReplyError(call, shimCodeSessionNotFound, "session_not_found")
				} else if outcome != "disconnect" {
					c.Reply(call, map[string]any{})
				}
			})
			store := newTestStore(t)
			attachment := hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-1", ShimSessionID: "exact-process"}
			if outcome == "missing_identity" {
				attachment.ShimSessionID = ""
			}
			store.Remember(attachment)
			s := &server{Registry: newSessionRegistry(), attachments: store}
			s.defaults.ClaudeShimWS = shim.WSURL()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodDelete, "/api/attachments/native-1?provider=claude", nil)
			applicationHandler(fixtureOwners(s)).ServeHTTP(w, r)
			_, retained := store.Get("claude", "native-1")
			if outcome == "success" {
				if w.Code != 200 || retained {
					t.Fatalf("close = %d %s retained=%v", w.Code, w.Body, retained)
				}
			} else if w.Code < 400 || !retained {
				t.Fatalf("uncertain close = %d %s retained=%v", w.Code, w.Body, retained)
			}
			calls := shim.CallsOf(shimMethodClose)
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
				if len(shim.CallsOf(method)) != 0 {
					t.Fatalf("close sent %s", method)
				}
			}
		})
	}
}

func TestClaudeExplicitClosePreservesReplacement(t *testing.T) {
	store := newTestStore(t)
	old := hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-1", ShimSessionID: "old-process"}
	store.Remember(old)
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		call, ok := c.Next()
		if !ok {
			return
		}
		replacement := old
		replacement.ShimSessionID = "replacement"
		store.Remember(replacement)
		c.Reply(call, map[string]any{})
	})
	s := &server{attachments: store}
	s.defaults.ClaudeShimWS = shim.WSURL()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/attachments/native-1", nil)
	request.SetPathValue("nativeThreadId", "native-1")
	fixtureOwners(s).hostedController().HandleAttachmentScoped(response, request)
	if response.Code < 400 {
		t.Fatal("replacement was not detected")
	}
	current, ok := store.Get("claude", "native-1")
	if !ok || current.ShimSessionID != "replacement" {
		t.Fatalf("replacement lost: %+v", current)
	}
}

func TestClaudeExplicitHostedFailurePreservesOperations(t *testing.T) {
	for _, endpoint := range []string{"close", "delete", "family", "attachment"} {
		t.Run(endpoint, func(t *testing.T) {
			s, store, _ := newBulkCloseTestServer(t)
			hosted, _, err := fixtureBeginSession(store, "close-test", "hash", "claude", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.BindSession(hosted.ID, "old-nano", "native-1", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := fixtureBeginTurn(store, hosted.ID, "queued-action", "hash")
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.Session(hosted.ID)
			s.queuedTurns = mustQueuedTurnStore(t)
			fixtureTurns, openErr := prompts.OpenStore(context.Background(), s.queuedTurns.Database())
			err = openErr
			s.queuedTurns.BindTransactionStore(fixtureTurns)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.queuedTurns.TransactionStore().Submit(context.Background(), prompts.Submission{
				ActionID: "still-queued", Provider: "claude", HostedThreadID: hosted.ID,
				NativeThreadID: "native-1", ProviderText: "not sent", TextHash: "hash", RequestHash: "hash",
			}); err != nil {
				t.Fatal(err)
			}
			s.attachments = newTestStore(t)
			s.attachments.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: "native-1", HostedThreadID: hosted.ID, ShimSessionID: "exact-process"})
			for _, native := range []string{"native-1", "other-thread"} {
				if err := fixtureSetStarred(t, s.threadStars, "claude", native, true); err != nil {
					t.Fatal(err)
				}
			}
			var reject atomic.Bool
			reject.Store(true)
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				call, ok := c.Next()
				if !ok {
					return
				}
				if reject.Load() {
					c.ReplyError(call, -32099, "stop refused")
				} else {
					c.Reply(call, map[string]any{})
				}
			})
			s.defaults.ClaudeShimWS = shim.WSURL()
			request := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				method, path, body := http.MethodPost, "/api/v1/hosted-threads/"+hosted.ID+"/close", ""
				switch endpoint {
				case "delete":
					method, path = http.MethodDelete, "/api/v1/hosted-threads/"+hosted.ID
				case "family":
					path = "/api/v1/hosted-threads/" + hosted.ID + "/family/close"
				case "attachment":
					method, path = http.MethodDelete, "/api/attachments/native-1?provider=claude"
				}
				applicationHandler(fixtureOwners(s)).ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
				return w
			}
			w := request()
			if w.Code < 400 && w.Code != 207 {
				t.Fatalf("failed stop reported success: %d %s", w.Code, w.Body)
			}
			after, ok := store.Session(hosted.ID)
			if !ok || after.Status != before.Status {
				t.Fatalf("failed stop settled session: %+v", after)
			}
			turn, _ := store.Turn(op.ID)
			if turn.Status != op.Status {
				t.Fatalf("failed stop settled operation: %+v", turn)
			}
			if _, ok := s.attachments.Get("claude", "native-1"); !ok {
				t.Fatal("failed stop erased attachment")
			}
			if _, ok := s.queuedTurns.First("claude", "native-1", hosted.ID); !ok {
				t.Fatal("failed stop removed queued prompt")
			}
			// Bulk close removes a whole tree's stars before it closes anyone.
			if endpoint != "family" && !s.threadStars.IsStarred("claude", "native-1") {
				t.Fatal("failed stop removed the star of a thread that is still open")
			}
			reject.Store(false)
			w = request()
			if w.Code != 200 {
				t.Fatalf("retry failed: %d %s", w.Code, w.Body)
			}
			if _, ok := s.attachments.Get("claude", "native-1"); ok {
				t.Fatal("successful retry retained attachment")
			}
			if _, ok := s.queuedTurns.First("claude", "native-1", hosted.ID); ok {
				t.Fatal("successful retry left queued prompt")
			}
			if s.threadStars.IsStarred("claude", "native-1") {
				t.Fatal("closing the thread left its star, so it stays a gray sidebar row")
			}
			if !s.threadStars.IsStarred("claude", "other-thread") {
				t.Fatal("closing one thread removed another thread's star")
			}
		})
	}
}

// A Claude detach shares the close path but the thread is not closed: it can
// resume, so it keeps its place in the sidebar.
func TestClaudeDetachKeepsTheStar(t *testing.T) {
	s, store, _ := newBulkCloseTestServer(t)
	hosted, _, err := fixtureBeginSession(store, "detach-test", "hash", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(hosted.ID, "old-nano", "native-1", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := fixtureSetStarred(t, s.threadStars, "claude", "native-1", true); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+hosted.ID+"/detach", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detach = %d %s", w.Code, w.Body)
	}
	if !s.threadStars.IsStarred("claude", "native-1") {
		t.Fatal("detach removed the star")
	}
}

func TestClaudeExplicitConcurrentClosesSendOneStop(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		call, ok := c.Next()
		if !ok {
			return
		}
		close(started)
		<-finish
		c.Reply(call, map[string]any{})
	})
	s := &server{Registry: newSessionRegistry(), attachments: newTestStore(t)}
	s.defaults.ClaudeShimWS = shim.WSURL()
	s.attachments.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: "cs_provisional", ShimSessionID: "cs_provisional"})
	results := make(chan int, 2)
	request := func() {
		w := httptest.NewRecorder()
		applicationHandler(fixtureOwners(s)).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/attachments/cs_provisional?provider=claude", nil))
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
	if calls := shim.CallsOf(shimMethodClose); len(calls) != 1 {
		t.Fatalf("duplicate stops: %v", calls)
	}
}

func TestClaudeExplicitStartingCloseDoesNotClaimSuccess(t *testing.T) {
	sess, _ := claudeTestSession(t)
	s := testServerWithSessions(&server{Registry: newSessionRegistry()}, map[string]*sessions.Session{sess.ID(): sess})
	w := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleLegacyClose(w, httptest.NewRequest(http.MethodDelete, "/", nil), sess)
	if w.Code != 502 || s.LookupSession(sess.ID()) != sess || sess.View().Summary().Status == sessionStatusExited {
		t.Fatalf("starting close lost in-flight attachment: %d %s", w.Code, w.Body)
	}
}

func TestClaudeExplicitLiveFailureKeepsRetryIdentity(t *testing.T) {
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			if call.Method == shimMethodClose {
				c.ReplyError(call, -32099, "stop refused")
			} else {
				c.Reply(call, map[string]any{"shimSessionId": "shim-1"})
			}
		}
	})
	store := newTestAgentExecutionStore(t, "close-test")
	hosted, _, _ := fixtureBeginSession(store, "live-close", "hash", "claude", presentationShared)
	sess := newCloseFixtureSession(t, store, hosted.ID)
	if err := sess.Runtime().BeginProviderSpawn(claudeShimSpawnRequest(sess, shim.WSURL()).ProviderRequest, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
	_ = store.BindSession(hosted.ID, sess.ID(), sess.State().ThreadID(), "ready", presentationShared)
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "pending", "hash")
	attachments := newTestStore(t)
	attachments.Remember(hostedstore.Attachment{Provider: "claude", NativeThreadID: sess.State().ThreadID(), HostedThreadID: hosted.ID, ShimSessionID: "shim-1"})
	s := testServerWithSessions(&server{Registry: newSessionRegistry(), attachments: attachments, agentExecutions: store}, map[string]*sessions.Session{sess.ID(): sess})
	w := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleLegacyClose(w, httptest.NewRequest(http.MethodDelete, "/", nil), sess)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("close = %d %s", w.Code, w.Body)
	}
	if _, ok := attachments.Get("claude", "shim-1"); !ok {
		t.Fatal("retry identity lost")
	}
	if s.LookupSession(sess.ID()) != nil {
		t.Fatal("failed local connection retained as live")
	}
	turn, _ := store.Turn(op.ID)
	if turn.Status != op.Status {
		t.Fatalf("failed close settled turn: %+v", turn)
	}
	record, _ := store.Session(hosted.ID)
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
			shim := newFakeShim(t, func(c *claudeshim.Conn) {
				for {
					call, ok := c.Next()
					if !ok {
						return
					}
					if call.Method == shimMethodClose && !success {
						c.ReplyError(call, -32099, "stop refused")
					} else {
						c.Reply(call, map[string]any{"shimSessionId": "shim-1"})
					}
				}
			})
			store := newTestAgentExecutionStore(t, "internal-close")
			hosted, _, err := fixtureBeginSession(store, "internal-close", "hash", "claude", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			sess := newCloseFixtureSession(t, store, hosted.ID)
			if err := sess.Runtime().BeginProviderSpawn(claudeShimSpawnRequest(sess, shim.WSURL()).ProviderRequest, nil, func() {}); err != nil {
				t.Fatal(err)
			}
			waitForClaudeStatus(t, sess, "ready")
			if err := store.BindSession(hosted.ID, sess.ID(), sess.State().ThreadID(), "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := fixtureBeginTurn(store, hosted.ID, "pending", "hash")
			if err != nil {
				t.Fatal(err)
			}
			sess.Teardown().Close()
			record, _ := store.Session(hosted.ID)
			turn, _ := store.Turn(op.ID)
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

// newCloseFixtureSession shares the real hosted store with the closing attachment.
func newCloseFixtureSession(t *testing.T, store *hostedstore.ExecutionStore, hosted string) *sessions.Session {
	t.Helper()
	cfg, _, _ := claudeTestSessionConfig(t)
	cfg.HostedThreadID, cfg.AgentExecutions, cfg.Metadata = hosted, store, store.Control()
	generation, err := store.Control().BeginTransportBinding(hosted)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(cfg)
	if err := sess.InitializeBinding(generation); err != nil {
		t.Fatal(err)
	}
	return sess
}
