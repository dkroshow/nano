package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeGrowingSourceEvictsSettledTurns(t *testing.T) {
	var rows []string
	for i := 0; i < 30; i++ {
		// Claude marks each turn's rows with its promptId; the reader keeps
		// it per retained turn only.
		rows = append(rows, fmt.Sprintf(`{"type":"user","uuid":"u%d","promptId":"p%d","message":{"content":"Prompt %d"}}`, i, i, i),
			fmt.Sprintf(`{"type":"assistant","uuid":"a%d","message":{"id":"m%d","content":"Answer %d","stop_reason":"end_turn"}}`, i, i, i))
	}
	rows = append(rows, `{"type":"user","uuid":"last","message":{"content":"Continue"}}`)
	whole, _, err := parseFileView(parsedView{}, inputFor([]byte(strings.Join(rows, "\n")+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []bool{false, true} {
		var view parsedView
		for _, row := range rows {
			parse, suffix := parseFileView, "\n"
			if wire {
				parse, suffix = parseWireView, ""
			}
			view, _, err = parse(view, inputFor([]byte(row+suffix)))
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Continuation.Turns) != 1 {
				t.Fatal("settled turn payload remained in continuation")
			}
		}
		state := view.Continuation
		if len(view.Items) != len(rows) || len(view.Turns) != 31 || len(state.Items) != 1 || len(state.Messages) != 1 || len(state.Rows) != 1 || len(state.RowEvents) != 1 || len(state.Calls) != 0 || len(state.TurnPrompts) > 1 || state.Framing.Closed {
			t.Fatal("eviction removed emitted evidence or retained closed assembly")
		}
		if !wire {
			a, _ := json.Marshal(whole)
			b, _ := json.Marshal(view)
			if !bytes.Equal(a, b) {
				t.Fatal("chunk boundary changed emission or continuation")
			}
		}
	}
}

func TestClaudeCheckpointRetainsUnfinishedEvidence(t *testing.T) {
	rows := `{"type":"user","message":{"content":"Pending"}}
{"type":"assistant","message":{"id":"tool","content":[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"pending"}}]}}
{"type":"user","message":{"content":"Another prompt"}}
`
	view, _, err := parseFileView(parsedView{}, inputFor([]byte(rows)))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Continuation.Turns) != 2 || len(view.Continuation.Items) != 3 || len(view.Continuation.Calls) != 1 {
		t.Fatal("a new prompt discarded unfinished evidence")
	}
}

func TestClaudeLargeOpenCheckpointReportsWithoutDroppingEvidence(t *testing.T) {
	content := strings.Repeat("unfinished ", 200000)
	row, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"id": "large", "content": content}})
	state, batch, err := ParseWire(State{}, inputFor(row))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if len(raw) <= checkpointWarningBytes {
		t.Fatal("test must exceed the continuation ceiling")
	}
	found := false
	for _, d := range batch.Diagnostics {
		found = found || d.Code == "continuation_limit_exceeded"
	}
	if !found {
		t.Fatal("oversized unfinished state had no diagnostic")
	}
	for _, item := range state.Items {
		if item.Text != content || item.Outcome == model.OutcomeTruncated {
			t.Fatal("ceiling silently truncated unfinished evidence")
		}
	}
}

// Streaming deltas skip the whole-state size check; the block's stop frame
// measures the same growth, so an oversized open continuation is still
// diagnosed, one frame later at most per block.
func TestClaudeStreamDeltasDeferCheckpointSizeToBlockStop(t *testing.T) {
	frames := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"large","content":[]}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	}
	chunk := strings.Repeat("unfinished ", 20000)
	for i := 0; i < 25; i++ {
		row, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": chunk}}})
		frames = append(frames, string(row))
	}
	frames = append(frames, `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
	var state State
	for index, frame := range frames {
		var batch model.Batch
		var err error
		state, batch, err = ParseWire(state, inputFor([]byte(frame)))
		if err != nil {
			t.Fatal(err)
		}
		diagnosed := false
		for _, d := range batch.Diagnostics {
			diagnosed = diagnosed || d.Code == "continuation_limit_exceeded"
		}
		last := index == len(frames)-1
		if diagnosed != last {
			t.Fatalf("frame %d diagnosed=%v; want the size check only on the block stop", index, diagnosed)
		}
	}
	if raw, _ := json.Marshal(state); len(raw) <= checkpointWarningBytes {
		t.Fatal("test must exceed the continuation ceiling")
	}
}

// The same bound as the Codex reader: a turn that never completes must not make
// every frame re-encode the whole retained continuation. Deltas already skip the
// check; the block stops that do measure follow the input, not the state.
func TestClaudeOpenTurnMeasuresCheckpointRarely(t *testing.T) {
	chunk := strings.Repeat("streamed output ", 128)
	var frames []string
	for block := 0; block < 60; block++ {
		frames = append(frames,
			fmt.Sprintf(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m%d","content":[]}}}`, block),
			`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`)
		for delta := 0; delta < 48; delta++ {
			row, _ := json.Marshal(map[string]any{"type": "stream_event", "event": map[string]any{
				"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": chunk}}})
			frames = append(frames, string(row))
		}
		frames = append(frames, `{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)
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
	if len(raw) <= checkpointWarningBytes {
		t.Fatalf("open turn did not retain an oversized continuation: checkpoint=%d", len(raw))
	}
	if diagnosed == 0 {
		t.Fatal("oversized open continuation was never diagnosed")
	}
	if state.measurements > 64 {
		t.Fatalf("measured the whole continuation %d times over %d frames", state.measurements, len(frames))
	}
}

func TestClaudePendingControlSurvivesTurnRetirement(t *testing.T) {
	rows := `{"type":"assistant","uuid":"call","message":{"id":"m","content":[{"type":"tool_use","id":"question","name":"AskUserQuestion","input":{}}],"stop_reason":"end_turn"}}
{"type":"control_request","request_id":"permission","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"question","input":{}}}
{"type":"user","message":{"content":"Next prompt"}}
`
	view, _, err := parseFileView(parsedView{}, inputFor([]byte(rows)))
	if err != nil {
		t.Fatal(err)
	}
	invocation := orderedItems(view)[0]
	if _, exists := view.Continuation.Items[invocation.ID]; !exists {
		t.Fatal("pending control lost its earlier invocation")
	}
	view, batch, err := parseFileView(view, inputFor([]byte(`{"type":"control_response","response":{"subtype":"success","request_id":"permission","response":{"behavior":"allow"}}}`+"\n")))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("late decision: %v %v", err, batch.Diagnostics)
	}
	if view.Items[invocation.ID].Outcome != model.OutcomeCompleted {
		t.Fatal("pending decision did not revise its original invocation")
	}
	if _, exists := view.Continuation.Items[invocation.ID]; exists || len(view.Continuation.Controls) != 0 {
		t.Fatal("settled control retained retired turn payload")
	}
}
