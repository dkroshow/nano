package codex

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A long open turn must not keep every item it has produced: the catalog
// re-saves the whole continuation on each append, so one worker's turn of
// about a thousand tool calls held 97 MB and every append took seconds. Rows
// follow a real rollout's shapes (task_started, custom_tool_call and its
// output) with invented ids and text. Boundary: the file checkpoint. It also
// checks that resuming from the checkpoint after every row emits what one
// pass does, so the smaller continuation changes nothing a consumer stores.
func TestCodexOpenTurnCheckpointDropsSettledResults(t *testing.T) {
	rows := []string{`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1","started_at":1793149318,"model_context_window":258400}}`}
	output := strings.Repeat(`command output line\n`, 200)
	for call := 0; call < 40; call++ {
		rows = append(rows,
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"custom_tool_call","id":"ctc_%d","status":"completed","call_id":"call_%d","name":"exec","input":"ls %d"}}`, call, call, call),
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"custom_tool_call_output","id":"ctco_%d","call_id":"call_%d","output":[{"type":"input_text","text":"%s"}]}}`, call, call, output))
	}
	raw := []byte(strings.Join(rows, "\n") + "\n")
	want, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	for id, item := range want.Continuation.Items {
		if item.Type == model.ItemToolResult {
			t.Fatalf("open turn checkpoint keeps settled result %s", id)
		}
	}
	view := parsedView{}
	for _, row := range rows {
		checkpoint, _ := json.Marshal(view)
		var restored parsedView
		if err := json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		if view, _, err = parseFileView(restored, inputFor([]byte(row+"\n"))); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(view.Items, want.Items) || !reflect.DeepEqual(view.Turns, want.Turns) {
		t.Fatal("resuming after every row emits different items or turns than one pass")
	}
}

// Codex can complete a native item again under its id, after a newer item of
// its type and whichever of its two copies came first. Found in review: an
// open turn that dropped such an item made the repeat a second item; a
// repeated earlier answer then became the turn's final answer, and history
// moved a command row's displayed creation time. Boundary: the file
// checkpoint, saved and restored after every record.
func TestCodexOpenTurnKeepsNativeItemsForRecompletion(t *testing.T) {
	command := string(fixture(t, "capture-item-completed-commandexecution"))
	for _, tc := range []struct {
		name  string
		rows  []string
		items int
		final string
	}{
		{"command", []string{command, strings.Replace(command, "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:01.000Z", 1)}, 1, ""},
		{"response-first user prompt", []string{userTwinResponse("response-1", "turn-1"), userTwinCompleted("completed-1", "turn-1"), userTwinCompleted("completed-1", "turn-1")}, 1, ""},
		{"response-first earlier answer", []string{taskStarted("t"), responseAgent("msg_1", "Earlier answer"), completedAgent("item-1", "t", "Earlier answer"),
			responseAgent("msg_2", "Latest answer"), completedAgent("item-2", "t", "Latest answer"), completedAgent("item-1", "t", "Earlier answer"),
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}` + "\n"}, 2, "Latest answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var view parsedView
			for _, row := range tc.rows {
				checkpoint, _ := json.Marshal(view)
				var restored parsedView
				if err := json.Unmarshal(checkpoint, &restored); err != nil {
					t.Fatal(err)
				}
				var err error
				if view, _, err = parseFileView(restored, inputFor([]byte(row))); err != nil {
					t.Fatal(err)
				}
			}
			if len(view.Items) != tc.items {
				t.Fatalf("a repeated native completion made %d items, want %d", len(view.Items), tc.items)
			}
			for _, turn := range view.Turns {
				if final := view.Items[turn.FinalItemID]; tc.final != "" && final.Text != tc.final {
					t.Fatalf("final answer %q, want %q", final.Text, tc.final)
				}
			}
		})
	}
}

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
