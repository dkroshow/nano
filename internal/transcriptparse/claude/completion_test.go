package claude

import (
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"testing"
)

func TestClaudeInterruptionMarkersAreNotPrompts(t *testing.T) {
	for _, name := range []string{"interruption-plain", "interruption-tag", "interruption-tool-use"} {
		t.Run(name, func(t *testing.T) {
			state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, name)))
			if err != nil {
				t.Fatal(err)
			}
			items := orderedItems(state)
			if len(items) != 2 || len(state.Turns) != 1 || items[1].Type != model.ItemNotice || items[1].Role != model.RoleSystem || items[1].Outcome != model.OutcomeInterrupted {
				t.Fatal("native interruption became another human prompt")
			}
			for _, turn := range state.Turns {
				if turn.Outcome != model.OutcomeInterrupted || len(turn.FinalItemRefs) != 0 {
					t.Fatal("interruption did not settle its existing turn")
				}
			}
		})
	}
}

func TestClaudeSyntheticErrorCannotBecomeFinal(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "synthetic-error-stop-sequence")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || items[1].Type != model.ItemNotice || items[1].Outcome != model.OutcomeFailed || items[1].Finality.State == model.FinalityCandidate || items[1].IsFinalAnswer {
		t.Fatal("synthetic error stub became an answer")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeFailed || len(turn.FinalItemRefs) != 0 {
			t.Fatal("stop_sequence overrode API error")
		}
	}
}

func TestClaudeAPIErrorKeepsConversationUnsettled(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "partial-assistant-api-error-stop")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "Partial." || !items[0].Complete || items[0].Outcome != model.OutcomeUnknown {
		t.Fatal("API failure erased generated text")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown || len(turn.FinalItemRefs) != 0 {
			t.Fatal("API failure settled whole conversation")
		}
		found := false
		for _, obs := range turn.Observations {
			if obs.Outcome == model.OutcomeFailed {
				found = true
			}
		}
		if !found {
			t.Fatal("API failure evidence lost")
		}
	}
}

func TestClaudeRefusalRetractsNamedBlocks(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "model-refusal-retraction")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "Retracted response." || items[0].Finality.State != model.FinalityRetracted || len(items[0].Finality.ObservationRefs) == 0 {
		t.Fatal("named retraction ignored or deleted content")
	}
	state, _, err = parseFileView(state, inputFor([]byte(`{"type":"result","subtype":"success","sessionId":"session-example"}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	items = orderedItems(state)
	if items[0].Finality.State != model.FinalityRetracted || items[0].IsFinalAnswer {
		t.Fatal("later success promoted retracted content")
	}
}

func TestClaudeDerivedStopCannotOverrideNativeFailure(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true}
{"type":"assistant","uuid":"late","message":{"id":"m","content":[{"type":"text","text":"Late snapshot"}],"stop_reason":"end_turn"}}
`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeFailed || len(turn.FinalItemRefs) != 0 || len(turn.Completion) != 2 {
			t.Fatal("derived message stop erased native failure evidence")
		}
	}
}

func TestClaudeNativeSuccessCannotBecomeProvisional(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"result","subtype":"success"}
{"type":"assistant","uuid":"late","message":{"id":"m","content":[{"type":"text","text":"Late snapshot"}],"stop_reason":"end_turn"}}
{"type":"assistant","uuid":"tool","message":{"id":"t","content":[{"type":"tool_use","id":"c","name":"Read","input":{}}]}}
`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeCompleted {
			t.Fatal("later derived evidence demoted native terminal")
		}
	}
}

func TestClaudeToolActivityReopensProvisionalStop(t *testing.T) {
	rows := []byte(`{"type":"assistant","uuid":"answer","message":{"id":"a","content":[{"type":"text","text":"Provisional"}],"stop_reason":"end_turn"}}
{"type":"assistant","uuid":"tool","message":{"id":"b","content":[{"type":"tool_use","id":"c","name":"Read","input":{}}],"stop_reason":"tool_use"}}
`)
	state, _, err := parseFileView(parsedView{}, inputFor(rows))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || items[0].Finality.State != model.FinalityRetracted {
		t.Fatal("tool activity kept provisional finality")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeRunning || len(turn.FinalItemRefs) != 0 || len(turn.Completion) != 1 {
			t.Fatal("provisional completion not revised with evidence intact")
		}
	}
}

func TestClaudeNonHumanOriginDoesNotOpenTurn(t *testing.T) {
	for _, name := range []string{"origin-task-notification", "origin-coordinator"} {
		t.Run(name, func(t *testing.T) {
			state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, name)))
			if err != nil {
				t.Fatal(err)
			}
			items := orderedItems(state)
			if len(items) != 1 || items[0].Type != model.ItemNotice || items[0].Role != model.RoleSystem || len(items[0].Detail) == 0 || len(state.Turns) != 0 {
				t.Fatal("non-human envelope became a human turn or lost origin")
			}
		})
	}
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "origin-human")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Type != model.ItemUserMessage || len(state.Turns) != 1 {
		t.Fatal("explicit human origin suppressed prompt")
	}
}
