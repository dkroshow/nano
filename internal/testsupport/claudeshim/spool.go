// Package claudeshim provides synthetic Claude wire fixtures for tests.
package claudeshim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Frame is one anonymized spool line in the shim's own format.
type Frame struct {
	Seq    int64  `json:"seq"`
	TS     string `json:"ts"`
	Dir    string `json:"dir"`
	Line   string `json:"line"`
	TurnID string `json:"turnId,omitempty"`
}

func RecvFrame(seq int64, line string) Frame {
	return Frame{Seq: seq, Dir: "recv", Line: line}
}

func SendFrame(seq int64, line string) Frame {
	return Frame{Seq: seq, Dir: "send", Line: line}
}

func (f Frame) WithTurn(id string) Frame { f.TurnID = id; return f }

func SpoolBytes(t *testing.T, frames ...Frame) []byte {
	t.Helper()
	var out []byte
	for _, frame := range frames {
		if frame.TS == "" {
			frame.TS = "2026-09-18T12:00:00Z"
		}
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, raw...), '\n')
	}
	return out
}

func WriteSpool(t *testing.T, dir, key string, frames ...Frame) string {
	t.Helper()
	path := filepath.Join(dir, key+".jsonl")
	if err := os.WriteFile(path, SpoolBytes(t, frames...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	MessageStart = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_%s"}}}`
	MessageStop  = `{"type":"stream_event","event":{"type":"message_stop"}}`
	UserPrompt   = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"prompt"}]}}`
)

func StartMessage(id string) string { return strings.Replace(MessageStart, "%s", id, 1) }

func TextDelta(index int, text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{
		"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}}})
	return string(raw)
}

const InitLine = `{"type":"system","subtype":"init","session_id":"native-1","model":"sonnet"}`
const ReplayResultLine = `{"type":"result","subtype":"success","session_id":"native-1"}`
