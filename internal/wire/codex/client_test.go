package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fakeTransport is a two-channel stand-in for a websocket or a child's stdio.
type fakeTransport struct {
	in          chan []byte
	out         chan []byte
	duringWrite []byte
}

func TestCallWithWriteAckPrecedesConcurrentDispatch(t *testing.T) {
	tr := newFakeTransport()
	tr.duringWrite = []byte(`{"method":"test/notification","params":{}}`)
	client := newClient(tr)
	acknowledged := make(chan struct{})
	notified := make(chan bool, 1)
	client.OnNotification = func(string, json.RawMessage) {
		select {
		case <-acknowledged:
			notified <- true
		default:
			notified <- false
		}
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	callDone := make(chan error, 1)
	go func() {
		callDone <- client.CallWithWriteAck(context.Background(), "test/call", nil, nil, func() {
			close(acknowledged)
		})
	}()
	_ = tr.nextWrite(t)
	if ok := <-notified; !ok {
		t.Fatal("notification dispatched before the successful-write callback")
	}
	tr.deliver(`{"id":1,"result":{}}`)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
}

func TestOutboundJSONRPCVersionIsOptIn(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "codex default omits version", want: false},
		{name: "ACP includes version", version: "2.0", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := newFakeTransport()
			client := newClient(tr)
			client.JSONRPCVersion = tt.version
			client.OnServerRequest = func(string, string, json.RawMessage) (any, *RPCError) {
				return map[string]any{"ok": true}, nil
			}
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			callDone := make(chan error, 1)
			go func() {
				callDone <- client.Call(context.Background(), "test/call", map[string]any{"x": 1}, nil)
			}()
			assertVersionField(t, tr.nextWrite(t), tt.want)
			tr.deliver(`{"jsonrpc":"2.0","id":1,"result":{}}`)
			if err := <-callDone; err != nil {
				t.Fatal(err)
			}

			if err := client.Notify("test/notify", map[string]any{"x": 2}); err != nil {
				t.Fatal(err)
			}
			assertVersionField(t, tr.nextWrite(t), tt.want)

			tr.deliver(`{"jsonrpc":"2.0","id":"server-1","method":"test/request","params":{}}`)
			assertVersionField(t, tr.nextWrite(t), tt.want)
		})
	}
}

func assertVersionField(t *testing.T, line string, want bool) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatalf("decode outbound envelope %q: %v", line, err)
	}
	_, got := envelope["jsonrpc"]
	if got != want {
		t.Fatalf("jsonrpc present = %v, want %v: %s", got, want, line)
	}
	if want && string(envelope["jsonrpc"]) != `"2.0"` {
		t.Fatalf("jsonrpc = %s, want 2.0", envelope["jsonrpc"])
	}
}

func TestClientCommandConstructors(t *testing.T) {
	codex := NewClient("--listen", "ws://127.0.0.1:0")
	codexTransport, ok := codex.tr.(*stdioTransport)
	if !ok || codexTransport.cmd == nil {
		t.Fatalf("NewClient transport = %#v", codex.tr)
	}
	if got := codexTransport.cmd.Args; len(got) != 4 || got[0] != "codex" || got[1] != "app-server" {
		t.Fatalf("codex argv = %v", got)
	}

	cmd := exec.Command("grok", "agent", "stdio")
	grok := NewClientCmd(cmd)
	grokTransport, ok := grok.tr.(*stdioTransport)
	if !ok || grokTransport.cmd != cmd {
		t.Fatalf("NewClientCmd did not retain command: %#v", grok.tr)
	}

	if err := NewClientCmd(nil).Start(); !errors.Is(err, errNilCommand) {
		t.Fatalf("nil command start = %v, want errNilCommand", err)
	}
}

type deadlineTestWriter struct {
	deadlineErr error
	deadlines   []time.Time
	writes      [][]byte
}

func (w *deadlineTestWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return w.deadlineErr
}

func (w *deadlineTestWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (*deadlineTestWriter) Close() error { return nil }

type plainTestWriter struct{ writes [][]byte }

func (w *plainTestWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (*plainTestWriter) Close() error { return nil }

func TestStdioWriteDeadlineAndFallback(t *testing.T) {
	for _, tt := range []struct {
		name            string
		deadlineErr     error
		wantDeadlineOps int
	}{
		{name: "deadline supported", wantDeadlineOps: 2},
		{name: "deadline unsupported still writes", deadlineErr: errors.ErrUnsupported, wantDeadlineOps: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writer := &deadlineTestWriter{deadlineErr: tt.deadlineErr}
			transport := &stdioTransport{stdin: writer}
			if err := transport.writeMessage([]byte(`{"id":1}`)); err != nil {
				t.Fatal(err)
			}
			if len(writer.deadlines) != tt.wantDeadlineOps {
				t.Fatalf("deadline calls = %d, want %d", len(writer.deadlines), tt.wantDeadlineOps)
			}
			if len(writer.writes) != 1 || string(writer.writes[0]) != "{\"id\":1}\n" {
				t.Fatalf("writes = %q", writer.writes)
			}
		})
	}

	plain := &plainTestWriter{}
	if err := (&stdioTransport{stdin: plain}).writeMessage([]byte(`{"id":2}`)); err != nil {
		t.Fatal(err)
	}
	if len(plain.writes) != 1 || string(plain.writes[0]) != "{\"id\":2}\n" {
		t.Fatalf("plain fallback writes = %q", plain.writes)
	}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{in: make(chan []byte, 16), out: make(chan []byte, 16)}
}

func (t *fakeTransport) start() error { return nil }
func (t *fakeTransport) writeMessage(l []byte) error {
	if len(t.duringWrite) > 0 {
		t.in <- append([]byte(nil), t.duringWrite...)
		t.duringWrite = nil
	}
	t.out <- append([]byte(nil), l...)
	return nil
}
func (t *fakeTransport) readMessage() ([]byte, error) {
	line, ok := <-t.in
	if !ok {
		return nil, errors.New("closed")
	}
	return line, nil
}
func (t *fakeTransport) close() error { close(t.in); return nil }

func (t *fakeTransport) deliver(line string) { t.in <- []byte(line) }

func (t *fakeTransport) nextWrite(tb testing.TB) string {
	tb.Helper()
	select {
	case line := <-t.out:
		return string(line)
	case <-time.After(2 * time.Second):
		tb.Fatal("expected a write, got none")
		return ""
	}
}

func (t *fakeTransport) expectNoWrite(tb testing.TB) {
	tb.Helper()
	select {
	case line := <-t.out:
		tb.Fatalf("unexpected write: %s", line)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDeferredServerRequest is the contract the interactive-prompt slice needs:
// OnServerRequest runs on the READ LOOP, so a handler that waits for a human
// would stall every session sharing this connection. Deferring frees the loop
// immediately while keeping the request answerable.
func TestDeferredServerRequest(t *testing.T) {
	tr := newFakeTransport()
	client := newClient(tr)
	var logged []string
	client.WireLog = func(dir Direction, line []byte) { logged = append(logged, string(dir)+" "+string(line)) }
	client.OnServerRequest = func(requestID, method string, params json.RawMessage) (any, *RPCError) {
		if method == "item/commandExecution/requestApproval" {
			return Deferred, nil
		}
		return map[string]any{"token": "t"}, nil
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	tr.deliver(`{"id":"req-1","method":"item/commandExecution/requestApproval","params":{"threadId":"t1"}}`)
	// Nothing goes out: the answer belongs to the operator, not the read loop.
	tr.expectNoWrite(t)

	// …but the id is kept. Dropping it is the failure mode that would strand a
	// request forever, with the turn parked on the far side.
	ids := client.DeferredRequestIDs()
	if len(ids) != 1 || ids[0] != `"req-1"` {
		t.Fatalf("deferred ids = %v", ids)
	}

	// The loop is still live: a second request answers normally while the first
	// is parked.
	tr.deliver(`{"id":"req-2","method":"attestation/generate","params":{}}`)
	if got := tr.nextWrite(t); !strings.Contains(got, `"id":"req-2"`) {
		t.Fatalf("second request response = %s", got)
	}

	if err := client.RespondToServerRequest(`"req-1"`, map[string]any{"decision": "accept"}, nil); err != nil {
		t.Fatal(err)
	}
	answer := tr.nextWrite(t)
	if !strings.Contains(answer, `"id":"req-1"`) || !strings.Contains(answer, `"decision":"accept"`) {
		t.Fatalf("deferred answer = %s", answer)
	}
	// A multiplexed daemon routes responses to sessions off the WireLog, so a
	// deferred answer that bypassed it would vanish from the owning Event Feed.
	if len(logged) == 0 || !strings.Contains(logged[len(logged)-1], `send {"id":"req-1"`) {
		t.Fatalf("deferred answer missed the wire log: %v", logged)
	}

	// Idempotent: the second answer is refused rather than written.
	if err := client.RespondToServerRequest(`"req-1"`, map[string]any{"decision": "decline"}, nil); !errors.Is(err, ErrRequestNotDeferred) {
		t.Fatalf("second answer = %v, want ErrRequestNotDeferred", err)
	}
	tr.expectNoWrite(t)
	if ids := client.DeferredRequestIDs(); len(ids) != 0 {
		t.Fatalf("answered id still registered: %v", ids)
	}

	// An id nobody deferred is refused the same way.
	if err := client.RespondToServerRequest(`"never-seen"`, nil, nil); !errors.Is(err, ErrRequestNotDeferred) {
		t.Fatalf("unknown id = %v", err)
	}
}

// TestRespondToDeadClient: a request that arrived on a connection that has
// since died cannot be answered, and must say so instead of writing into a
// closed transport.
func TestRespondToDeadClient(t *testing.T) {
	tr := newFakeTransport()
	client := newClient(tr)
	client.OnServerRequest = func(requestID, method string, params json.RawMessage) (any, *RPCError) {
		return Deferred, nil
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	tr.deliver(`{"id":"req-1","method":"item/tool/requestUserInput","params":{}}`)
	tr.expectNoWrite(t)

	client.Close()
	<-client.Done()

	if err := client.RespondToServerRequest(`"req-1"`, map[string]any{"answers": map[string]any{}}, nil); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("answer on a dead client = %v, want ErrClientClosed", err)
	}
}
