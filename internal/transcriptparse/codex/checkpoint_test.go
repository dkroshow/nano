package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A turn that never completes keeps its whole assembly in the continuation, so
// encoding that state to measure it must not happen on every frame: one open
// 2.5-hour turn made each wire frame re-encode hundreds of megabytes and the
// session's event loop stopped serving anything else. The measurement cadence
// follows the input, not the retained state, and an open continuation past the
// ceiling is still diagnosed.
func TestCodexOpenTurnMeasuresCheckpointRarely(t *testing.T) {
	frames := []string{`{"method":"turn/started","params":{"threadId":"s","turn":{"id":"p0","status":"inProgress"}}}`}
	chunk := strings.Repeat("streamed output ", 128)
	for item := 0; item < 100; item++ {
		frames = append(frames, fmt.Sprintf(`{"method":"item/started","params":{"threadId":"s","turnId":"p0","item":{"id":"a%d","type":"agentMessage","text":""}}}`, item))
		for delta := 0; delta < 48; delta++ {
			row, _ := json.Marshal(map[string]any{"method": "item/agentMessage/delta",
				"params": map[string]any{"threadId": "s", "turnId": "p0", "itemId": fmt.Sprintf("a%d", item), "delta": chunk}})
			frames = append(frames, string(row))
		}
		frames = append(frames, fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"s","turnId":"p0","item":{"id":"a%d","type":"agentMessage","text":"done %d"}}}`, item, item))
	}
	if len(frames) != 5001 {
		t.Fatalf("frame count %d; the check wants a 5,000 frame open turn", len(frames)-1)
	}
	var state State
	diagnosed := 0
	for _, frame := range frames {
		var batch model.Batch
		var err error
		state, batch, err = ParseWire(state, inputFor([]byte(frame)))
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
		t.Fatalf("open turn did not retain an oversized continuation: turns=%d checkpoint=%d", len(state.Turns), len(raw))
	}
	if diagnosed == 0 {
		t.Fatal("oversized open continuation was never diagnosed")
	}
	// Total input is about 10 MiB, so the amortized rule allows a few dozen
	// measurements; per-frame measurement would be 5,001 of them.
	if state.measurements > 64 {
		t.Fatalf("measured the whole continuation %d times over %d frames", state.measurements, len(frames))
	}
}
