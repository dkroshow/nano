// harness_test.go — an in-process shim (httptest server) plus a minimal
// JSON-RPC-over-WebSocket client, so the tests exercise the real wire surface
// rather than the handlers directly.
package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	testCallTimeout = 5 * time.Second
	testWaitTimeout = 5 * time.Second
	testPollEvery   = 2 * time.Millisecond
)

type testShim struct {
	t   *testing.T
	srv *server
	url string
	dir string
	cwd string // working directory every child in this test is started in
}

func newTestShim(t *testing.T) *testShim {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "spools")
	if err := prepareSpoolDir(dir); err != nil {
		t.Fatalf("preparing spool dir: %v", err)
	}
	srv := newServer(dir)
	hs := httptest.NewServer(srv)
	t.Cleanup(func() {
		for _, info := range srv.mgr.infos() {
			if s := srv.mgr.take(info.ShimSessionID); s != nil {
				s.close()
			}
		}
		hs.Close()
	})
	return &testShim{
		t:   t,
		srv: srv,
		url: "ws" + strings.TrimPrefix(hs.URL, "http"),
		dir: dir,
		cwd: t.TempDir(),
	}
}

// start dials a fresh connection and starts a fake child on it, returning the
// connection (now the attached one) and the shim session id.
func (sh *testShim) start(nativeID string, extra ...string) (*testClient, string) {
	sh.t.Helper()
	tc := sh.dial()
	var res startResult
	tc.mustCall(methodStart, startParams{
		Bin:  os.Args[0],
		Args: fakeChildArgs(nativeID, extra...),
		Cwd:  sh.cwd,
	}, &res)
	if res.ShimSessionID == "" {
		sh.t.Fatal("session/start returned an empty shimSessionId")
	}
	return tc, res.ShimSessionID
}

func (sh *testShim) dial() *testClient {
	sh.t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(sh.url, nil)
	if err != nil {
		sh.t.Fatalf("dialing %s: %v", sh.url, err)
	}
	tc := &testClient{t: sh.t, ws: ws, pending: map[int64]chan rawMessage{}}
	sh.t.Cleanup(tc.close)
	go tc.readLoop()
	return tc
}

// session reaches into the running shim; used for assertions the wire does not
// expose (spool contents, attachment bookkeeping).
func (sh *testShim) session(id string) *shimSession {
	sh.t.Helper()
	s := sh.srv.mgr.byShimID(id)
	if s == nil {
		sh.t.Fatalf("session %s is not registered", id)
	}
	return s
}

func (sh *testShim) waitDetached(id string) {
	sh.t.Helper()
	s := sh.session(id)
	waitFor(sh.t, "session "+id+" to be detached", func() bool { return !s.info().Attached })
}

func (sh *testShim) spoolEvents(id string) []spoolEvent {
	sh.t.Helper()
	events, err := sh.session(id).spool.readAfter(0)
	if err != nil {
		sh.t.Fatalf("reading spool for %s: %v", id, err)
	}
	return events
}

func (sh *testShim) spoolPath(id string) string { return filepath.Join(sh.dir, id+".jsonl") }

// rawMessage is any inbound frame: a response when ID is set, a notification
// when Method is.
type rawMessage struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type testClient struct {
	t  *testing.T
	ws *websocket.Conn

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rawMessage
	frames  []rawMessage // every inbound frame, in arrival order
}

func (tc *testClient) readLoop() {
	for {
		_, frame, err := tc.ws.ReadMessage()
		if err != nil {
			return
		}
		var msg rawMessage
		if err := json.Unmarshal(frame, &msg); err != nil {
			tc.t.Errorf("undecodable frame %q: %v", frame, err)
			continue
		}
		tc.mu.Lock()
		tc.frames = append(tc.frames, msg)
		if ch := tc.pending[idOf(msg)]; ch != nil {
			ch <- msg
		}
		tc.mu.Unlock()
	}
}

func (tc *testClient) call(method string, params, result any) *rpcError {
	tc.t.Helper()
	tc.mu.Lock()
	tc.nextID++
	id := tc.nextID
	ch := make(chan rawMessage, 1)
	tc.pending[id] = ch
	tc.mu.Unlock()
	defer func() {
		tc.mu.Lock()
		delete(tc.pending, id)
		tc.mu.Unlock()
	}()

	frame, err := json.Marshal(map[string]any{"jsonrpc": jsonRPCVersion, "id": id, "method": method, "params": params})
	if err != nil {
		tc.t.Fatalf("marshaling %s: %v", method, err)
	}
	tc.writeMu.Lock()
	err = tc.ws.WriteMessage(websocket.TextMessage, frame)
	tc.writeMu.Unlock()
	if err != nil {
		tc.t.Fatalf("sending %s: %v", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				tc.t.Fatalf("decoding %s result: %v", method, err)
			}
		}
		return nil
	case <-time.After(testCallTimeout):
		tc.t.Fatalf("timed out waiting for a %s response", method)
		return nil
	}
}

func (tc *testClient) mustCall(method string, params, result any) {
	tc.t.Helper()
	if rpcErr := tc.call(method, params, result); rpcErr != nil {
		tc.t.Fatalf("%s failed: %v (code %d)", method, rpcErr.Message, rpcErr.Code)
	}
}

func (tc *testClient) callErr(method string, params any) *rpcError {
	tc.t.Helper()
	rpcErr := tc.call(method, params, nil)
	if rpcErr == nil {
		tc.t.Fatalf("%s unexpectedly succeeded", method)
	}
	return rpcErr
}

func (tc *testClient) input(sessionID, line string) {
	tc.t.Helper()
	tc.mustCall(methodInput, inputParams{ShimSessionID: sessionID, Line: line}, nil)
}

// idOf returns the request id a frame answers, or 0 for a notification.
func idOf(msg rawMessage) int64 {
	if msg.ID == nil {
		return 0
	}
	return *msg.ID
}

func (tc *testClient) inbound() []rawMessage {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return append([]rawMessage(nil), tc.frames...)
}

func (tc *testClient) notifications() []rawMessage {
	var out []rawMessage
	for _, msg := range tc.inbound() {
		if msg.Method != "" {
			out = append(out, msg)
		}
	}
	return out
}

// frameKinds labels every frame received so far — a notification by its
// method, a response as "result" — so tests can assert their relative order.
func (tc *testClient) frameKinds() []string {
	var out []string
	for _, msg := range tc.inbound() {
		if msg.Method == "" {
			out = append(out, "result")
			continue
		}
		out = append(out, msg.Method)
	}
	return out
}

// lines returns the session/line notifications received so far, in order.
func (tc *testClient) lines() []lineParams {
	tc.t.Helper()
	var out []lineParams
	for _, msg := range tc.notifications() {
		if msg.Method != methodLine {
			continue
		}
		var p lineParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			tc.t.Fatalf("decoding %s params: %v", methodLine, err)
		}
		out = append(out, p)
	}
	return out
}

func (tc *testClient) waitLines(want int) []lineParams {
	tc.t.Helper()
	waitFor(tc.t, "at least "+strconv.Itoa(want)+" session/line notifications", func() bool {
		return len(tc.lines()) >= want
	})
	return tc.lines()
}

func (tc *testClient) waitMethod(method string) rawMessage {
	tc.t.Helper()
	var found rawMessage
	waitFor(tc.t, "a "+method+" notification", func() bool {
		for _, msg := range tc.notifications() {
			if msg.Method == method {
				found = msg
				return true
			}
		}
		return false
	})
	return found
}

// methodOrder returns the arrival positions of each notification method, so
// tests can assert ordering (replayDone before exited, say).
func (tc *testClient) methodOrder() []string {
	var out []string
	for _, msg := range tc.notifications() {
		out = append(out, msg.Method)
	}
	return out
}

func (tc *testClient) decode(msg rawMessage, into any) {
	tc.t.Helper()
	if err := json.Unmarshal(msg.Params, into); err != nil {
		tc.t.Fatalf("decoding %s params: %v", msg.Method, err)
	}
}

func (tc *testClient) close() { tc.ws.Close() }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWaitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(testPollEvery)
	}
	t.Fatalf("timed out waiting for %s", what)
}
