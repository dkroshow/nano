// Package grokserver supplies a scripted ACP process for integration tests.
package grokserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Call is one request the ACP client sent over its standard input.
type Call struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage
}

// Conn is the scripted peer of a single Grok client process.
type Conn struct {
	t         *testing.T
	socket    net.Conn
	reader    *bufio.Scanner
	writeMu   sync.Mutex
	responses chan Call
}

// Server owns the disposable executable and its ACP connections.
type Server struct {
	bin         string
	listener    net.Listener
	mu          sync.Mutex
	connections []net.Conn
}

// NewServer runs script for each process connection. The executable only bridges
// stdin/stdout to the test, so the script controls every provider response.
func NewServer(t *testing.T, script func(*Conn)) *Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{listener: listener, bin: filepath.Join(t.TempDir(), "fake-grok")}
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	bridge := fmt.Sprintf(`#!/usr/bin/env python3
import socket, sys, threading
peer = socket.create_connection((%q, int(%q)))
def forward_input():
    try:
        for line in sys.stdin.buffer:
            peer.sendall(line)
        peer.shutdown(socket.SHUT_WR)
    except OSError:
        pass
threading.Thread(target=forward_input, daemon=True).start()
try:
    while True:
        data = peer.recv(65536)
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
except OSError:
    pass
finally:
    peer.close()
`, host, port)
	if err := os.WriteFile(server.bin, []byte(bridge), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		server.mu.Lock()
		defer server.mu.Unlock()
		for _, conn := range server.connections {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			socket, err := listener.Accept()
			if err != nil {
				return
			}
			server.mu.Lock()
			server.connections = append(server.connections, socket)
			server.mu.Unlock()
			go func() {
				defer socket.Close()
				reader := bufio.NewScanner(socket)
				reader.Buffer(make([]byte, 4096), 16<<20)
				script(&Conn{t: t, socket: socket, reader: reader, responses: make(chan Call, 32)})
			}()
		}
	}()
	return server
}

// Bin returns the executable consumed by ProviderRequest.GrokBin.
func (s *Server) Bin() string { return s.bin }

// Next reads the next client request and retains replies to scripted server requests.
func (c *Conn) Next() (Call, bool) {
	for c.reader.Scan() {
		var call Call
		if json.Unmarshal(c.reader.Bytes(), &call) == nil && len(call.ID) > 0 {
			if call.Method != "" {
				return call, true
			}
			c.responses <- call
		}
	}
	return Call{}, false
}

// Send writes one complete ACP message, including an arbitrary server request.
func (c *Conn) Send(message any) {
	raw, err := json.Marshal(message)
	if err != nil {
		c.t.Errorf("encode ACP fixture: %v", err)
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.socket.Write(append(raw, '\n'))
}

// Reply answers a client request with the supplied result.
func (c *Conn) Reply(call Call, result any) {
	c.Send(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

// Notify sends provider evidence independently of client requests.
func (c *Conn) Notify(method string, params any) {
	c.Send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Response waits for the client's reply to a scripted server request.
func (c *Conn) Response() Call {
	select {
	case reply := <-c.responses:
		return reply
	case <-time.After(5 * time.Second):
		c.t.Fatal("ACP client did not answer server request")
		return Call{}
	}
}
