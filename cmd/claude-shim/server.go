// server.go — the client-facing surface: JSON-RPC 2.0 over WebSocket, one
// message per text frame, mirroring the shape nano already speaks to the codex
// app-server daemon (internal/wire/codex/client.go).
//
// One connection may drive many sessions, but a session has at most one live
// connection (the single-attachment guard, the analog of nano's claimThread).
// Detach is implicit: when the WebSocket drops, every session that connection
// held becomes attachable again, its child untouched and its spool still
// growing — that is the whole point of the daemon.
//
// Outbound frames go through a per-connection queue drained by one writer
// goroutine. Notifications are emitted while a session lock is held, so they
// must never block on the socket; a client that cannot keep up past
// connSendTimeout is dropped and can re-attach with replay.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Client→shim methods.
const (
	methodStart  = "session/start"
	methodAttach = "session/attach"
	methodInput  = "session/input"
	methodClose  = "session/close"
	methodList   = "session/list"
)

// Shim→client notifications.
const (
	methodLine       = "session/line"
	methodExited     = "session/exited"
	methodReplayDone = "session/replayDone"
)

// JSON-RPC error codes: the standard set plus this shim's session errors.
const (
	codeParseError      = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternalError   = -32603
	codeSessionAttached = -32001
	codeSessionNotFound = -32002
	codeSessionExited   = -32003
)

const (
	jsonRPCVersion   = "2.0"
	connSendQueue    = 1024
	connSendTimeout  = 5 * time.Second
	connWriteTimeout = 10 * time.Second
)

type rpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func errorf(code int, message string) *rpcError { return &rpcError{Code: code, Message: message} }

// Request/response payloads.
type (
	startParams struct {
		Bin           string   `json:"bin"`
		Args          []string `json:"args"`
		Cwd           string   `json:"cwd"`
		TurnIDSession string   `json:"turnIdSession"`
	}
	startResult struct {
		ShimSessionID string `json:"shimSessionId"`
	}
	attachParams struct {
		ShimSessionID   string `json:"shimSessionId"`
		NativeSessionID string `json:"nativeSessionId"`
		FromSeq         int64  `json:"fromSeq"`
	}
	attachResult struct {
		ShimSessionID   string `json:"shimSessionId"`
		NativeSessionID string `json:"nativeSessionId"`
		State           string `json:"state"`
		TurnActive      bool   `json:"turnActive"`
		LastSeq         int64  `json:"lastSeq"`
	}
	inputParams struct {
		ShimSessionID string `json:"shimSessionId"`
		Line          string `json:"line"`
	}
	inputResult struct {
		TurnID string `json:"turnId,omitempty"`
	}
	closeParams struct {
		ShimSessionID string `json:"shimSessionId"`
	}
	listResult struct {
		Sessions []sessionInfo `json:"sessions"`
	}
)

// Notification payloads.
type (
	lineParams struct {
		ShimSessionID string `json:"shimSessionId"`
		Seq           int64  `json:"seq"`
		// TS is when the shim first saw this line, carried so a replay can
		// date its lines to when they happened rather than to the moment the
		// client re-attached. Omitted for a line spooled by an older shim.
		TS     string `json:"ts,omitempty"`
		Dir    string `json:"dir"`
		Line   string `json:"line"`
		TurnID string `json:"turnId,omitempty"`
	}
	exitedParams struct {
		ShimSessionID string `json:"shimSessionId"`
		ExitCode      int    `json:"exitCode"`
	}
	replayDoneParams struct {
		ShimSessionID string `json:"shimSessionId"`
		LastSeq       int64  `json:"lastSeq"`
	}
)

type server struct {
	mgr      *manager
	upgrader websocket.Upgrader
}

func newServer(spoolDir string) *server {
	return &server{
		mgr: newManager(spoolDir),
		upgrader: websocket.Upgrader{
			// Browser-origin protection, same posture as the codex daemon:
			// a handshake carrying an Origin header is a browser, and this
			// daemon has no business being reachable from a page.
			CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
		},
	}
}

func (srv *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := srv.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade from %s: %v", r.RemoteAddr, err)
		return
	}
	newConn(srv, ws).serve()
}

// conn is one client connection: a JSON-RPC peer plus the set of sessions it
// currently holds the attachment for.
type conn struct {
	srv         *server
	ws          *websocket.Conn
	out         chan []byte
	priorityOut chan []byte
	done        chan struct{}

	mu       sync.Mutex
	sessions map[string]*shimSession
	closed   bool
}

func newConn(srv *server, ws *websocket.Conn) *conn {
	return &conn{
		srv:         srv,
		ws:          ws,
		out:         make(chan []byte, connSendQueue),
		priorityOut: make(chan []byte, connSendQueue),
		done:        make(chan struct{}),
		sessions:    map[string]*shimSession{},
	}
}

func (c *conn) serve() {
	go c.writeLoop()
	for {
		_, frame, err := c.ws.ReadMessage()
		if err != nil {
			break
		}
		c.handleFrame(frame)
	}
	c.shutdown()
}

// shutdown detaches every session this connection held. The children keep
// running and their spools keep growing; a later attach replays the gap.
func (c *conn) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	held := make([]*shimSession, 0, len(c.sessions))
	for _, s := range c.sessions {
		held = append(held, s)
	}
	c.sessions = map[string]*shimSession{}
	c.mu.Unlock()

	close(c.done)
	c.ws.Close()
	for _, s := range held {
		s.detach(c)
	}
}

func (c *conn) writeLoop() {
	for {
		var frame []byte
		select {
		case frame = <-c.priorityOut:
		default:
			select {
			case frame = <-c.priorityOut:
			case frame = <-c.out:
			case <-c.done:
				return
			}
		}
		if err := c.ws.SetWriteDeadline(time.Now().Add(connWriteTimeout)); err != nil {
			c.ws.Close()
			return
		}
		if err := c.ws.WriteMessage(websocket.TextMessage, frame); err != nil {
			c.ws.Close() // unblocks the read loop, which runs shutdown
			return
		}
		_ = c.ws.SetWriteDeadline(time.Time{})
	}
}

// trackAttached remembers a session so a dropped connection detaches it. It
// reports false if the connection has already shut down — the caller took the
// attachment on a corpse and must give it back.
func (c *conn) trackAttached(s *shimSession) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.sessions[s.id] = s
	return true
}

func (c *conn) forgetAttached(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sessions, id)
}

func (c *conn) notify(method string, params any) bool {
	return c.send(rpcNotification{JSONRPC: jsonRPCVersion, Method: method, Params: params})
}

// Responses bypass the replay notification queue. A large attach replay must
// not make a live session/input acknowledgement wait behind thousands of
// historical frames on the same WebSocket.
func (c *conn) respondPriority(msg any) {
	frame, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshaling outbound response: %v", err)
		return
	}
	timer := time.NewTimer(connSendTimeout)
	defer timer.Stop()
	select {
	case c.priorityOut <- frame:
	case <-c.done:
	case <-timer.C:
		log.Printf("client %s is not draining its response queue; dropping connection", c.ws.RemoteAddr())
		c.ws.Close()
	}
}

// send queues one outbound message. It never blocks indefinitely: callers hold
// a session lock, so a wedged socket must cost the connection, not the child.
func (c *conn) send(msg any) bool {
	frame, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshaling outbound message: %v", err)
		return false
	}
	timer := time.NewTimer(connSendTimeout)
	defer timer.Stop()
	select {
	case c.out <- frame:
		return true
	case <-c.done:
		return false
	case <-timer.C:
		log.Printf("client %s is not draining its queue; dropping connection", c.ws.RemoteAddr())
		c.ws.Close()
		return false
	}
}

func (c *conn) handleFrame(frame []byte) {
	var req rpcRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		c.respond(nil, nil, errorf(codeParseError, "parse error"))
		return
	}
	if req.Method == "" {
		c.respond(req.ID, nil, errorf(codeInvalidRequest, "missing method"))
		return
	}
	if req.ID == nil {
		return // the shim defines no client→shim notifications
	}
	c.dispatch(req.ID, req.Method, req.Params)
}

// dispatch runs one request to completion, including its response. Requests
// are handled serially per connection, and session/start and session/attach
// both respond from inside the handler: their result must be on the wire
// before the first notification about the session it names.
func (c *conn) dispatch(id *json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case methodStart:
		c.handleStart(id, params)
	case methodAttach:
		c.handleAttach(id, params)
	case methodInput:
		result, rpcErr := c.handleInput(params)
		c.respond(id, result, rpcErr)
	case methodClose:
		result, rpcErr := c.handleClose(params)
		c.respond(id, result, rpcErr)
	case methodList:
		c.respond(id, listResult{Sessions: c.srv.mgr.infos()}, nil)
	default:
		c.respond(id, nil, errorf(codeMethodNotFound, "unknown method "+method))
	}
}

func (c *conn) respond(id *json.RawMessage, result any, rpcErr *rpcError) {
	resp := rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Error: rpcErr}
	if rpcErr == nil {
		resp.Result = result
	}
	c.respondPriority(resp)
}

func (c *conn) handleStart(id *json.RawMessage, raw json.RawMessage) {
	var p startParams
	if err := json.Unmarshal(raw, &p); err != nil {
		c.respond(id, nil, errorf(codeInvalidParams, err.Error()))
		return
	}
	if p.Bin == "" {
		c.respond(id, nil, errorf(codeInvalidParams, "bin is required"))
		return
	}
	s, err := c.srv.mgr.start(p.Bin, p.Args, p.Cwd, p.TurnIDSession, c)
	if err != nil {
		c.respond(id, nil, errorf(codeInternalError, err.Error()))
		return
	}
	c.respond(id, startResult{ShimSessionID: s.id}, nil)
	s.beginReading()
}

func (c *conn) handleAttach(id *json.RawMessage, raw json.RawMessage) {
	var p attachParams
	if err := json.Unmarshal(raw, &p); err != nil {
		c.respond(id, nil, errorf(codeInvalidParams, err.Error()))
		return
	}
	if p.ShimSessionID == "" && p.NativeSessionID == "" {
		c.respond(id, nil, errorf(codeInvalidParams, "shimSessionId or nativeSessionId is required"))
		return
	}
	s := c.srv.mgr.byShimID(p.ShimSessionID)
	if s == nil {
		s = c.srv.mgr.byNativeID(p.NativeSessionID)
	}
	if s == nil {
		c.respond(id, nil, errorf(codeSessionNotFound, "session_not_found"))
		return
	}

	err := s.attach(c, p.FromSeq, func(info sessionInfo) {
		c.respond(id, attachResult{
			ShimSessionID:   info.ShimSessionID,
			NativeSessionID: info.NativeSessionID,
			State:           info.State,
			TurnActive:      info.TurnActive,
			LastSeq:         info.LastSeq,
		}, nil)
	})
	switch {
	case errors.Is(err, errSessionAttached):
		c.respond(id, nil, errorf(codeSessionAttached, "session_attached"))
	case errors.Is(err, errReplayClientDropped):
		// The attach result was already sent. The failed replay closed this
		// socket and released the session for the next client; no second RPC
		// response can be delivered or would be meaningful.
		return
	case err != nil:
		c.respond(id, nil, errorf(codeInternalError, err.Error()))
	}
}

func (c *conn) handleInput(raw json.RawMessage) (any, *rpcError) {
	var p inputParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errorf(codeInvalidParams, err.Error())
	}
	s := c.srv.mgr.byShimID(p.ShimSessionID)
	if s == nil {
		return nil, errorf(codeSessionNotFound, "session_not_found")
	}
	turnID, err := s.writeInputWithTurnID(p.Line)
	switch {
	case errors.Is(err, errSessionExited):
		return nil, errorf(codeSessionExited, "session_exited")
	case err != nil:
		return nil, errorf(codeInternalError, err.Error())
	}
	return inputResult{TurnID: turnID}, nil
}

func (c *conn) handleClose(raw json.RawMessage) (any, *rpcError) {
	var p closeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errorf(codeInvalidParams, err.Error())
	}
	s := c.srv.mgr.take(p.ShimSessionID)
	if s == nil {
		return nil, errorf(codeSessionNotFound, "session_not_found")
	}
	c.forgetAttached(s.id)
	s.close()
	return struct{}{}, nil
}
