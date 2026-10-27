// Package wiretest provides fake provider endpoints shared by wire-package tests.
package wiretest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// Call is one request received by a fake provider endpoint.
type Call struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// Server is a newline-free JSON-RPC WebSocket fake.
type Server struct {
	testing.TB
	http  *httptest.Server
	mu    sync.Mutex
	conns map[*websocket.Conn]struct{}
	Calls chan Call
}

// NewServer starts a fake endpoint that acknowledges every request.
func NewServer(t testing.TB) *Server {
	t.Helper()
	server := &Server{TB: t, conns: make(map[*websocket.Conn]struct{}), Calls: make(chan Call, 32)}
	upgrader := websocket.Upgrader{}
	server.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		server.mu.Lock()
		server.conns[conn] = struct{}{}
		server.mu.Unlock()
		defer func() {
			server.mu.Lock()
			delete(server.conns, conn)
			server.mu.Unlock()
			_ = conn.Close()
		}()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var call Call
			if json.Unmarshal(raw, &call) != nil || len(call.ID) == 0 {
				continue
			}
			server.Calls <- call
			response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": map[string]any{"ok": true}})
			if err := conn.WriteMessage(websocket.TextMessage, response); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// URL returns the WebSocket address of the fake endpoint.
func (s *Server) URL() string { return "ws" + strings.TrimPrefix(s.http.URL, "http") }

// Notify sends one provider notification to every connected client.
func (s *Server) Notify(method string, params any) {
	s.Helper()
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		s.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.conns {
		if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			s.Fatal(err)
		}
	}
}

// Close stops the fake endpoint and closes its clients.
func (s *Server) Close() {
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
	s.mu.Unlock()
	if s.http != nil {
		s.http.Close()
	}
}
