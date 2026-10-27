package grok_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dkroshow/nano/internal/wire/grok"
	"github.com/dkroshow/nano/internal/wire/wiretest"
)

func TestDecodeFrameFromSharedFakeServer(t *testing.T) {
	server := wiretest.NewServer(t)
	conn, _, err := websocket.DefaultDialer.Dial(server.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	server.Notify("session/update", map[string]any{
		"sessionId": "session-1",
		"update":    map[string]any{"sessionUpdate": "agent_message_chunk"},
	})
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	frame, relevant, err := grok.DecodeFrame(json.RawMessage(raw), grok.MaxFrameBytes)
	if err != nil || !relevant || frame.Params.SessionID != "session-1" {
		t.Fatalf("DecodeFrame() = (%+v, %v, %v)", frame, relevant, err)
	}
}
