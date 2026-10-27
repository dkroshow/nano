package codex

import (
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexFileActionFixturesRetainExecution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     model.ItemType
		outcome model.Outcome
	}{
		{"file-event-msg-exec-command-begin", model.ItemCommand, model.OutcomeRunning},
		{"file-event-msg-exec-command-end", model.ItemCommand, model.OutcomeFailed},
		{"file-event-msg-patch-apply-begin", model.ItemFileChange, model.OutcomeRunning},
		{"file-event-msg-patch-apply-end", model.ItemFileChange, model.OutcomeCompleted},
		{"capture-patch-without-direct-twin", model.ItemFileChange, model.OutcomeCompleted},
	} {
		state, _ := parseFileCase(t, tc.name)
		items := orderedItems(state)
		if len(items) != 1 || items[0].Type != tc.typ || items[0].Outcome != tc.outcome || items[0].CallID == "" || len(items[0].Parts) == 0 {
			t.Fatalf("%s lost native action facets", tc.name)
		}
		for _, turn := range state.Turns {
			if len(turn.Completion) != 0 {
				t.Fatal("action completed its parent turn")
			}
		}
	}
}

func TestCodexFileActionProgressDoesNotReplaceGeneratedInvocation(t *testing.T) {
	raw := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c","arguments":"{\"cmd\":\"exit 2\"}"}}
{"type":"event_msg","payload":{"type":"exec_command_begin","call_id":"c","command":["exit","2"]}}
{"type":"event_msg","payload":{"type":"exec_command_end","call_id":"c","exit_code":2,"stdout":"","stderr":"failed","aggregated_output":"failed"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"c","output":"native result"}}
`
	state, _, err := parseFileView(parsedView{}, inputFor([]byte(raw)))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 3 || items[0].Outcome != model.OutcomeUnknown || items[1].Type != model.ItemCommand || items[1].Outcome != model.OutcomeFailed || len(items[1].Observations) != 2 || !strings.Contains(string(items[1].Detail), "failed") {
		t.Fatal("execution progress duplicated or failed generation")
	}
	if len(items[2].Relations) != 1 || items[2].Relations[0].ToItem != items[0].ID {
		t.Fatal("result linked execution update instead of generated invocation")
	}
	found := false
	for _, r := range items[1].Relations {
		if r.Kind == model.RelationProgressOf && r.ToItem == items[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatal("native execution did not retain addressed invocation relationship")
	}
}

func TestCodexFileGenerationStatusAndCommandResultStaySeparate(t *testing.T) {
	for _, name := range []string{"file-response-item-custom-tool-call", "file-response-item-local-shell-call"} {
		state, _ := parseFileCase(t, name)
		item := orderedItems(state)[0]
		if !item.Complete || item.Outcome != model.OutcomeUnknown {
			t.Fatalf("%s: generated arguments proved execution", name)
		}
		if name == "file-response-item-local-shell-call" && item.Text != "printf example" {
			t.Fatal("nested native shell argv lost")
		}
	}
	state, _ := parseFileCase(t, "edge-command-arguments-and-object-result")
	items := orderedItems(state)
	if len(items) != 2 || items[0].Outcome != model.OutcomeUnknown || items[1].Outcome != model.OutcomeFailed || len(items[1].Relations) != 1 {
		t.Fatal("command result lost native exit failure or failed generation")
	}
	state, _ = parseFileCase(t, "edge-question-answer")
	items = orderedItems(state)
	if len(items) != 2 || items[0].Type != model.ItemInteraction || len(items[1].Relations) != 1 || !strings.Contains(items[1].Text, "choice") {
		t.Fatal("native question invocation lost its answer facet")
	}
}
