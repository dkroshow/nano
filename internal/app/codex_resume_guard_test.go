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

	protocol "github.com/dkroshow/nano/internal/wire/codex"

	"github.com/gorilla/websocket"
)

// fakeAppServer is a minimal codex app-server daemon: a websocket endpoint
// answering initialize/thread calls so daemon-backed spawn paths run in tests
// without a codex binary. Responses use the real wire framing (one JSON-RPC
// message per text frame, no "jsonrpc" field). A per-method reply delay lets
// tests hold a thread handshake in flight without stalling other traffic.
type fakeAppServer struct {
	srv *httptest.Server

	mu              sync.Mutex
	resumeErr       string // non-empty: thread/resume answers this RPC error message
	resumeErrors    []string
	startErr        string // non-empty: thread/start answers this RPC error message
	turnStartErr    string // non-empty: turn/start answers this RPC error message
	turnStartResult map[string]any
	dropStart       bool // thread/start closes transport without an RPC answer
	startParams     json.RawMessage
	startResult     map[string]any
	resumeResult    map[string]any
	modelList       map[string]any // model/list result; nil answers an empty result
	onStart         func(json.RawMessage)
	replyDelays     map[string]time.Duration
	resumeDelays    map[string]time.Duration
	resumeCounts    map[string]int
	resumeTiers     []string // each thread/resume's serviceTier param, in arrival order
	turnStarts      int
	turnStartTexts  []string          // each turn/start's prompt text, in arrival order
	steers          []json.RawMessage // turn/steer params, in arrival order
	steerErr        string            // non-empty: turn/steer answers this RPC error message
	dropSteer       bool              // turn/steer closes the transport without an RPC answer
	resumeTurns     json.RawMessage   // non-nil: thread/resume responses carry these thread.turns
	resumes         int
	unsubscribes    map[string]int // thread/unsubscribe requests, by threadId
	mcpCalls        int            // mcpServerStatus/list requests received
	mcpDetails      []string       // the `detail` param of each of those requests
	skillsCalls     int            // skills/list requests received
	skillsCwds      []string       // the sole cwd carried by each request
	skillsInFlight  int
	skillsMaxFlight int
	connections     map[*websocket.Conn]*sync.Mutex
	subscriptions   map[*websocket.Conn]map[string]bool
}

func newFakeAppServer(t *testing.T) *fakeAppServer {
	t.Helper()
	f := &fakeAppServer{
		replyDelays: map[string]time.Duration{}, unsubscribes: map[string]int{},
		resumeDelays: map[string]time.Duration{}, resumeCounts: map[string]int{},
		connections:   map[*websocket.Conn]*sync.Mutex{},
		subscriptions: map[*websocket.Conn]map[string]bool{},
	}
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		var writeMu sync.Mutex // delayed replies and test notifications share one writer
		f.connections[conn] = &writeMu
		f.subscriptions[conn] = map[string]bool{}
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.connections, conn)
			delete(f.subscriptions, conn)
			f.mu.Unlock()
			conn.Close()
		}()
		writeFrame := func(frame []byte) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteMessage(websocket.TextMessage, frame)
		}
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(frame, &msg) != nil || msg.ID == nil {
				continue // notifications (initialized) need no reply
			}
			isSkills := msg.Method == "skills/list"
			if isSkills {
				var request struct {
					Cwds []string `json:"cwds"`
				}
				_ = json.Unmarshal(msg.Params, &request)
				cwd := ""
				if len(request.Cwds) == 1 {
					cwd = request.Cwds[0]
				}
				f.mu.Lock()
				f.skillsCalls++
				f.skillsCwds = append(f.skillsCwds, cwd)
				f.skillsInFlight++
				if f.skillsInFlight > f.skillsMaxFlight {
					f.skillsMaxFlight = f.skillsInFlight
				}
				f.mu.Unlock()
			}
			reply := f.respond(conn, msg.ID, msg.Method, msg.Params)
			f.mu.Lock()
			delay := f.replyDelays[msg.Method]
			if msg.Method == "thread/resume" {
				var params struct {
					ThreadID string `json:"threadId"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				if configured, ok := f.resumeDelays[params.ThreadID]; ok {
					delay = configured
				}
			}
			drop := msg.Method == "thread/start" && f.dropStart || msg.Method == "turn/steer" && f.dropSteer
			f.mu.Unlock()
			if drop {
				return
			}
			if delay > 0 {
				go func() {
					time.Sleep(delay)
					_ = writeFrame(reply)
					if isSkills {
						f.mu.Lock()
						f.skillsInFlight--
						f.mu.Unlock()
					}
				}()
				continue
			}
			if err := writeFrame(reply); err != nil {
				return
			}
			if isSkills {
				f.mu.Lock()
				f.skillsInFlight--
				f.mu.Unlock()
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAppServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeAppServer) setResumeError(message string) {
	f.mu.Lock()
	f.resumeErr = message
	f.mu.Unlock()
}

func (f *fakeAppServer) setResumeErrorSequence(messages ...string) {
	f.mu.Lock()
	f.resumeErrors = append([]string(nil), messages...)
	f.mu.Unlock()
}

func (f *fakeAppServer) setStartError(message string) {
	f.mu.Lock()
	f.startErr = message
	f.mu.Unlock()
}

func (f *fakeAppServer) setTurnStartError(message string) {
	f.mu.Lock()
	f.turnStartErr = message
	f.mu.Unlock()
}

func (f *fakeAppServer) setTurnStartResult(result map[string]any) {
	f.mu.Lock()
	f.turnStartResult = result
	f.mu.Unlock()
}

func (f *fakeAppServer) startedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.turnStartTexts...)
}

func (f *fakeAppServer) setSteerError(message string) {
	f.mu.Lock()
	f.steerErr = message
	f.mu.Unlock()
}

func (f *fakeAppServer) setDropSteer(drop bool) {
	f.mu.Lock()
	f.dropSteer = drop
	f.mu.Unlock()
}

func (f *fakeAppServer) steerParams() []json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]json.RawMessage(nil), f.steers...)
}

func (f *fakeAppServer) setDropStart(drop bool) {
	f.mu.Lock()
	f.dropStart = drop
	f.mu.Unlock()
}

func (f *fakeAppServer) setStartResult(result map[string]any) {
	f.mu.Lock()
	f.startResult = result
	f.mu.Unlock()
}

func (f *fakeAppServer) setResumeResult(result map[string]any) {
	f.mu.Lock()
	f.resumeResult = result
	f.mu.Unlock()
}

func (f *fakeAppServer) setOnStart(fn func(json.RawMessage)) {
	f.mu.Lock()
	f.onStart = fn
	f.mu.Unlock()
}

// unsubscribed counts the thread/unsubscribe calls received for threadID.
func (f *fakeAppServer) unsubscribed(threadID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsubscribes[threadID]
}

func (f *fakeAppServer) setReplyDelay(method string, delay time.Duration) {
	f.mu.Lock()
	f.replyDelays[method] = delay
	f.mu.Unlock()
}

func (f *fakeAppServer) setResumeTurns(turns json.RawMessage) {
	f.mu.Lock()
	f.resumeTurns = turns
	f.mu.Unlock()
}

func (f *fakeAppServer) resumeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumes
}

func (f *fakeAppServer) dropConnections() {
	f.mu.Lock()
	connections := make([]*websocket.Conn, 0, len(f.connections))
	for conn := range f.connections {
		connections = append(connections, conn)
	}
	f.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (f *fakeAppServer) emitThreadNotification(threadID string, payload map[string]any) {
	f.mu.Lock()
	type target struct {
		conn *websocket.Conn
		mu   *sync.Mutex
	}
	targets := []target{}
	for conn, writeMu := range f.connections {
		if f.subscriptions[conn][threadID] {
			targets = append(targets, target{conn: conn, mu: writeMu})
		}
	}
	f.mu.Unlock()
	frame, _ := json.Marshal(payload)
	for _, candidate := range targets {
		candidate.mu.Lock()
		_ = candidate.conn.WriteMessage(websocket.TextMessage, frame)
		candidate.mu.Unlock()
	}
}

func (f *fakeAppServer) mcpListCalls() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mcpCalls, append([]string(nil), f.mcpDetails...)
}

func (f *fakeAppServer) skillsListCalls() (calls, maxInFlight int, cwds []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.skillsCalls, f.skillsMaxFlight, append([]string(nil), f.skillsCwds...)
}

func (f *fakeAppServer) respond(conn *websocket.Conn, id json.RawMessage, method string, params json.RawMessage) []byte {
	result := map[string]any{}
	switch method {
	case "model/list":
		f.mu.Lock()
		if f.modelList != nil {
			result = f.modelList
		}
		f.mu.Unlock()
	case "thread/read":
		var req struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &req)
		result = map[string]any{"thread": map[string]any{"id": req.ThreadID, "cwd": "/tmp"}}
	case "thread/resume":
		f.mu.Lock()
		f.resumes++
		var target struct {
			ThreadID    string `json:"threadId"`
			ServiceTier string `json:"serviceTier"`
		}
		_ = json.Unmarshal(params, &target)
		f.resumeCounts[target.ThreadID]++
		f.resumeTiers = append(f.resumeTiers, target.ServiceTier)
		resumeErr := f.resumeErr
		if len(f.resumeErrors) > 0 {
			resumeErr = f.resumeErrors[0]
			f.resumeErrors = f.resumeErrors[1:]
		}
		resumeTurns := f.resumeTurns
		resumeResult := f.resumeResult
		f.mu.Unlock()
		if resumeErr != "" {
			out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": resumeErr}})
			return out
		}
		if resumeResult != nil {
			result = resumeResult
			break
		}
		var req struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &req)
		f.mu.Lock()
		if f.subscriptions[conn] != nil {
			f.subscriptions[conn][req.ThreadID] = true
		}
		f.mu.Unlock()
		thread := map[string]any{"id": req.ThreadID, "cwd": "/tmp"}
		if resumeTurns != nil {
			thread["turns"] = resumeTurns
		}
		result = map[string]any{"model": "gpt-test", "thread": thread}
	case "thread/start":
		f.mu.Lock()
		f.startParams = append(f.startParams[:0], params...)
		startErr := f.startErr
		startResult := f.startResult
		onStart := f.onStart
		f.mu.Unlock()
		if onStart != nil {
			onStart(append(json.RawMessage(nil), params...))
		}
		if startErr != "" {
			out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": startErr}})
			return out
		}
		if startResult != nil {
			result = startResult
		} else {
			result = map[string]any{"model": "gpt-test", "thread": map[string]any{"id": "thread-fresh-1", "cwd": "/tmp"}}
		}
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		encoded, _ := json.Marshal(result)
		_ = json.Unmarshal(encoded, &started)
		if started.Thread.ID != "" {
			f.mu.Lock()
			if f.subscriptions[conn] != nil {
				f.subscriptions[conn][started.Thread.ID] = true
			}
			f.mu.Unlock()
		}
	case "thread/unsubscribe":
		var req struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &req)
		f.mu.Lock()
		f.unsubscribes[req.ThreadID]++
		f.mu.Unlock()
	case "turn/start":
		var start struct {
			Input []struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.Unmarshal(params, &start)
		f.mu.Lock()
		f.turnStarts++
		if len(start.Input) > 0 {
			f.turnStartTexts = append(f.turnStartTexts, start.Input[0].Text)
		}
		turnStartErr := f.turnStartErr
		turnStartResult := f.turnStartResult
		f.mu.Unlock()
		if turnStartErr != "" {
			out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": turnStartErr}})
			return out
		}
		if turnStartResult != nil {
			result = turnStartResult
		}
	case "turn/steer":
		// Like Codex, it joins the expected turn or refuses.
		var req struct {
			ExpectedTurnID string `json:"expectedTurnId"`
		}
		_ = json.Unmarshal(params, &req)
		f.mu.Lock()
		f.steers = append(f.steers, append(json.RawMessage(nil), params...))
		steerErr := f.steerErr
		f.mu.Unlock()
		if steerErr != "" {
			out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": steerErr}})
			return out
		}
		result = map[string]any{"turnId": req.ExpectedTurnID}
	case "skills/list":
		result = map[string]any{"data": []any{map[string]any{"name": "some-skill"}}}
	case "mcpServerStatus/list":
		var req struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(params, &req)
		f.mu.Lock()
		f.mcpCalls++
		f.mcpDetails = append(f.mcpDetails, req.Detail)
		f.mu.Unlock()
		// Tool schemas are what makes the real response huge; the projection
		// keeps only the map keys, so give each tool a non-trivial body.
		result = map[string]any{"data": []any{
			map[string]any{"name": "docs", "tools": map[string]any{
				"list_endpoints": map[string]any{"inputSchema": map[string]any{"type": "object"}},
				"fetch_doc":      map[string]any{"inputSchema": map[string]any{"type": "object"}},
			}},
			map[string]any{"name": "empty", "tools": map[string]any{}},
		}}
	}
	out, _ := json.Marshal(map[string]any{"id": id, "result": result})
	return out
}

// The fixture owns the provider process. Once it closes, wait for it instead
// of starting the installed Codex binary from a background test handshake.
func (f *fakeAppServer) daemon() *codexDaemon {
	d := newCodexDaemon(f.wsURL(), supervisedDaemonLabel)
	d.supervisedJobLoaded = func() bool { return true }
	return d
}

func newResumeTestServer(t *testing.T, wsURL string) *server {
	t.Helper()
	metadata, executions := newTestHostedControl(t)
	s := &server{
		defaults:        sessionDefaults{cwd: t.TempDir()},
		codex:           newCodexDaemon(wsURL, supervisedDaemonLabel),
		spoolDir:        t.TempDir(),
		runtimeEvents:   newRuntimeHub(),
		sessions:        map[string]*session{},
		controlMetadata: metadata,
		agentExecutions: executions,
	}
	s.codex.supervisedJobLoaded = func() bool { return true }
	s.runtime = s
	// Session handshakes run in background. Tear them down before the temporary
	// control.sqlite cleanup so a late provider reply cannot query a closed DB.
	t.Cleanup(func() {
		s.mu.Lock()
		sessions := make([]*session, 0, len(s.sessions))
		for _, sess := range s.sessions {
			sessions = append(sessions, sess)
		}
		s.mu.Unlock()
		for _, sess := range sessions {
			sess.close()
		}
	})
	return s
}

func postResume(t *testing.T, s *server, threadID string) *httptest.ResponseRecorder {
	t.Helper()
	seedCanonicalResumeOwner(t, s, "codex", threadID, s.defaults.cwd)
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
		s.mu.Lock()
		sess := s.sessions[sessionID]
		s.mu.Unlock()
		if sess == nil {
			t.Fatalf("session %s disappeared while waiting for status %q", sessionID, want)
		}
		if sum := sess.summary(); sum.Status == want {
			return sum
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s never reached status %q", sessionID, want)
	return sessionSummary{}
}

func TestCodexDaemonReconnectsExistingSessionsWithoutAnotherRPC(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	rec := postResume(t, s, "thread-recover-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d %s", rec.Code, rec.Body.String())
	}
	var spawned sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &spawned); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	waitForSessionStatus(t, s, spawned.ID, "ready")
	if got := fake.resumeCount(); got != 1 {
		t.Fatalf("initial resume count = %d, want 1", got)
	}

	// No model, MCP, spawn, or send request follows this drop. Recovery must be
	// initiated by the connection-loss path itself; otherwise /turn can never
	// help because reconnecting sessions fail its readiness gate.
	fake.dropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fake.resumeCount() >= 2 && waitSessionStatus(s, spawned.ID) == "ready" {
			s.mu.Lock()
			sess := s.sessions[spawned.ID]
			s.mu.Unlock()
			if sess != nil {
				sess.close()
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon drop was not recovered autonomously: resumes=%d status=%s", fake.resumeCount(), waitSessionStatus(s, spawned.ID))
}

func TestCodexDaemonReconnectOwnsResumeFailureFromDroppedSocket(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.setReplyDelay("thread/resume", 2*time.Second)
	s := newResumeTestServer(t, fake.wsURL())
	rec := postResume(t, s, "thread-recover-inflight")
	var spawned sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &spawned); err != nil {
		t.Fatalf("decode resume response: %v", err)
	}
	waitForWireMethod(t, s, spawned.ID, "thread/resume")

	fake.setReplyDelay("thread/resume", 0)
	fake.dropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fake.resumeCount() >= 2 && waitSessionStatus(s, spawned.ID) == "ready" {
			s.mu.Lock()
			sess := s.sessions[spawned.ID]
			s.mu.Unlock()
			if sess != nil {
				sess.close()
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("in-flight resume loss was not recovered: resumes=%d status=%s", fake.resumeCount(), waitSessionStatus(s, spawned.ID))
}

func waitSessionStatus(s *server, sessionID string) string {
	s.mu.Lock()
	sess := s.sessions[sessionID]
	s.mu.Unlock()
	if sess == nil {
		return "missing"
	}
	return sess.summary().Status
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
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-00000000071e"
	hostedID := seedCanonicalResumeOwner(t, s, "codex", threadID, s.defaults.cwd)
	if _, err := s.controlMetadata.appendEvent(metadataEvent{HostedThreadID: hostedID, Field: postureFieldServiceTier, Value: "default",
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

	fake.setResumeErrorSequence("thread not found")
	waitForSessionStatus(t, s, resume().ID, "exited")
	view, err := s.controlMetadata.view(hostedID)
	if err != nil || view.Fields[postureFieldServiceTier].DesiredDisposition != metadataDispositionRequested {
		t.Fatalf("tier after a failed resume = %+v (%v), want still requested", view.Fields[postureFieldServiceTier], err)
	}

	waitForSessionStatus(t, s, resume().ID, "ready")
	fake.dropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for fake.resumeCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	fake.mu.Lock()
	tiers := append([]string(nil), fake.resumeTiers...)
	fake.mu.Unlock()
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
	s := newResumeTestServer(t, fake.wsURL())
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
	s.codex.mu.Lock()
	routed := s.codex.sessions[threadID]
	s.codex.mu.Unlock()
	if routed == nil || routed.id != sum.ID {
		t.Fatalf("thread %s not routed to session %s", threadID, sum.ID)
	}
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
	fake.setResumeTurns(json.RawMessage(`[{"id":"turn-durable-terminal","status":"inProgress","startedAt":1785510514}]`))
	// Hold the reply until the test replaces the default catalog-backed tail.
	// waitForWireMethod observes the request before this delayed response lands.
	fake.setReplyDelay("thread/resume", time.Second)
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "thread-durable-terminal"

	rec := postResume(t, s, threadID)
	spawned := decodeSummary(t, rec)
	waitForWireMethod(t, s, spawned.ID, "thread/resume")
	s.mu.Lock()
	sess := s.sessions[spawned.ID]
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("resumed session missing")
	}
	sess.savedTurn = func(_ context.Context, turnID string) (durableTurnEvidence, bool) {
		return durableTurnEvidence{Turn: liveProjectionTurn{ID: turnID, Status: "completed", CompletedAtMs: 1785510514001}}, turnID == "turn-durable-terminal"
	}

	summary := waitForSessionStatus(t, s, spawned.ID, "ready")
	if summary.TurnActive {
		t.Fatalf("resume handshake resurrected durable terminal turn: %#v", summary)
	}
	sess.stateMu.Lock()
	guard := sess.durablyTerminalTurnID
	sess.stateMu.Unlock()
	if guard != "turn-durable-terminal" {
		t.Fatalf("durable terminal guard = %q, want resumed turn", guard)
	}
}

// TestResumeConflictReturns409Shape byte-checks the common 409 contract
// and proves the losing resume produced no wire traffic (no second
// thread/resume reached the daemon).
func TestResumeConflictReturns409Shape(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
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
	if n := fake.resumeCount(); n != 1 {
		t.Fatalf("thread/resume reached the wire %d times, want 1 (guard must fire before wire traffic)", n)
	}
}

// TestConcurrentDoubleResumeOneWinner races two resumes of the same thread:
// exactly one spawns, the loser's 409 names the winner's session.
func TestConcurrentDoubleResumeOneWinner(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
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
	if n := fake.resumeCount(); n != 1 {
		t.Fatalf("thread/resume reached the wire %d times, want 1", n)
	}
}

// TestResumeBackgroundFailureExitsAndReleasesClaim pins the frozen R3 failure
// contract: the POST itself succeeds (async spawn), the background handshake
// failure lands as status exited with the message surfaced as "error" on the
// summary and /state, the claim is released, and a second resume succeeds.
func TestResumeBackgroundFailureExitsAndReleasesClaim(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000004"

	fake.setResumeError("no rollout found for thread")
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

	s.codex.mu.Lock()
	_, claimed := s.codex.sessions[threadID]
	s.codex.mu.Unlock()
	if claimed {
		t.Fatal("failed spawn left the thread claim in the routing map")
	}
	fake.setResumeError("")
	if rec = postResume(t, s, threadID); rec.Code != http.StatusOK {
		t.Fatalf("retry after failure status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// A dead session has no routing claim, so accepting a turn would run work that
// nano cannot stream or control. Zero-turn boot recovery becomes ready before
// accepting its first turn; every other exited session must reject one.
func TestExitedCodexSessionRejectsTurn(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000014"

	fake.setResumeError("no rollout found for thread")
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
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000005"
	const handshakeDelay = 2 * time.Second

	fake.setReplyDelay("thread/resume", handshakeDelay)
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
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000006"

	fake.setResumeTurns(json.RawMessage(`[{"id":"turn_1","items":[{"type":"agentMessage","text":"hello"}]}]`))
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

// The compact state/event copies still omit app-server history, but the
// normalized conversation projection must consume the bounded tail before
// that omission or a restart in the middle of a turn loses its prompt prefix.
func TestResumeHandshakeHydratesConversationProjection(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000007"
	fake.setResumeTurns(json.RawMessage(`[
		{"id":"turn-1","status":"completed","startedAt":1785510000,"completedAt":1785510001,"items":[
			{"id":"user-1","type":"userMessage","content":[{"type":"text","text":"before restart"}]},
			{"id":"agent-1","type":"agentMessage","text":"completed answer"}
		]},
		{"id":"turn-2","status":"inProgress","startedAt":1785510100,"items":[
			{"id":"user-2","type":"userMessage","content":[{"type":"text","text":"survive restart"}]},
			{"id":"agent-2","type":"agentMessage","text":"partial answer"}
		]}
	]`))

	sum := decodeSummary(t, postResume(t, s, threadID))
	waitForSessionStatus(t, s, sum.ID, "ready")
	s.mu.Lock()
	sess := s.sessions[sum.ID]
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("resumed session missing")
	}
	snapshot := projectionStateForTest(sess.projection)
	if snapshot.NativeThreadID != threadID {
		t.Fatalf("projection thread = %q, want %q", snapshot.NativeThreadID, threadID)
	}
	if len(snapshot.Turns) != 2 || snapshot.Turns[1].Status != "in_progress" || !snapshot.Runtime.TurnActive {
		t.Fatalf("resumed projection turns/runtime = %#v / %#v", snapshot.Turns, snapshot.Runtime)
	}
}

// TestResumeWireReplyOmitsHistoricalTurns proves the large history returned by
// app-server never reaches Nano's stored event pipeline. The handshake still
// receives the original reply and preserves its compact state snapshot.
func TestResumeWireReplyOmitsHistoricalTurns(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000008"
	const historicalText = "resume-history-must-not-reach-the-event-feed"
	fake.setResumeTurns(json.RawMessage(`[{"id":"turn_1","items":[{"type":"agentMessage","text":"` + historicalText + strings.Repeat("x", 128*1024) + `"}]}]`))

	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	waitForSessionStatus(t, s, sum.ID, "ready")

	s.mu.Lock()
	sess := s.sessions[sum.ID]
	s.mu.Unlock()
	if sess == nil {
		t.Fatal("resumed session missing")
	}
	sess.mu.Lock()
	events := append([]wireEvent(nil), sess.events...)
	sess.mu.Unlock()

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
	s := newResumeTestServer(t, fake.wsURL())
	const handshakeDelay = 2 * time.Second

	fake.setReplyDelay("thread/start", handshakeDelay)
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
	s := newResumeTestServer(t, fake.wsURL())

	fake.setStartError("model not available")
	// Hold the provider's answer until the POST has answered: otherwise a
	// loaded machine lets the failure land first and the summary reads exited.
	answer := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(answer) })
	fake.setOnStart(func(json.RawMessage) { <-answer })
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
	// Nothing was ever claimed (a fresh start has no id to pre-claim) and
	// nothing may be left behind by the failure.
	s.codex.mu.Lock()
	routed := len(s.codex.sessions)
	s.codex.mu.Unlock()
	if routed != 0 {
		t.Fatalf("failed start left %d routing entries behind", routed)
	}
}

// TestCloseDuringStartHandshakeLeavesNoZombieRouting closes a fresh start while
// thread/start is still in flight. The late reply must not resurrect the
// session or route the thread it minted to a torn-down one — and because
// close() ran before there was any id to unsubscribe, the handshake owes the
// daemon that unsubscribe itself.
func TestCloseDuringStartHandshakeLeavesNoZombieRouting(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	const handshakeDelay = 250 * time.Millisecond

	fake.setReplyDelay("thread/start", handshakeDelay)
	sum := decodeSummary(t, postStart(t, s))

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, body = %s", rec.Code, rec.Body.String())
	}

	time.Sleep(handshakeDelay + 300*time.Millisecond) // let the delayed handshake land
	s.codex.mu.Lock()
	routed := len(s.codex.sessions)
	s.codex.mu.Unlock()
	if routed != 0 {
		t.Fatalf("handshake completion routed %d threads to a closed session", routed)
	}
	s.mu.Lock()
	_, listed := s.sessions[sum.ID]
	s.mu.Unlock()
	if listed {
		t.Fatal("closed session came back into the session list")
	}
	if n := fake.unsubscribed("thread-fresh-1"); n != 1 {
		t.Fatalf("thread/unsubscribe for the abandoned thread ran %d times, want 1", n)
	}
}

// TestCloseDuringResumeHandshakeLeavesNoZombieRouting closes the session while
// the background handshake is still in flight: once the handshake lands, the
// thread must not be routed to the torn-down session and stays resumable.
func TestCloseDuringResumeHandshakeLeavesNoZombieRouting(t *testing.T) {
	fake := newFakeAppServer(t)
	s := newResumeTestServer(t, fake.wsURL())
	const threadID = "0199a213-abcd-7000-8000-000000000007"
	const handshakeDelay = 250 * time.Millisecond

	fake.setReplyDelay("thread/resume", handshakeDelay)
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
	s.codex.mu.Lock()
	_, claimed := s.codex.sessions[threadID]
	s.codex.mu.Unlock()
	if claimed {
		t.Fatal("handshake completion re-routed the thread to a closed session")
	}
	fake.setReplyDelay("thread/resume", 0)
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

// TestRegisterThreadKeepsFirstBinding pins the clobber guard: a conflicting
// registration keeps the first session's routing, and releaseThread only
// removes the entry for its owner.
func TestRegisterThreadKeepsFirstBinding(t *testing.T) {
	d := newCodexDaemon("ws://unused", supervisedDaemonLabel)
	first := &session{id: "aaaa",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	second := &session{id: "bbbb",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	d.registerThread("t1", first)
	d.registerThread("t1", second)
	if d.sessions["t1"] != first {
		t.Fatalf("registerThread clobbered the first binding with %v", d.sessions["t1"])
	}
	d.releaseThread("t1", second)
	if d.sessions["t1"] != first {
		t.Fatal("releaseThread removed a routing entry it did not own")
	}
	d.releaseThread("t1", first)
	if _, ok := d.sessions["t1"]; ok {
		t.Fatal("releaseThread left the owner's entry in place")
	}
}
