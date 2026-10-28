// Package codexserver implements the scripted Codex JSON-RPC endpoint used by tests.
package codexserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Server is a minimal codex app-server daemon: a websocket endpoint
// answering initialize/thread calls so daemon-backed spawn paths run in tests
// without a codex binary. Responses use the real wire framing (one JSON-RPC
// message per text frame, no "jsonrpc" field). A per-method reply delay lets
// tests hold a thread handshake in flight without stalling other traffic.
type Server struct {
	Srv *httptest.Server

	Mu                sync.Mutex
	ResumeErr         string // non-empty: thread/resume answers this RPC error message
	ResumeErrors      []string
	StartErr          string // non-empty: thread/start answers this RPC error message
	TurnStartErr      string // non-empty: turn/start answers this RPC error message
	TurnStartResult   map[string]any
	DropStart         bool // thread/start closes transport without an RPC answer
	StartParams       json.RawMessage
	StartResult       map[string]any
	ResumeResult      map[string]any
	ModelList         map[string]any // model/list result; nil answers an empty result
	OnStart           func(json.RawMessage)
	OnResume          func(json.RawMessage)
	OnTurnStart       func(string)
	TurnStartReply    func(string) (json.RawMessage, error)
	WithholdTurnStart bool // accepts a request without returning a response
	OnTurnInterrupt   func()
	ReplyDelays       map[string]time.Duration
	ResumeDelays      map[string]time.Duration
	ResumeCounts      map[string]int
	ResumeTiers       []string // each thread/resume's serviceTier param, in arrival order
	TurnStarts        int
	TurnStartTexts    []string          // each turn/start's prompt text, in arrival order
	Steers            []json.RawMessage // turn/steer params, in arrival order
	SteerErr          string            // non-empty: turn/steer answers this RPC error message
	DropSteer         bool              // turn/steer closes the transport without an RPC answer
	ResumeTurns       json.RawMessage   // non-nil: thread/resume responses carry these thread.turns
	Resumes           int
	Unsubscribes      map[string]int // thread/unsubscribe requests, by threadId
	McpCalls          int            // mcpServerStatus/list requests received
	McpDetails        []string       // the `detail` param of each of those requests
	SkillsCalls       int            // skills/list requests received
	SkillsCwds        []string       // the sole cwd carried by each request
	SkillsInFlight    int
	SkillsMaxFlight   int
	Connections       map[*websocket.Conn]*sync.Mutex
	subscriptions     map[*websocket.Conn]map[string]bool
}

func NewServer(t testing.TB) *Server {
	t.Helper()
	f := &Server{
		ReplyDelays: map[string]time.Duration{}, Unsubscribes: map[string]int{},
		ResumeDelays: map[string]time.Duration{}, ResumeCounts: map[string]int{},
		Connections:   map[*websocket.Conn]*sync.Mutex{},
		subscriptions: map[*websocket.Conn]map[string]bool{},
	}
	upgrader := websocket.Upgrader{}
	f.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.Mu.Lock()
		var writeMu sync.Mutex // delayed replies and test notifications share one writer
		f.Connections[conn] = &writeMu
		f.subscriptions[conn] = map[string]bool{}
		f.Mu.Unlock()
		defer func() {
			f.Mu.Lock()
			delete(f.Connections, conn)
			delete(f.subscriptions, conn)
			f.Mu.Unlock()
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
				f.Mu.Lock()
				f.SkillsCalls++
				f.SkillsCwds = append(f.SkillsCwds, cwd)
				f.SkillsInFlight++
				if f.SkillsInFlight > f.SkillsMaxFlight {
					f.SkillsMaxFlight = f.SkillsInFlight
				}
				f.Mu.Unlock()
			}
			reply := f.Respond(conn, msg.ID, msg.Method, msg.Params)
			f.Mu.Lock()
			delay := f.ReplyDelays[msg.Method]
			if msg.Method == "thread/resume" {
				var params struct {
					ThreadID string `json:"threadId"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				if configured, ok := f.ResumeDelays[params.ThreadID]; ok {
					delay = configured
				}
			}
			drop := msg.Method == "thread/start" && f.DropStart || msg.Method == "turn/steer" && f.DropSteer
			withhold := msg.Method == "turn/start" && f.WithholdTurnStart
			f.Mu.Unlock()
			if drop {
				return
			}
			if withhold {
				continue
			}
			if delay > 0 {
				go func() {
					time.Sleep(delay)
					_ = writeFrame(reply)
					if isSkills {
						f.Mu.Lock()
						f.SkillsInFlight--
						f.Mu.Unlock()
					}
				}()
				continue
			}
			if err := writeFrame(reply); err != nil {
				return
			}
			if isSkills {
				f.Mu.Lock()
				f.SkillsInFlight--
				f.Mu.Unlock()
			}
		}
	}))
	t.Cleanup(f.Srv.Close)
	return f
}

func (f *Server) WSURL() string {
	return "ws" + strings.TrimPrefix(f.Srv.URL, "http")
}

func (f *Server) SetResumeError(message string) {
	f.Mu.Lock()
	f.ResumeErr = message
	f.Mu.Unlock()
}

func (f *Server) SetResumeErrorSequence(messages ...string) {
	f.Mu.Lock()
	f.ResumeErrors = append([]string(nil), messages...)
	f.Mu.Unlock()
}

func (f *Server) SetStartError(message string) {
	f.Mu.Lock()
	f.StartErr = message
	f.Mu.Unlock()
}

func (f *Server) SetTurnStartError(message string) {
	f.Mu.Lock()
	f.TurnStartErr = message
	f.Mu.Unlock()
}

func (f *Server) SetTurnStartResult(result map[string]any) {
	f.Mu.Lock()
	f.TurnStartResult = result
	f.Mu.Unlock()
}

func (f *Server) StartedTexts() []string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]string(nil), f.TurnStartTexts...)
}

func (f *Server) SetSteerError(message string) {
	f.Mu.Lock()
	f.SteerErr = message
	f.Mu.Unlock()
}

func (f *Server) SetDropSteer(drop bool) {
	f.Mu.Lock()
	f.DropSteer = drop
	f.Mu.Unlock()
}

func (f *Server) SteerParams() []json.RawMessage {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]json.RawMessage(nil), f.Steers...)
}

func (f *Server) SetDropStart(drop bool) {
	f.Mu.Lock()
	f.DropStart = drop
	f.Mu.Unlock()
}

func (f *Server) SetStartResult(result map[string]any) {
	f.Mu.Lock()
	f.StartResult = result
	f.Mu.Unlock()
}

func (f *Server) SetResumeResult(result map[string]any) {
	f.Mu.Lock()
	f.ResumeResult = result
	f.Mu.Unlock()
}

func (f *Server) SetOnStart(fn func(json.RawMessage)) {
	f.Mu.Lock()
	f.OnStart = fn
	f.Mu.Unlock()
}

// unsubscribed counts the thread/unsubscribe calls received for threadID.
func (f *Server) Unsubscribed(threadID string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Unsubscribes[threadID]
}

func (f *Server) SetReplyDelay(method string, delay time.Duration) {
	f.Mu.Lock()
	f.ReplyDelays[method] = delay
	f.Mu.Unlock()
}

func (f *Server) SetResumeTurns(turns json.RawMessage) {
	f.Mu.Lock()
	f.ResumeTurns = turns
	f.Mu.Unlock()
}

func (f *Server) ResumeCount() int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.Resumes
}

func (f *Server) DropConnections() {
	f.Mu.Lock()
	connections := make([]*websocket.Conn, 0, len(f.Connections))
	for conn := range f.Connections {
		connections = append(connections, conn)
	}
	f.Mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (f *Server) EmitThreadNotification(threadID string, payload map[string]any) {
	f.emitNotification(threadID, payload, true)
}

// EmitNotification sends a daemon frame even when its thread RPC was refused.
// This exercises Nano's routing claim independently of provider subscription state.
func (f *Server) EmitNotification(payload map[string]any) {
	f.emitNotification("", payload, false)
}

// EmitRawThreadNotification preserves a captured frame's original bytes.
func (f *Server) EmitRawThreadNotification(threadID string, frame []byte) {
	f.emitFrame(threadID, frame, true)
}

func (f *Server) emitNotification(threadID string, payload map[string]any, subscribed bool) {
	frame, _ := json.Marshal(payload)
	f.emitFrame(threadID, frame, subscribed)
}

func (f *Server) emitFrame(threadID string, frame []byte, subscribed bool) {
	f.Mu.Lock()
	type target struct {
		conn *websocket.Conn
		mu   *sync.Mutex
	}
	targets := []target{}
	for conn, writeMu := range f.Connections {
		if !subscribed || f.subscriptions[conn][threadID] {
			targets = append(targets, target{conn: conn, mu: writeMu})
		}
	}
	f.Mu.Unlock()
	for _, candidate := range targets {
		candidate.mu.Lock()
		_ = candidate.conn.WriteMessage(websocket.TextMessage, frame)
		candidate.mu.Unlock()
	}
}

func (f *Server) McpListCalls() (int, []string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.McpCalls, append([]string(nil), f.McpDetails...)
}

func (f *Server) SkillsListCalls() (calls, maxInFlight int, cwds []string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.SkillsCalls, f.SkillsMaxFlight, append([]string(nil), f.SkillsCwds...)
}

func (f *Server) Respond(conn *websocket.Conn, id json.RawMessage, method string, params json.RawMessage) []byte {
	result := map[string]any{}
	switch method {
	case "model/list":
		f.Mu.Lock()
		if f.ModelList != nil {
			result = f.ModelList
		}
		f.Mu.Unlock()
	case "thread/read":
		var req struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &req)
		result = map[string]any{"thread": map[string]any{"id": req.ThreadID, "cwd": "/tmp"}}
	case "thread/resume":
		f.Mu.Lock()
		f.Resumes++
		var target struct {
			ThreadID    string `json:"threadId"`
			ServiceTier string `json:"serviceTier"`
		}
		_ = json.Unmarshal(params, &target)
		f.ResumeCounts[target.ThreadID]++
		f.ResumeTiers = append(f.ResumeTiers, target.ServiceTier)
		resumeErr := f.ResumeErr
		if len(f.ResumeErrors) > 0 {
			resumeErr = f.ResumeErrors[0]
			f.ResumeErrors = f.ResumeErrors[1:]
		}
		resumeTurns := f.ResumeTurns
		resumeResult := f.ResumeResult
		onResume := f.OnResume
		f.Mu.Unlock()
		if onResume != nil {
			onResume(append(json.RawMessage(nil), params...))
		}
		if resumeErr != "" {
			out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": resumeErr}})
			return out
		}
		f.Mu.Lock()
		if f.subscriptions[conn] != nil {
			f.subscriptions[conn][target.ThreadID] = true
		}
		f.Mu.Unlock()
		if resumeResult != nil {
			result = resumeResult
			break
		}
		var req struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &req)
		thread := map[string]any{"id": req.ThreadID, "cwd": "/tmp"}
		if resumeTurns != nil {
			thread["turns"] = resumeTurns
		}
		result = map[string]any{"model": "gpt-test", "thread": thread}
	case "thread/start":
		f.Mu.Lock()
		f.StartParams = append(f.StartParams[:0], params...)
		startErr := f.StartErr
		startResult := f.StartResult
		onStart := f.OnStart
		f.Mu.Unlock()
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
			f.Mu.Lock()
			if f.subscriptions[conn] != nil {
				f.subscriptions[conn][started.Thread.ID] = true
			}
			f.Mu.Unlock()
		}
	case "thread/unsubscribe":
		var req struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &req)
		f.Mu.Lock()
		f.Unsubscribes[req.ThreadID]++
		f.Mu.Unlock()
	case "turn/start":
		var start struct {
			Input []struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.Unmarshal(params, &start)
		f.Mu.Lock()
		f.TurnStarts++
		if len(start.Input) > 0 {
			f.TurnStartTexts = append(f.TurnStartTexts, start.Input[0].Text)
		}
		turnStartErr := f.TurnStartErr
		turnStartResult := f.TurnStartResult
		onTurnStart := f.OnTurnStart
		turnStartReply := f.TurnStartReply
		f.Mu.Unlock()
		if onTurnStart != nil && len(start.Input) > 0 {
			onTurnStart(start.Input[0].Text)
		}
		if turnStartReply != nil {
			text := ""
			if len(start.Input) > 0 {
				text = start.Input[0].Text
			}
			result, err := turnStartReply(text)
			if err != nil {
				out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32000, "message": err.Error()}})
				return out
			}
			out, _ := json.Marshal(map[string]any{"id": id, "result": result})
			return out
		}
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
		f.Mu.Lock()
		f.Steers = append(f.Steers, append(json.RawMessage(nil), params...))
		steerErr := f.SteerErr
		f.Mu.Unlock()
		if steerErr != "" {
			out, _ := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": steerErr}})
			return out
		}
		result = map[string]any{"turnId": req.ExpectedTurnID}
	case "turn/interrupt":
		f.Mu.Lock()
		onInterrupt := f.OnTurnInterrupt
		f.Mu.Unlock()
		if onInterrupt != nil {
			onInterrupt()
		}
	case "skills/list":
		result = map[string]any{"data": []any{map[string]any{"name": "some-skill"}}}
	case "mcpServerStatus/list":
		var req struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(params, &req)
		f.Mu.Lock()
		f.McpCalls++
		f.McpDetails = append(f.McpDetails, req.Detail)
		f.Mu.Unlock()
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
