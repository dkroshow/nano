package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// RPCError is the JSON-RPC error returned by the Claude shim.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type message struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *RPCError        `json:"error,omitempty"`
}

func (m *message) isResponse() bool     { return m.ID != nil && m.Method == "" }
func (m *message) isNotification() bool { return m.ID == nil && m.Method != "" }

// Client is one JSON-RPC 2.0 WebSocket connection to the supervised shim.
type Client struct {
	conn         *websocket.Conn
	onNotify     func(string, json.RawMessage)
	onDisconnect func(error)
	writeMu      sync.Mutex
	mu           sync.Mutex
	nextID       int64
	pending      map[int64]chan *message
	done         chan struct{}
	readErr      error
}

// Dial opens one shim connection and starts its reader loop.
func Dial(url string, timeout time.Duration, onNotify func(string, json.RawMessage), onDisconnect func(error)) (*Client, error) {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = timeout
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}
	client := &Client{
		conn: conn, onNotify: onNotify, onDisconnect: onDisconnect,
		pending: map[int64]chan *message{}, done: make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

// Call sends one request and waits for its response or context cancellation.
func (c *Client) Call(ctx context.Context, method string, params, result any, fallbackTimeout time.Duration) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(fallbackTimeout)
	}
	err = c.conn.SetWriteDeadline(deadline)
	if err == nil {
		err = c.conn.WriteMessage(websocket.TextMessage, frame)
	}
	_ = c.conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("connection closed waiting for %s: %v", method, c.readErr)
	case response := <-ch:
		if response.Error != nil {
			return response.Error
		}
		if result != nil {
			return json.Unmarshal(response.Result, result)
		}
		return nil
	}
}

// Close drops the shim WebSocket.
func (c *Client) Close() error { return c.conn.Close() }

// Done closes when the reader loop exits.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) readLoop() {
	for {
		_, frame, err := c.conn.ReadMessage()
		if err != nil {
			c.readErr = err
			break
		}
		var msg message
		if json.Unmarshal(frame, &msg) != nil {
			continue
		}
		switch {
		case msg.isResponse():
			c.deliverResponse(&msg)
		case msg.isNotification():
			c.onNotify(msg.Method, msg.Params)
		}
	}
	close(c.done)
	c.onDisconnect(c.readErr)
}

func (c *Client) deliverResponse(msg *message) {
	var id int64
	if json.Unmarshal(*msg.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}
