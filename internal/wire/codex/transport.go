package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
)

// stdioTransport speaks newline-delimited JSON over a spawned child's stdio.
type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
}

var errNilCommand = errors.New("protocol: nil command")

var stdioWriteTimeout = 10 * time.Second

type deadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

func (t *stdioTransport) start() error {
	if t.cmd == nil {
		return errNilCommand
	}
	stdin, err := t.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	t.stdin = stdin
	t.scanner = bufio.NewScanner(stdout)
	t.scanner.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	return t.cmd.Start()
}

func (t *stdioTransport) startContext(context.Context) error { return t.start() }

func (t *stdioTransport) writeMessage(line []byte) error {
	if writer, ok := t.stdin.(deadlineWriter); ok {
		if err := writer.SetWriteDeadline(time.Now().Add(stdioWriteTimeout)); err == nil {
			defer writer.SetWriteDeadline(time.Time{})
		}
	}
	_, err := t.stdin.Write(append(line, '\n'))
	return err
}

func (t *stdioTransport) readMessage() ([]byte, error) {
	if !t.scanner.Scan() {
		if err := t.scanner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	// Copy: scanner.Bytes() aliases a buffer reused by the next Scan, and
	// msg.Params/msg.Result live on as sub-slices of this line (delivered
	// to pending Call()s and handlers that outlive this iteration).
	return append([]byte(nil), t.scanner.Bytes()...), nil
}

func (t *stdioTransport) close() error {
	if t.stdin != nil {
		t.stdin.Close()
	}
	if t.cmd == nil {
		return nil
	}
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	if t.cmd.Process == nil {
		return nil
	}
	return t.cmd.Wait()
}

// wsTransport dials a running app-server daemon; each JSON-RPC message is
// one websocket text frame (no newline framing). The dial sends no Origin
// header: the daemon 403s handshakes that carry one (browser-origin
// protection), so browser-shaped clients are rejected by design.
type wsTransport struct {
	url  string
	conn *websocket.Conn
}

const wsWriteTimeout = 10 * time.Second

func (t *wsTransport) start() error {
	return t.startContext(context.Background())
}

func (t *wsTransport) startContext(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, t.url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", t.url, err)
	}
	t.conn = conn
	return nil
}

func (t *wsTransport) writeMessage(line []byte) error {
	if err := t.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
		return err
	}
	defer t.conn.SetWriteDeadline(time.Time{})
	return t.conn.WriteMessage(websocket.TextMessage, line)
}

func (t *wsTransport) readMessage() ([]byte, error) {
	_, frame, err := t.conn.ReadMessage()
	return frame, err
}

func (t *wsTransport) close() error {
	if t.conn == nil {
		return nil
	}
	return t.conn.Close()
}
