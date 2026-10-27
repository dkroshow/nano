package codex

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexActionAfterTerminalCannotReopenEarlierExecution(t *testing.T) {
	rows := []string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}`,
		`{"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"c","command":["first"]}}`,
		`{"type":"event_msg","payload":{"type":"exec_command_end","call_id":"c","exit_code":1}}`,
		`{"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"c","command":["second"]}}`,
		`{"type":"event_msg","payload":{"type":"exec_command_end","call_id":"c","exit_code":0}}`,
	}
	var state parsedView
	var codes []string
	for _, row := range rows {
		checkpoint, _ := json.Marshal(state)
		var restored parsedView
		if err := json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		next, batch, err := parseFileView(restored, inputFor([]byte(row+"\n")))
		if err != nil {
			t.Fatal(err)
		}
		state = next
		for _, diagnostic := range batch.Diagnostics {
			codes = append(codes, diagnostic.Code)
		}
	}
	items := orderedItems(state)
	if len(items) != 3 || items[0].Outcome != model.OutcomeFailed || items[0].Text != "first" || len(items[0].Observations) != 2 || items[1].Outcome != model.OutcomeRunning || items[2].Outcome != model.OutcomeCompleted {
		t.Fatal("reused native call ID rewrote a terminal execution or guessed a later binding")
	}
	if len(codes) != 2 || codes[0] != "ambiguous_action_call_id" || codes[1] != "ambiguous_action_call_id" {
		t.Fatalf("ambiguous action evidence was not diagnosed: %v", codes)
	}
}

func TestCodexAmbiguousSerializationTwinsRemainSeparate(t *testing.T) {
	raw := []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"same"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"same"}]}}
{"type":"event_msg","payload":{"type":"agent_message","message":"same"}}
`)
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 3 {
		t.Fatal("equal text chose one of two possible twins")
	}
	for _, item := range items {
		if len(item.Relations) != 0 {
			t.Fatal("ambiguous text acquired a twin relationship")
		}
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "ambiguous_serialization_twin" {
		t.Fatal("ambiguous twin capture lacked a diagnostic")
	}
}
