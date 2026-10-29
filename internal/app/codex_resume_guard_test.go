package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sidebar"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// The fixture owns the provider process. Once it closes, wait for it instead
// of starting the installed Codex binary from a background test handshake.
func (f *fakeAppServer) daemon() *codexDaemon {
	d := newCodexDaemon(f.WSURL(), supervisedDaemonLabel)
	return d
}

func newResumeTestServer(t *testing.T, wsURL string) *server {
	t.Helper()
	metadata, executions := newTestHostedControl(t)
	s := &server{
		defaults:        config.SessionDefaults{CWD: t.TempDir()},
		codex:           newCodexDaemon(wsURL, supervisedDaemonLabel),
		spoolDir:        t.TempDir(),
		runtimeEvents:   sidebar.NewRuntimeHub(),
		Registry:        newSessionRegistry(),
		controlMetadata: metadata,
		agentExecutions: executions,
	}
	// Session handshakes run in background. Tear them down before the temporary
	// control.sqlite cleanup so a late provider reply cannot query a closed DB.
	t.Cleanup(func() {
		sessions := make([]*session, 0, len(s.SnapshotSessions()))
		for _, sess := range s.SnapshotSessions() {
			sessions = append(sessions, sess)
		}
		for _, sess := range sessions {
			sess.Close()
		}
	})
	return s
}

func postResume(t *testing.T, s *server, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	seedCanonicalResumeOwner(t, s, "codex", threadID, s.defaults.CWD)
	body := `{"provider":"codex","mode":"resume","threadId":"` + threadID + `"}`
	rec := httptest.NewRecorder()
	s.handleSessionCreate(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body)))
	return rec
}

// waitForSessionStatus polls the session summary until the background
// handshake lands on the wanted status (resume spawns are async per Slice R3).
func waitForSessionStatus(t *testing.T, s *server, sessionID, want string) sessionSummary {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess := s.LookupSession(sessionID)
		if sess == nil {
			t.Fatalf("session %s disappeared while waiting for status %q", sessionID, want)
		}
		if sum := sess.Summary(); sum.Status == want {
			return sum
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s never reached status %q", sessionID, want)
	return sessionSummary{}
}

func TestCodexDaemonReconnectsExistingSessionsWithoutAnotherRPC(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	rec := postResume(t, s, "thread-recover-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d %s", rec.Code, rec.Body.String())
	}
	var spawned sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &spawned); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	waitForSessionStatus(t, s, spawned.ID, "ready")
	if got := fake.ResumeCount(); got != 1 {
		t.Fatalf("initial resume count = %d, want 1", got)
	}

	// No model, MCP, spawn, or send request follows this drop. Recovery must be
	// initiated by the connection-loss path itself; otherwise /turn can never
	// help because reconnecting sessions fail its readiness gate.
	fake.DropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fake.ResumeCount() >= 2 && waitSessionStatus(s, spawned.ID) == "ready" {
			sess := s.LookupSession(spawned.ID)
			if sess != nil {
				sess.Close()
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon drop was not recovered autonomously: resumes=%d status=%s", fake.ResumeCount(), waitSessionStatus(s, spawned.ID))
}

func TestCodexDaemonReconnectOwnsResumeFailureFromDroppedSocket(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.SetReplyDelay("thread/resume", 2*time.Second)
	s := newResumeTestServer(t, fake.WSURL())
	rec := postResume(t, s, "thread-recover-inflight")
	var spawned sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &spawned); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	waitForWireMethod(t, s, spawned.ID, "thread/resume")

	fake.SetReplyDelay("thread/resume", 0)
	fake.DropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fake.ResumeCount() >= 2 && waitSessionStatus(s, spawned.ID) == "ready" {
			sess := s.LookupSession(spawned.ID)
			if sess != nil {
				sess.Close()
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("in-flight resume loss was not recovered: resumes=%d status=%s", fake.ResumeCount(), waitSessionStatus(s, spawned.ID))
}

func waitSessionStatus(s *server, sessionID string) string {
	sess := s.LookupSession(sessionID)
	if sess == nil {
		return "missing"
	}
	return sess.Summary().Status
}

func getSessionState(t *testing.T, s *server, sessionID string) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sessionID+"/state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("state status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// Codex resumes a thread at the tier in its own config unless the resume
// names one. Catches a hosted thread requested at the standard tier coming
// back from a restart or a dropped connection at the config's tier, and a
// failed resume (thread not found) recorded as Codex rejecting that tier.
func TestCodexResumeSendsTheThreadsServiceTier(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-00000000071e"
	hostedID := seedCanonicalResumeOwner(t, s, "codex", threadID, s.defaults.CWD)
	if _, err := s.controlMetadata.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldServiceTier, Value: "default",
		Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, SourceEventKey: "tier:" + hostedID}); err != nil {
		t.Fatal(err)
	}
	resume := func() sessionSummary {
		t.Helper()
		rec := postResume(t, s, threadID)
		var spawned sessionSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &spawned); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("resume = %d %s", rec.Code, rec.Body.String())
		}
		return spawned
	}

	fake.SetResumeErrorSequence("thread not found")
	waitForSessionStatus(t, s, resume().ID, "exited")
	view, err := s.controlMetadata.View(hostedID)
	if err != nil || view.Fields[postureFieldServiceTier].DesiredDisposition != metadataDispositionRequested {
		t.Fatalf("tier after a failed resume = %+v (%v), want still requested", view.Fields[postureFieldServiceTier], err)
	}

	waitForSessionStatus(t, s, resume().ID, "ready")
	fake.DropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for fake.ResumeCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	fake.Mu.Lock()
	tiers := append([]string(nil), fake.ResumeTiers...)
	fake.Mu.Unlock()
	if len(tiers) < 3 || tiers[0] != "default" || tiers[1] != "default" || tiers[2] != "default" {
		t.Fatalf("thread/resume serviceTier = %q, want default on every resume and on reconnect", tiers)
	}
}

// TestResumeHappyPathAndCloseDetaches covers the resume flow — the claim
// routes the thread to the session from the moment the POST returns, the
// background handshake lands on ready — and the release path: closing the
// session frees the thread for a fresh resume.
func TestResumeHappyPathAndCloseDetaches(t *testing.T) {
	fake := newFakeAppServer(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	resumeEntered := make(chan struct{}, 1)
	fake.OnResume = func(json.RawMessage) {
		select {
		case resumeEntered <- struct{}{}:
		default:
		}
		<-release
	}
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000001"

	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Provider != "codex" || sum.ThreadID != threadID || sum.TurnActive || sum.Error != "" {
		t.Fatalf("summary = %#v", sum)
	}
	// The pre-claim IS the routing registration: the thread routes to this
	// session immediately, whether or not the handshake has finished.
	routed := s.LookupSession(sum.ID)
	// The daemon is serving this connection once its resume request reaches
	// the fake. Keep that reply blocked while testing pre-handshake routing.
	<-resumeEntered
	fake.EmitNotification(map[string]any{"method": "routing/probe", "params": map[string]any{"threadId": threadID, "marker": "pre-handshake"}})
	waitForClaude(t, "pre-handshake routing notification", func() bool {
		for _, event := range claudeWireEvents(routed) {
			if strings.Contains(string(event.Msg), "pre-handshake") {
				return true
			}
		}
		return false
	})
	if routed.Summary().Status != "starting" {
		t.Fatal("routing probe did not precede the delayed handshake")
	}

	releaseOnce.Do(func() { close(release) })
	if sum = waitForSessionStatus(t, s, sum.ID, "ready"); sum.ThreadID != threadID || sum.Model != "gpt-test" {
		t.Fatalf("ready summary = %#v", sum)
	}

	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec = postResume(t, s, threadID); rec.Code != http.StatusOK {
		t.Fatalf("resume after close status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCodexResumeDurableTerminalTurnEndsIdleAfterHandshake(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.SetResumeTurns(json.RawMessage(`[{"id":"turn-durable-terminal","status":"inProgress","startedAt":1785510514}]`))
	// Hold the reply until the test replaces the default catalog-backed tail.
	// waitForWireMethod observes the request before this delayed response lands.
	fake.SetReplyDelay("thread/resume", time.Second)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "thread-durable-terminal"

	rec := postResume(t, s, threadID)
	spawned := decodeSummary(t, rec)
	waitForWireMethod(t, s, spawned.ID, "thread/resume")
	sess := s.LookupSession(spawned.ID)
	if sess == nil {
		t.Fatal("resumed session missing")
	}
	sess.ConfigureSavedReaders(func(_ context.Context, turnID string) (liveview.DurableTurnEvidence, bool) {
		return liveview.DurableTurnEvidence{Turn: liveview.Turn{ID: turnID, Status: "completed", CompletedAtMs: 1785510514001}}, turnID == "turn-durable-terminal"
	}, nil, nil, nil, nil)

	summary := waitForSessionStatus(t, s, spawned.ID, "ready")
	if summary.TurnActive {
		t.Fatalf("resume handshake resurrected durable terminal turn: %#v", summary)
	}
	fixture := &providerRuntimeFixture{session: sess, codex: fake}
	fixture.emitAndWait(t, []byte(`{"method":"turn/started","params":{"threadId":"thread-durable-terminal","turn":{"id":"turn-durable-terminal"}}}`))
	if summary := sess.Summary(); summary.TurnActive {
		t.Fatalf("durable terminal guard allowed the old turn to reopen: %+v", summary)
	}
}

// TestResumeConflictReturns409Shape byte-checks the common 409 contract
// and proves the losing resume produced no wire traffic (no second
// thread/resume reached the daemon).
func TestResumeConflictReturns409Shape(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000002"

	first := postResume(t, s, threadID)
	if first.Code != http.StatusOK {
		t.Fatalf("first resume status = %d, body = %s", first.Code, first.Body.String())
	}
	var winner sessionSummary
	if err := json.Unmarshal(first.Body.Bytes(), &winner); err != nil {
		t.Fatal(err)
	}

	second := postResume(t, s, threadID)
	if second.Code != http.StatusConflict {
		t.Fatalf("second resume status = %d, body = %s", second.Code, second.Body.String())
	}
	if ct := second.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := `{"error":{"code":"thread_attached","message":"Thread is already attached","details":{"existingSessionId":"` + winner.ID + `"}}}`
	if got := strings.TrimSpace(second.Body.String()); got != want {
		t.Fatalf("409 body = %q, want %q", got, want)
	}
	// Wait for the winner's background handshake so the count is settled.
	waitForSessionStatus(t, s, winner.ID, "ready")
	if n := fake.ResumeCount(); n != 1 {
		t.Fatalf("thread/resume reached the wire %d times, want 1 (guard must fire before wire traffic)", n)
	}
}

// TestConcurrentDoubleResumeOneWinner races two resumes of the same thread:
// exactly one spawns, the loser's 409 names the winner's session.
func TestConcurrentDoubleResumeOneWinner(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000003"

	recs := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = postResume(t, s, threadID)
		}(i)
	}
	wg.Wait()

	var winner sessionSummary
	var loserBody string
	wins, losses := 0, 0
	for _, rec := range recs {
		switch rec.Code {
		case http.StatusOK:
			wins++
			if err := json.Unmarshal(rec.Body.Bytes(), &winner); err != nil {
				t.Fatal(err)
			}
		case http.StatusConflict:
			losses++
			loserBody = strings.TrimSpace(rec.Body.String())
		default:
			t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins = %d losses = %d, want exactly one of each", wins, losses)
	}
	want := `{"error":{"code":"thread_attached","message":"Thread is already attached","details":{"existingSessionId":"` + winner.ID + `"}}}`
	if loserBody != want {
		t.Fatalf("loser body = %q, want %q", loserBody, want)
	}
	waitForSessionStatus(t, s, winner.ID, "ready")
	if n := fake.ResumeCount(); n != 1 {
		t.Fatalf("thread/resume reached the wire %d times, want 1", n)
	}
}

// TestResumeBackgroundFailureExitsAndReleasesClaim pins the frozen R3 failure
// contract: the POST itself succeeds (async spawn), the background handshake
// failure lands as status exited with the message surfaced as "error" on the
// summary and /state, the claim is released, and a second resume succeeds.
func TestResumeBackgroundFailureExitsAndReleasesClaim(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000004"

	fake.SetResumeError("no rollout found for thread")
	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s (failure must land on the session, not the POST)", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	sum = waitForSessionStatus(t, s, sum.ID, "exited")
	if !strings.Contains(sum.Error, "no rollout found for thread") {
		t.Fatalf("summary error = %q, want the handshake failure message", sum.Error)
	}
	var stateErr string
	if err := json.Unmarshal(getSessionState(t, s, sum.ID)["error"], &stateErr); err != nil {
		t.Fatalf("state has no error field: %v", err)
	}
	if !strings.Contains(stateErr, "no rollout found for thread") {
		t.Fatalf("state error = %q, want the handshake failure message", stateErr)
	}

	fake.SetResumeError("")
	if rec = postResume(t, s, threadID); rec.Code != http.StatusOK {
		t.Fatalf("retry after failure status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// A dead session has no routing claim, so accepting a turn would run work that
// nano cannot stream or control. Zero-turn boot recovery becomes ready before
// accepting its first turn; every other exited session must reject one.
func TestExitedCodexSessionRejectsTurn(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000014"

	fake.SetResumeError("no rollout found for thread")
	rec := postResume(t, s, threadID)
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, sum.ID, sessionStatusExited)

	turn := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(turn, httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+sum.ID+"/turn", strings.NewReader(`{"text":"must not run"}`)))
	if turn.Code != http.StatusConflict {
		t.Fatalf("turn status = %d, body = %s, want 409", turn.Code, turn.Body.String())
	}
}

// TestResumeReturnsStartingWhileHandshakeInFlight pins the frozen R3 async
// contract: with the thread/resume reply deliberately held for two seconds,
// the POST returns well under that with status "starting" and the requested
// threadId, the session is immediately subscribable (/state, wire events with
// the sent thread/resume), and the background handshake then lands on ready.
func TestResumeReturnsStartingWhileHandshakeInFlight(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000005"
	const handshakeDelay = 2 * time.Second

	fake.SetReplyDelay("thread/resume", handshakeDelay)
	begin := time.Now()
	rec := postResume(t, s, threadID)
	elapsed := time.Since(begin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if elapsed >= handshakeDelay/2 {
		t.Fatalf("resume POST took %v, want well under the %v handshake delay", elapsed, handshakeDelay)
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Status != "starting" || sum.ThreadID != threadID || sum.Error != "" {
		t.Fatalf("summary = %#v, want status starting with the requested threadId", sum)
	}

	state := getSessionState(t, s, sum.ID)
	var stateStatus, stateThreadID string
	json.Unmarshal(state["status"], &stateStatus)
	json.Unmarshal(state["threadId"], &stateThreadID)
	if stateStatus != "starting" || stateThreadID != threadID {
		t.Fatalf("state status = %q threadId = %q", stateStatus, stateThreadID)
	}
	if _, present := state["error"]; present {
		t.Fatal("in-flight resume carried an error field")
	}

	// The sent thread/resume is already in the wire buffer: the handshake's
	// traffic lands in the session's feed while the reply is still pending.
	waitForWireMethod(t, s, sum.ID, "thread/resume")

	if sum = waitForSessionStatus(t, s, sum.ID, "ready"); sum.Model != "gpt-test" || sum.Error != "" {
		t.Fatalf("ready summary = %#v", sum)
	}
}

// waitForWireMethod polls the session's events page until a sent frame with
// the wanted method shows up.
func waitForWireMethod(t *testing.T, s *server, sessionID, method string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sessionID+"/events/page?beforeSeq=999999", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("events page status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var page struct {
			Events []struct {
				Dir string          `json:"dir"`
				Msg json.RawMessage `json:"msg"`
			} `json:"events"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, evt := range page.Events {
			var msg struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(evt.Msg, &msg) == nil && evt.Dir == "send" && msg.Method == method {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no sent %s frame reached session %s's wire buffer", method, sessionID)
}

// TestResumeStateSnapshotStripsTurns proves the stored thread snapshot has
// thread.turns replaced with [] while every sibling field survives intact.
func TestResumeStateSnapshotStripsTurns(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000006"

	fake.SetResumeTurns(json.RawMessage(`[{"id":"turn_1","items":[{"type":"agentMessage","text":"hello"}]}]`))
	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, sum.ID, "ready")

	var snapshot struct {
		Model  string `json:"model"`
		Thread struct {
			ID    string            `json:"id"`
			Cwd   string            `json:"cwd"`
			Turns []json.RawMessage `json:"turns"`
		} `json:"thread"`
	}
	rawThread := getSessionState(t, s, sum.ID)["thread"]
	if err := json.Unmarshal(rawThread, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Thread.Turns == nil || len(snapshot.Thread.Turns) != 0 {
		t.Fatalf("thread.turns = %s, want [] (stripped, not absent)", rawThread)
	}
	if snapshot.Model != "gpt-test" || snapshot.Thread.ID != threadID || snapshot.Thread.Cwd != "/tmp" {
		t.Fatalf("snapshot lost sibling fields: %s", rawThread)
	}
}

// TestResumeWireReplyOmitsHistoricalTurns proves the large history returned by
// app-server never reaches Nano's stored event pipeline. The handshake still
// receives the original reply and preserves its compact state snapshot.
func TestResumeWireReplyOmitsHistoricalTurns(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000008"
	const historicalText = "resume-history-must-not-reach-the-event-feed"
	fake.SetResumeTurns(json.RawMessage(`[{"id":"turn_1","items":[{"type":"agentMessage","text":"` + historicalText + strings.Repeat("x", 128*1024) + `"}]}]`))

	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, sum.ID, "ready")
	sess := s.LookupSession(sum.ID)
	if sess == nil {
		t.Fatal("resumed session missing")
	}
	var events []wireEvent
	waitForClaude(t, "compact resume reply persisted", func() bool {
		events = claudeWireEvents(sess)
		for _, event := range events {
			if strings.Contains(string(event.Msg), `"nanoOmittedFields"`) {
				return true
			}
		}
		return false
	})

	for _, evt := range events {
		if evt.Dir != string(protocol.Recv) || !strings.Contains(string(evt.Msg), `"nanoOmittedFields"`) {
			continue
		}
		if strings.Contains(string(evt.Msg), historicalText) {
			t.Fatalf("resume event retained historical turns (%d bytes)", len(evt.Msg))
		}
		if len(evt.Msg) > 4096 {
			t.Fatalf("resume event is %d bytes, want compact reply", len(evt.Msg))
		}
		var reply struct {
			Result struct {
				Thread struct {
					ID    string            `json:"id"`
					Turns []json.RawMessage `json:"turns"`
				} `json:"thread"`
				Omitted struct {
					Fields []string `json:"fields"`
					Reason string   `json:"reason"`
				} `json:"nanoOmittedFields"`
			} `json:"result"`
		}
		if err := json.Unmarshal(evt.Msg, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Result.Thread.ID != threadID || reply.Result.Thread.Turns == nil || len(reply.Result.Thread.Turns) != 0 {
			t.Fatalf("compact thread = %#v, want id with turns []", reply.Result.Thread)
		}
		if len(reply.Result.Omitted.Fields) != 1 || reply.Result.Omitted.Fields[0] != "thread.turns" || reply.Result.Omitted.Reason == "" {
			t.Fatalf("omission marker = %#v", reply.Result.Omitted)
		}
		return
	}
	t.Fatal("no compact thread/resume reply found in the session wire events")
}

// postStart spawns a fresh codex thread through the real HTTP handler.
func postStart(t *testing.T, s *server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleSessionCreate(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(`{"provider":"codex","mode":"start"}`)))
	return rec
}

func decodeSummary(t *testing.T, rec *httptest.ResponseRecorder) sessionSummary {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	return sum
}

// TestStartReturnsStartingWhileHandshakeInFlight is the fresh-start half of the
// async spawn contract: with thread/start held for two seconds, the POST comes
// back well under that — status "starting", no thread id yet, spawnMode naming
// it a creation rather than an attach — and the session is listed and
// subscribable at once. The background handshake then lands it on ready.
//
// The bug: a synchronous thread/start left the New Window dialog on
// "Creating…" for up to codexHandshakeTimeout (60s), which reads as a hang.
func TestStartReturnsStartingWhileHandshakeInFlight(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const handshakeDelay = 2 * time.Second

	fake.SetReplyDelay("thread/start", handshakeDelay)
	begin := time.Now()
	sum := decodeSummary(t, postStart(t, s))
	elapsed := time.Since(begin)
	if elapsed >= handshakeDelay/2 {
		t.Fatalf("start POST took %v, want well under the %v handshake delay", elapsed, handshakeDelay)
	}
	if sum.Status != "starting" || sum.ThreadID != "" || sum.Error != "" {
		t.Fatalf("summary = %#v, want status starting with no thread id yet", sum)
	}
	if sum.SpawnMode != "start" {
		t.Fatalf("spawnMode = %q, want start (the UI says creating vs attaching from this)", sum.SpawnMode)
	}
	// Registered before the handshake: the card is on screen, and /state
	// answers, while thread/start is still in flight.
	state := getSessionState(t, s, sum.ID)
	var stateStatus string
	json.Unmarshal(state["status"], &stateStatus)
	if stateStatus != "starting" {
		t.Fatalf("state status = %q, want starting", stateStatus)
	}

	if ready := waitForSessionStatus(t, s, sum.ID, "ready"); ready.ThreadID != "thread-fresh-1" || ready.Error != "" {
		t.Fatalf("ready summary = %#v, want the started thread id", ready)
	}
}

// TestStartBackgroundFailureExitsWithError: a fresh start whose handshake fails
// keeps the R3 failure contract the resume path has — the POST succeeded, so
// the failure has to land on the session card as exited + error.
func TestStartBackgroundFailureExitsWithError(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())

	fake.SetStartError("model not available")
	// Hold the provider's answer until the POST has answered: otherwise a
	// loaded machine lets the failure land first and the summary reads exited.
	answer := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(answer) })
	fake.SetOnStart(func(json.RawMessage) { <-answer })
	sum := decodeSummary(t, postStart(t, s))
	if sum.Status != "starting" {
		t.Fatalf("summary = %#v, want the failure to land on the session, not the POST", sum)
	}
	release.Do(func() { close(answer) })
	sum = waitForSessionStatus(t, s, sum.ID, sessionStatusExited)
	if !strings.Contains(sum.Error, "model not available") {
		t.Fatalf("summary error = %q, want the handshake failure message", sum.Error)
	}
	var stateErr string
	if err := json.Unmarshal(getSessionState(t, s, sum.ID)["error"], &stateErr); err != nil {
		t.Fatalf("state has no error field: %v", err)
	}
	if !strings.Contains(stateErr, "model not available") {
		t.Fatalf("state error = %q", stateErr)
	}
	fake.SetStartError("")
	retry := decodeSummary(t, postStart(t, s))
	waitForSessionStatus(t, s, retry.ID, "ready")
}

// TestCloseDuringStartHandshakeLeavesNoZombieRouting closes a fresh start while
// thread/start is still in flight. The late reply must not resurrect the
// session or route the thread it minted to a torn-down one — and because
// close() ran before there was any id to unsubscribe, the handshake owes the
// daemon that unsubscribe itself.
func TestCloseDuringStartHandshakeLeavesNoZombieRouting(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const handshakeDelay = 250 * time.Millisecond

	fake.SetReplyDelay("thread/start", handshakeDelay)
	sum := decodeSummary(t, postStart(t, s))

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}

	time.Sleep(handshakeDelay + 300*time.Millisecond) // let the delayed handshake land
	listed := s.LookupSession(sum.ID) != nil
	if listed {
		t.Fatal("closed session came back into the session list")
	}
	if n := fake.Unsubscribed("thread-fresh-1"); n != 1 {
		t.Fatalf("thread/unsubscribe for the abandoned thread ran %d times, want 1", n)
	}
	fake.SetReplyDelay("thread/start", 0)
	retry := decodeSummary(t, postStart(t, s))
	waitForSessionStatus(t, s, retry.ID, "ready")
}

// TestCloseDuringResumeHandshakeLeavesNoZombieRouting closes the session while
// the background handshake is still in flight: once the handshake lands, the
// thread must not be routed to the torn-down session and stays resumable.
func TestCloseDuringResumeHandshakeLeavesNoZombieRouting(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.WSURL())
	const threadID = "0199a213-abcd-7000-8000-000000000007"
	const handshakeDelay = 250 * time.Millisecond

	fake.SetReplyDelay("thread/resume", handshakeDelay)
	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}

	time.Sleep(handshakeDelay + 200*time.Millisecond) // let the delayed handshake land
	fake.SetReplyDelay("thread/resume", 0)
	rec = postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume after mid-handshake close status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var retry sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &retry); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, retry.ID, "ready")
}
