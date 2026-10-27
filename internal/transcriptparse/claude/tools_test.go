package claude

import (
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeNativeCommandAndProposedChanges(t *testing.T) {
	state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, "tool-call-completeness-and-failure")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || items[0].Type != model.ItemCommand || items[0].Text != "printf example" || !strings.Contains(items[0].Parts[0].Text, `"timeout":1000`) || items[0].Outcome != model.OutcomeUnknown || items[1].Outcome != model.OutcomeFailed || len(batch.Diagnostics) != 0 {
		t.Fatal("command specialization lost arguments or conflated generation and execution")
	}
	state, batch, err = parseFileView(parsedView{}, inputFor(fixture(t, "plan-and-file-change-tools")))
	if err != nil {
		t.Fatal(err)
	}
	items = orderedItems(state)
	if len(items) != 2 || items[0].Type != model.ItemPlan || items[0].Text != "Example plan." || items[1].Type != model.ItemFileChange || !strings.Contains(items[1].Text, "/workspace/example.txt") || len(batch.Diagnostics) != 0 {
		t.Fatal("native plan/edit intent not specialized")
	}
	if items[0].Outcome != model.OutcomeUnknown || items[1].Outcome != model.OutcomeUnknown {
		t.Fatal("proposed plan/edit became approved or applied")
	}
	state, batch, err = parseFileView(state, inputFor([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"plan1","content":"Accepted"}]}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	items = orderedItems(state)
	if len(items) != 3 || len(batch.Diagnostics) != 0 || len(items[2].Relations) != 1 || items[2].Relations[0].ToItem != items[0].ID {
		t.Fatal("plan result lost its scoped invocation")
	}
}

func TestClaudeStreamedCommandKeepsArgumentsAcrossProjection(t *testing.T) {
	var state parsedView
	rows := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"Bash","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"printf example\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
	}
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputFor([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	item := orderedItems(state)[0]
	if item.Type != model.ItemCommand || item.Text != "printf example" || item.Parts[0].Text != `{"command":"printf example"} ` || !item.Complete || item.Outcome != model.OutcomeUnknown {
		t.Fatal("display command replaced native argument assembly")
	}
}
