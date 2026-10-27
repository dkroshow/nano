package claude

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeQuestionCallAfterControlKeepsFirstItem(t *testing.T) {
	rows := []string{
		`{"type":"control_request","request_id":"r","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"c","input":{"questions":[{"question":"Choose?"}]}}}`,
		`{"type":"control_response","response":{"subtype":"success","request_id":"r","response":{"behavior":"allow","updatedInput":{"answers":{"Choose?":"Blue"}}}}}`,
		`{"type":"assistant","uuid":"tool","message":{"id":"m","content":[{"type":"tool_use","id":"c","name":"AskUserQuestion","input":{"questions":[{"question":"Choose?"}]}}]}}`,
		`{"type":"user","uuid":"answer","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"Answered"}]},"toolUseResult":{"answers":{"Choose?":"Blue"}}}`,
	}
	var state parsedView
	first := ""
	for i, row := range rows {
		checkpoint, _ := json.Marshal(state)
		var restored parsedView
		if err := json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		next, batch, err := parseFileView(restored, inputFor([]byte(row+"\n")))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("late question: %v %+v", err, batch.Diagnostics)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(checkpoint, after) {
			t.Fatal("late call mutated an earlier checkpoint")
		}
		state = next
		if i == 0 {
			first = orderedItems(state)[0].ID
		}
	}
	items := orderedItems(state)
	if len(items) != 2 || items[0].ID != first || items[0].SourceSeq != 1 || items[0].Outcome != model.OutcomeCompleted || items[0].TurnKey == "" || items[1].TurnKey != items[0].TurnKey || items[1].Relations[0].ToItem != first || items[1].Text != "Choose?\n→ Blue" {
		t.Fatal("late native invocation duplicated its control or lost decision/result binding")
	}
	if items[0].Observations[0].Scope.TurnKey != "" {
		t.Fatal("late binding rewrote the original unknown turn scope")
	}
}

func TestClaudeMixedQuestionResultsRetainRowOverlayWithoutGuessing(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"calls","message":{"id":"m","content":[{"type":"tool_use","id":"one","name":"AskUserQuestion","input":{"questions":[{"question":"First?"}]}},{"type":"tool_use","id":"two","name":"AskUserQuestion","input":{"questions":[{"question":"Second?"}]}}]}}` + "\n" +
		`{"type":"user","uuid":"results","message":{"content":[{"type":"tool_result","tool_use_id":"one","content":"Native first answer"},{"type":"tool_result","tool_use_id":"two","content":"Native second answer"}]},"toolUseResult":{"answers":{"First?":"Blue","Second?":"Green"}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 4 || items[2].Text != "Native first answer" || items[3].Text != "Native second answer" || items[2].Relations[0].ToItem != items[0].ID || items[3].Relations[0].ToItem != items[1].ID {
		t.Fatal("mixed native result blocks lost their own text or call binding")
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "ambiguous_question_answer_overlay" {
		t.Fatal("row-level answers silently assigned to multiple result blocks")
	}
	found := false
	for _, obs := range batch.Observations {
		var row map[string]json.RawMessage
		if json.Unmarshal(obs.Detail, &row) == nil && rawObject(row, "toolUseResult")["answers"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("row-level answer overlay was dropped")
	}
}

func TestClaudeCompositeQuestionKeepsInvocationAndResultFacets(t *testing.T) {
	raw := fixture(t, "question-control-and-file-answer")
	input := inputFor(raw)
	input.Scope.NativeThreadID = nil
	state, batch, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || len(state.Turns) != 0 || len(batch.Diagnostics) != 0 {
		t.Fatalf("want one interaction and its result without an invented turn: items=%d turns=%d diagnostics=%v", len(items), len(state.Turns), batch.Diagnostics)
	}
	if items[0].Type != model.ItemInteraction || items[1].Type != model.ItemToolResult || items[1].Text != "Choose a color.\n→ Blue, Green" {
		t.Fatal("structured question answer was not retained as a result facet")
	}
	if len(items[1].Relations) != 1 || items[1].Relations[0].ToItem != items[0].ID || items[1].Relations[0].Kind != model.RelationResultOf {
		t.Fatal("file answer has no proven question invocation")
	}
	var detail struct {
		Result struct {
			Answers map[string][]string `json:"answers"`
		} `json:"toolUseResult"`
	}
	if json.Unmarshal(items[1].Detail, &detail) != nil || len(detail.Result.Answers["Choose a color."]) != 2 {
		t.Fatal("exact native answer keys or arrays lost")
	}
	state, batch, err = parseFileView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 2 || len(state.Turns) != 0 || len(batch.Diagnostics) != 0 {
		t.Fatal("composite replay duplicated an interaction or result")
	}
}

func TestClaudeQuestionResultCannotChooseReusedCallID(t *testing.T) {
	rows := []byte(`{"type":"control_request","request_id":"one","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"reused","input":{"questions":[]}}}` + "\n" +
		`{"type":"control_request","request_id":"two","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"reused","input":{"questions":[]}}}` + "\n" +
		`{"type":"user","uuid":"answer","message":{"content":[{"type":"tool_result","tool_use_id":"reused","content":"Answer"}]},"toolUseResult":{"answers":{}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputFor(rows))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range batch.Diagnostics {
		if diagnostic.Code == "ambiguous_question_call" {
			found = true
		}
	}
	if !found {
		t.Fatal("ambiguous native question call was not diagnosed")
	}
	for _, item := range state.Items {
		if item.Type == model.ItemToolResult && len(item.Relations) != 0 {
			t.Fatal("ambiguous result was joined")
		}
	}
}

func TestClaudeQuestionToolCallAndControlShareInvocation(t *testing.T) {
	rows := []byte(`{"type":"assistant","uuid":"tool","message":{"id":"message","content":[{"type":"tool_use","id":"call","name":"AskUserQuestion","input":{"questions":[{"question":"Choose?"}]}}]}}` + "\n" +
		`{"type":"control_request","request_id":"request","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","tool_use_id":"call","input":{"questions":[{"question":"Choose?"}]}}}` + "\n" +
		`{"type":"control_response","response":{"subtype":"success","request_id":"request","response":{"behavior":"allow","updatedInput":{"answers":{"Choose?":"Blue"}}}}}` + "\n" +
		`{"type":"user","uuid":"answer","message":{"content":[{"type":"tool_result","tool_use_id":"call","content":"Answered"}]},"toolUseResult":{"answers":{"Choose?":"Blue"}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputFor(rows))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || len(state.Turns) != 1 || len(batch.Diagnostics) != 0 || items[0].Type != model.ItemInteraction || items[1].Type != model.ItemToolResult {
		t.Fatal("native question invocation and matching control did not remain one item")
	}
	if items[0].SourceSeq != 1 || len(items[0].Observations) != 3 || items[1].TurnKey != items[0].TurnKey || items[1].Relations[0].ToItem != items[0].ID {
		t.Fatal("question join lost original position, observations or turn ownership")
	}
	if items[0].Text == "" {
		t.Fatal("control answer erased generated question content")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown {
			t.Fatal("answer completed the conversation")
		}
	}
}
