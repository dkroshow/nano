package claude

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeControlAnsweredPendingAndReplay(t *testing.T) {
	raw := fixture(t, "control-replay-answered-and-pending")
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || len(state.Turns) != 0 || len(batch.Diagnostics) != 0 {
		t.Fatal("controls did not produce two independent interactions without turns")
	}
	if items[0].Type != model.ItemInteraction || items[0].Outcome != model.OutcomeCompleted || items[1].Outcome != model.OutcomeRunning || !items[1].Complete {
		t.Fatal("answer state was confused with native content completeness")
	}
	if items[0].CallID != "call1" || items[1].CallID != "call1" || items[0].ID == items[1].ID {
		t.Fatal("request identities merged merely because tool call IDs match")
	}
	saved, _ := json.Marshal(state)
	var restored parsedView
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	state, batch, err = parseFileView(restored, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	items = orderedItems(state)
	if !bytes.Equal(saved, after) || len(items) != 2 || items[0].Outcome != model.OutcomeCompleted || items[1].Outcome != model.OutcomeRunning || len(batch.Diagnostics) != 0 {
		t.Fatal("control replay mutated prior state, duplicated requests or reopened an answer")
	}
	if len(items[0].Relations) != 3 || len(items[1].Relations) != 1 {
		t.Fatal("control result/replay evidence is missing")
	}
}

func TestClaudeControlSettingsAndUnknownCancellation(t *testing.T) {
	prompt := []byte(`{"type":"user","message":{"content":"Prompt"}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputFor(prompt))
	if err != nil {
		t.Fatal(err)
	}
	state, batch, err := parseFileView(state, inputFor(fixture(t, "control-settings-interrupt-cancel")))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 || len(state.Turns) != 1 {
		t.Fatal("settings created content or turns")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown || len(turn.Completion) != 0 || turn.Model != "" {
			t.Fatal("control acknowledgement changed conversation completion or model")
		}
	}
	foundUnverified := false
	for _, diagnostic := range batch.Diagnostics {
		if diagnostic.Code == "unverified_control_schema" {
			foundUnverified = true
		}
	}
	if !foundUnverified {
		t.Fatal("unverified cancellation framing was silently accepted")
	}
}

func TestClaudeControlNamespacesAndConflicts(t *testing.T) {
	var state parsedView
	feed := func(raw, direction string) model.Batch {
		t.Helper()
		input := inputFor([]byte(raw))
		input.Source.Direction = direction
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, input)
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	feed(`{"type":"control_request","request_id":"same","request":{"subtype":"initialize"}}`, "send")
	feed(`{"type":"control_request","request_id":"same","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}`, "recv")
	response := `{"type":"control_response","response":{"subtype":"success","request_id":"same","response":{"behavior":"allow"}}}`
	batch := feed(response, "")
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "ambiguous_control_namespace" || orderedItems(state)[0].Outcome != model.OutcomeRunning {
		t.Fatal("directionless response guessed between both requesters")
	}
	batch = feed(response, "send")
	if len(batch.Diagnostics) != 0 || orderedItems(state)[0].Outcome != model.OutcomeCompleted {
		t.Fatal("response did not match opposite direction")
	}
	batch = feed(`{"type":"control_response","response":{"subtype":"success","request_id":"same","response":{"behavior":"deny"}}}`, "send")
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "control_response_conflict" || orderedItems(state)[0].Outcome != model.OutcomeCompleted {
		t.Fatal("conflicting response replaced the accepted decision")
	}
}

func TestClaudeControlUnsupportedAndOrphanEvidence(t *testing.T) {
	for _, name := range []string{"control-hook-callback", "control-mcp-message", "control-request-user-dialog", "control-elicitation", "permission-denied-and-question-skipped"} {
		t.Run(name, func(t *testing.T) {
			state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, name)))
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Observations) == 0 || len(state.Turns) != 0 {
				t.Fatal("control evidence lost or assigned to an invented turn")
			}
			if name != "control-elicitation" && len(batch.Diagnostics) == 0 {
				t.Fatal("uncertain schema or missing request not diagnosed")
			}
		})
	}
}

func TestClaudeControlFirstNativeBindingWithoutTurns(t *testing.T) {
	for _, request := range []string{
		`{"subtype":"initialize"}`,
		`{"subtype":"can_use_tool","tool_name":"Bash","input":{}}`,
	} {
		input := inputFor([]byte(`{"type":"control_request","request_id":"first","request":` + request + `}`))
		input.Scope.NativeThreadID = nil
		state, _, err := parseWireView(parsedView{}, input)
		if err != nil {
			t.Fatal(err)
		}
		input.Bytes = []byte(`{"type":"control_response","session_id":"native","response":{"subtype":"success","request_id":"first","response":{"behavior":"allow"}}}`)
		state, batch, err := parseWireView(state, input)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Diagnostics) != 0 || len(state.Turns) != 0 {
			t.Fatal("first native binding lost a source-local control or invented a turn")
		}
		for _, item := range state.Items {
			if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != "native" || item.Outcome != model.OutcomeCompleted {
				t.Fatal("source-local interaction was not bound and answered")
			}
		}
	}
}
