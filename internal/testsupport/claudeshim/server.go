package claudeshim

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/claude"
	"github.com/gorilla/websocket"
)

// Server is a scripted claude-shim daemon: one websocket endpoint speaking
// the frozen JSON-RPC contract (docs/decisions/claude-shim-daemon.md). Each
// connection runs the test's script, which reads calls and pushes replies and
// notifications exactly as the real daemon would; every call it handles is
// recorded for assertions.
type Server struct {
	Srv *httptest.Server

	Mu    sync.Mutex
	Calls []Call
}

type Call struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

func NewServer(t *testing.T, script func(c *Conn)) *Server {
	t.Helper()
	f := &Server{}
	upgrader := websocket.Upgrader{}
	f.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		script(&Conn{T: t, Shim: f, Conn: conn})
	}))
	t.Cleanup(f.Srv.Close)
	return f
}

func (f *Server) WSURL() string { return "ws" + strings.TrimPrefix(f.Srv.URL, "http") }

func (f *Server) CallsOf(method string) []Call {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []Call
	for _, c := range f.Calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// inputLines returns the raw stdin lines the shim was asked to forward.
func (f *Server) InputLines() []string {
	var out []string
	for _, call := range f.CallsOf(protocol.MethodInput) {
		var p struct {
			Line string `json:"line"`
		}
		if json.Unmarshal(call.Params, &p) == nil {
			out = append(out, p.Line)
		}
	}
	return out
}

type Conn struct {
	T    *testing.T
	Shim *Server
	Conn *websocket.Conn

	WriteMu sync.Mutex
}

// next reads the next request, recording it; ok is false once the client hangs
// up.
func (c *Conn) Next() (Call, bool) {
	for {
		_, frame, err := c.Conn.ReadMessage()
		if err != nil {
			return Call{}, false
		}
		var call Call
		if json.Unmarshal(frame, &call) != nil || call.ID == nil {
			continue
		}
		c.Shim.Mu.Lock()
		c.Shim.Calls = append(c.Shim.Calls, call)
		c.Shim.Mu.Unlock()
		return call, true
	}
}

func (c *Conn) Reply(call Call, result any) {
	if result == nil {
		result = map[string]any{}
	}
	c.Write(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

func (c *Conn) ReplyError(call Call, code int, message string) {
	c.Write(map[string]any{
		"jsonrpc": "2.0", "id": call.ID,
		"error": map[string]any{"code": code, "message": message},
	})
}

func (c *Conn) Notify(method string, params any) {
	c.Write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Conn) Line(seq int64, dir, line string) {
	c.LineForTurn(seq, dir, "", line)
}

func (c *Conn) LineForTurn(seq int64, dir, turnID, line string) {
	c.Notify(protocol.MethodLine, map[string]any{
		"shimSessionId": "shim-1", "seq": seq, "dir": dir, "turnId": turnID, "line": line,
	})
}

// answerRest replies {} to every remaining call until the client disconnects,
// so input/close traffic is recorded without the script scripting it.
func (c *Conn) AnswerRest() {
	turn := 0
	for {
		call, ok := c.Next()
		if !ok {
			return
		}
		if call.Method == protocol.MethodInput {
			turn++
			c.Reply(call, map[string]any{"turnId": "claude:shim-1:" + strconv.Itoa(turn)})
			continue
		}
		c.Reply(call, nil)
	}
}

func (c *Conn) Write(msg any) {
	frame, err := json.Marshal(msg)
	if err != nil {
		c.T.Errorf("marshal shim frame: %v", err)
		return
	}
	c.WriteMu.Lock()
	defer c.WriteMu.Unlock()
	if err := c.Conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		return // the client hung up; the script is done either way
	}
}

// writeWithin is the backpressure-sensitive variant used by the large replay
// regression. Ordinary fake-shim scripts intentionally ignore a peer that
// hangs up; this helper instead reports whether Nano kept draining the socket.
func (c *Conn) WriteWithin(msg any, timeout time.Duration) error {
	frame, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.WriteMu.Lock()
	defer c.WriteMu.Unlock()
	if err := c.Conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	err = c.Conn.WriteMessage(websocket.TextMessage, frame)
	_ = c.Conn.SetWriteDeadline(time.Time{})
	return err
}
