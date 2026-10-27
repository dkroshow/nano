package grok

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The same bound as the Codex reader: a prompt that never completes keeps its
// assembly in the continuation, and measuring that continuation must follow the
// input rather than the retained state. An oversized open prompt is still
// diagnosed.
func TestGrokOpenPromptMeasuresCheckpointRarely(t *testing.T) {
	chunk := strings.Repeat("streamed output ", 256)
	frames := []string{`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Prompt"}},"_meta":{"promptId":"p0","eventId":"u0"}}}`}
	for event := 0; event < 1000; event++ {
		row, _ := json.Marshal(map[string]any{"method": "session/update", "params": map[string]any{
			"sessionId": "s",
			"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": chunk}},
			"_meta":     map[string]any{"promptId": "p0", "eventId": fmt.Sprintf("a%d", event)}}})
		frames = append(frames, string(row))
	}
	var state State
	diagnosed := 0
	for _, frame := range frames {
		var batch model.Batch
		var err error
		state, batch, err = ParseWire(state, inputBytes([]byte(frame)))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range batch.Diagnostics {
			if d.Code == "continuation_limit_exceeded" {
				diagnosed++
			}
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Turns) != 1 || len(raw) <= checkpointWarningBytes {
		t.Fatalf("open prompt did not retain an oversized continuation: turns=%d checkpoint=%d", len(state.Turns), len(raw))
	}
	if diagnosed == 0 {
		t.Fatal("oversized open continuation was never diagnosed")
	}
	if state.measurements > 64 {
		t.Fatalf("measured the whole continuation %d times over %d frames", state.measurements, len(frames))
	}
}
