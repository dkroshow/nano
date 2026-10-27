package grok

import (
	"bytes"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokReverseFixtureKeepsThreeInteractionFacets(t *testing.T) {
	var state parsedView
	rows := bytes.Split(bytes.TrimSpace(fixtureBytes(t, "reverse-requests")), []byte{'\n'})
	for _, row := range rows {
		before, _ := json.Marshal(state)
		var restored parsedView
		if err := json.Unmarshal(before, &restored); err != nil {
			t.Fatal(err)
		}
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(restored, inputBytes(row))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("reverse fixture: %v %+v", err, batch.Diagnostics)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(before, after) {
			t.Error("interaction changed prior checkpoint")
		}
	}
	items := controlItems(state)
	if len(items) != 3 || len(state.Turns) != 0 {
		t.Fatal("requests/answers lost facets or invented turns")
	}
	wantText := []string{"Read example", "Read and report.", "Choose a color"}
	wantOutcome := []model.Outcome{model.OutcomeCompleted, model.OutcomeUnknown, model.OutcomeCompleted}
	for i, item := range items {
		if item.Type != model.ItemInteraction || item.Text != wantText[i] || item.Outcome != wantOutcome[i] || !item.Complete || len(item.Observations) != 2 {
			t.Errorf("interaction %d lost body, outcome or ordered evidence", i)
		}
		if item.NativeItemID != "" || len(item.Aliases) != 1 || item.Aliases[0].Namespace != "grok.rpcRequestId" || item.IsFinalAnswer {
			t.Error("RPC request became a native item/final answer")
		}
		if len(item.Parts) < 2 || item.Parts[len(item.Parts)-1].Type != "control_response" {
			t.Error("response lost its own part")
		}
	}
	state, batch, err := parseWireView(state, inputBytes(rows[5]))
	if err != nil || len(batch.Diagnostics) != 0 || len(state.Items) != 3 || len(controlItems(state)[2].Observations) != 3 || len(controlItems(state)[2].Parts) != 2 {
		t.Error("response replay duplicated or discarded interaction evidence")
	}
}

func TestGrokPermissionUsesNativeOptionKindAndKeepsExecutionSeparate(t *testing.T) {
	rows := []string{
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call","title":"Read original","status":"in_progress"},"_meta":{"promptId":"p1","eventId":"call-event"}}}`,
		`{"id":1,"method":"session/request_permission","params":{"sessionId":"s1","toolCall":{"toolCallId":"call","title":"Approve read"},"options":[{"optionId":"looks-like-allow","kind":"reject_once","name":"Reject"}]}}`,
		`{"id":1,"result":{"outcome":{"outcome":"selected","optionId":"looks-like-allow"}}}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	items := controlItems(state)
	if len(items) != 2 || items[0].Outcome != model.OutcomeRunning || items[1].Outcome != model.OutcomeDeclined {
		t.Fatal("option name overrode native kind or approval changed execution")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown {
			t.Error("interaction completed parent turn")
		}
	}
}

func TestGrokQuestionAnswersPreserveUnansweredAndAmbiguousKeys(t *testing.T) {
	for _, tc := range []struct{ questions, answers, diagnostic string }{
		{`[{"question":"Choose"}]`, `{}`, "unanswered_control_question"},
		{`[{"question":"Choose"},{"question":"Choose"}]`, `{"Choose":["Blue"]}`, "ambiguous_question_key"},
	} {
		state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"id":"q","method":"_x.ai/ask_user_question","params":{"sessionId":"s1","questions":`+tc.questions+`}}`)))
		if err != nil {
			t.Fatal(err)
		}
		state, batch, err := parseWireView(state, inputBytes([]byte(`{"id":"q","result":{"outcome":"accepted","answers":`+tc.answers+`}}`)))
		items := controlItems(state)
		if err != nil || len(items) != 1 || items[0].Outcome != model.OutcomeUnknown || !hasRPCDiagnostic(batch, tc.diagnostic) {
			t.Errorf("ambiguous/unanswered reply: %v %+v", err, batch.Diagnostics)
		}
	}
}

func TestGrokPermissionRequiresOneDeclaredOption(t *testing.T) {
	for _, tc := range []struct{ options, selection string }{
		{`[{"kind":"allow_once"}]`, `{}`},
		{`[{"optionId":"same","kind":"reject_once"},{"optionId":"same","kind":"allow_once"}]`, `{"optionId":"same"}`},
	} {
		state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"id":"p","method":"session/request_permission","params":{"sessionId":"s1","options":`+tc.options+`}}`)))
		if err != nil {
			t.Fatal(err)
		}
		var selection map[string]any
		if err := json.Unmarshal([]byte(tc.selection), &selection); err != nil {
			t.Fatal(err)
		}
		selection["outcome"] = "selected"
		row, _ := json.Marshal(map[string]any{"id": "p", "result": map[string]any{"outcome": selection}})
		state, batch, err := parseWireView(state, inputBytes(row))
		if err != nil || controlItems(state)[0].Outcome != model.OutcomeUnknown || !hasRPCDiagnostic(batch, "unverified_permission_option") {
			t.Errorf("missing or duplicate option accepted: %v %+v", err, batch.Diagnostics)
		}
	}
}

func TestGrokInteractionPartsRetainWrappedNativePaths(t *testing.T) {
	rows := []string{
		`{"dir":"recv","msg":{"id":1,"method":"_x.ai/ask_user_question","params":{"questions":[{"question":"First"},{"question":"Second"}]}}}`,
		`{"dir":"send","msg":{"id":1,"result":{"outcome":"cancelled"}}}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	item := controlItems(state)[0]
	for index, path := range []string{"$.msg.params.questions[0]", "$.msg.params.questions[1]", "$.msg.result"} {
		if item.Parts[index].Source.BlockPath != path {
			t.Errorf("part source %q; want %q", item.Parts[index].Source.BlockPath, path)
		}
	}
}

func TestGrokControlClocksKeepCallerAndNativeEvidenceSeparate(t *testing.T) {
	capture, observed := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
	input := inputBytes([]byte(`{"id":1,"method":"session/request_permission","params":{"sessionId":"s","toolCall":{"title":"Read"}}}`))
	input.CaptureAt, input.ObservedAt = &capture, &observed
	state, _, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Bytes = []byte(`{"id":1,"result":{"outcome":{"outcome":"cancelled"}}}`)
	state, batch, err := parseWireView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	item := controlItems(state)[0]
	if item.CreatedAt != nil || len(item.Times) != 4 {
		t.Fatal("control clocks missing or promoted to native time")
	}
	for _, obs := range item.Observations {
		if len(obs.Times) != 2 || obs.Times[0].Class != model.TimeCapture || obs.Times[1].Class != model.TimeObserved {
			t.Fatal("clock classes lost")
		}
		for _, stamp := range obs.Times {
			if stamp.Scope.ItemID != item.ID || stamp.ObservationRef != obs.ID {
				t.Error("clock belongs to another observation or scope")
			}
		}
	}
	if batch.Observations[0].Times[0].Scope.ItemID != item.ID {
		t.Error("emitted reply clock scope differs from item")
	}
}

func controlItems(state parsedView) []model.Item {
	var items []model.Item
	for _, item := range state.Items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
	return items
}
