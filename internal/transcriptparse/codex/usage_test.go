package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexUsageRepeatAndDecreaseKeepEvidenceWithoutNegativeContributions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     int64
		window    int64
		decreases int
	}{
		{"capture-repeated-usage", 3410, 258400, 0},
		{"edge-usage-repeat-and-reset", 10, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state parsedView
			codes := map[string]int{}
			for _, row := range bytes.Split(bytes.TrimSpace(fixture(t, tc.name)), []byte{'\n'}) {
				raw, _ := json.Marshal(state)
				var restored parsedView
				if err := json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				next, batch, err := parseFileView(restored, inputFor(append(append([]byte(nil), row...), '\n')))
				if err != nil {
					t.Fatal(err)
				}
				after, _ := json.Marshal(restored)
				if !bytes.Equal(raw, after) {
					t.Fatal("usage changed prior checkpoint")
				}
				state = next
				for _, d := range batch.Diagnostics {
					codes[d.Code]++
				}
			}
			if codes["repeated_usage_snapshot"] != 1 || codes["usage_counter_decrease"] != tc.decreases {
				t.Errorf("usage evidence diagnostics: %v", codes)
			}
			for _, turn := range state.Turns {
				if turn.Usage == nil || turn.Usage.Input == nil || *turn.Usage.Input != tc.input || len(turn.UsageRefs) != 1 {
					t.Error("repeated/cumulative usage changed selected response total")
				}
				if tc.window > 0 && (turn.Usage.ContextWindow == nil || *turn.Usage.ContextWindow != tc.window) {
					t.Error("context capacity outside token counters was lost")
				}
				if len(turn.UsageObservations) < 4 || len(turn.UsageObservations[0].Values.Native) == 0 {
					t.Error("usage revisions or native counter fields disappeared")
				}
			}
		})
	}
}

func TestCodexUsageDecreaseDoesNotCrossNativeScope(t *testing.T) {
	var state parsedView
	for _, params := range []string{
		`{"threadId":"a","turnId":"turn","tokenUsage":{"last":{"inputTokens":10},"total":{"inputTokens":100},"modelContextWindow":500}}`,
		`{"threadId":"b","turnId":"turn","tokenUsage":{"last":{"inputTokens":1},"total":{"inputTokens":1},"modelContextWindow":200}}`,
	} {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputFor([]byte(`{"method":"thread/tokenUsage/updated","params":`+params+`}`)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("separate scopes: %v %+v", err, batch.Diagnostics)
		}
	}
	for _, turn := range state.Turns {
		want := int64(500)
		if *turn.Scope.NativeThreadID == "b" {
			want = 200
		}
		if turn.Usage == nil || turn.Usage.ContextWindow == nil || *turn.Usage.ContextWindow != want {
			t.Error("wire context capacity lost")
		}
	}
}

// A closed file's last read emits its turns with their usage samples. Found
// in review of moving samples out of the turn: closing the checkpoint dropped
// the samples before the read's turn changes received them. Boundary: one
// closed read through ParseFile.
func TestCodexClosedReadKeepsUsageOnEmittedTurns(t *testing.T) {
	raw := []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11},"last_token_usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11},"model_context_window":258400}}}` + "\n")
	input := inputFor(raw)
	input.Closed = true
	view, _, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range view.Turns {
		if len(turn.UsageObservations) == 0 || turn.Usage == nil {
			t.Fatalf("a closed read emitted its turn without usage samples: %+v", turn.UsageObservations)
		}
	}
}
