package claude

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeSpoolEnvelopeKeepsCaptureClockSeparate(t *testing.T) {
	state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, "shim-envelope-replay-exit")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || len(state.Turns) != 1 || items[0].Text != "Example" || items[0].CreatedAt != nil || len(batch.Diagnostics) != 0 {
		t.Fatal("spool native content or clock separation lost")
	}
	captured := false
	for _, stamp := range items[0].Times {
		if stamp.Class == model.TimeCapture && stamp.SourceField == "shim.ts" && stamp.Value != nil {
			captured = true
		}
	}
	if !captured || items[0].Observations[0].NativeEventID != "" || items[0].Observations[0].Source.Direction != "send" || !strings.Contains(items[0].Observations[0].Source.BlockPath, "line::json") {
		t.Fatal("capture metadata became native identity or lost provenance")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeCompleted {
			t.Fatal("wrapped result not interpreted")
		}
	}
}

func TestClaudeSpoolCheckpointDoesNotRepeatTerminal(t *testing.T) {
	raw := fixture(t, "shim-envelope-replay-exit")
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(state)
	var restored parsedView
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	state, _, err = parseFileView(restored, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(saved, after) {
		t.Fatal("capture replay mutated prior checkpoint")
	}
	for _, turn := range state.Turns {
		if len(turn.Completion) != 1 || turn.Outcome != model.OutcomeCompleted {
			t.Fatal("capture replay repeated or lost native terminal")
		}
	}
}

func captureRow(seq int, native string) []byte {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/line", "params": map[string]any{"shimSessionId": "shim-example", "seq": seq, "dir": "recv", "ts": "2026-01-01T00:00:00Z", "line": native}})
	return raw
}

func TestClaudeCaptureReplayAndConflict(t *testing.T) {
	rows := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"A"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ha"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ha"}}}`,
	}
	var state parsedView
	for i, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputFor(captureRow(i+1, row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	input := inputFor(captureRow(3, rows[2]))
	now := time.Unix(100, 0).UTC()
	input.ObservedAt = &now
	state, batch, err := parseWireView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "Ahaha" || len(items[0].Observations) != 4 || len(items[0].Relations) != 1 || items[0].Relations[0].Kind != model.RelationReplayOf || len(batch.Diagnostics) != 0 {
		t.Fatal("capture seq replay appended native delta or dropped a distinct equal chunk")
	}
	replay := items[0].Observations[3]
	if replay.NativeEventID != "" {
		t.Fatal("capture sequence became native event identity")
	}
	observed := false
	for _, stamp := range replay.Times {
		if stamp.Class == model.TimeObserved && stamp.Value != nil && stamp.Value.Equal(now) {
			observed = true
		}
		if stamp.Class == model.TimeNative && stamp.Value != nil {
			t.Fatal("capture time filled null native time")
		}
	}
	if !observed {
		t.Fatal("replay receipt time missing")
	}
	state, batch, err = parseWireView(state, inputFor(captureRow(3, strings.ReplaceAll(rows[2], "ha", "changed"))))
	if err != nil {
		t.Fatal(err)
	}
	if orderedItems(state)[0].Text != "Ahaha" || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "capture_event_conflict" {
		t.Fatal("conflicting capture sequence changed accepted content")
	}
}

func TestClaudeProcessExitDoesNotCompleteTurn(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputFor(captureRow(1, `{"type":"user","message":{"content":"Prompt"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	before := len(state.Items)
	for _, method := range []string{"session/replayDone", "session/exited"} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{"shimSessionId": "shim-example", "lastSeq": 1, "exitCode": 0}})
		state, _, err = parseWireView(state, inputFor(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	if before != 1 || len(state.Items) != before || len(state.Turns) != 1 {
		t.Fatal("transport lifecycle created native content")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown || len(turn.Completion) != 0 {
			t.Fatal("process exit completed provider turn")
		}
	}
}

func TestClaudeWireObservationIDsHaveSingleMeaning(t *testing.T) {
	var state parsedView
	seen := map[string]bool{}
	for _, row := range bytes.Split(bytes.TrimSpace(fixture(t, "paired-split-wire")), []byte{'\n'}) {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputFor(row))
		if err != nil {
			t.Fatal(err)
		}
		for _, obs := range batch.Observations {
			if seen[obs.ID] {
				t.Fatal("observation ID emitted twice with different scope or meaning")
			}
			seen[obs.ID] = true
		}
	}
}
