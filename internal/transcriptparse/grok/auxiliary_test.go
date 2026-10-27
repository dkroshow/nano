package grok

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokModelHistoryRetainsBodiesCallsAndOpaqueReasoning(t *testing.T) {
	input := inputBytes(fixtureBytes(t, "capture-chat-history"))
	input.Closed = true
	state, batch, err := parseFileView(parsedView{}, input)
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("model history: %v %+v", err, batch.Diagnostics)
	}
	items := controlItems(state)
	if len(items) != 9 || len(state.Turns) != 0 {
		t.Fatal("model history lost facets or created ACP turns")
	}
	want := []model.ItemType{model.ItemNotice, model.ItemUserMessage, model.ItemReasoning, model.ItemAgentMessage, model.ItemToolCall, model.ItemToolCall, model.ItemToolCall, model.ItemToolResult, model.ItemToolCall}
	for i, item := range items {
		if item.Type != want[i] || !item.Complete || item.IsFinalAnswer || item.CreatedAt != nil {
			t.Errorf("model facet %d has wrong semantics", i)
		}
		if item.TurnKey != "" || item.Scope.NativeTurnID != nil || len(item.Observations) != 1 {
			t.Error("model history borrowed turn/event identity")
		}
	}
	if items[2].Text != "[text]" || len(items[2].Parts) != 2 || items[2].Parts[1].Type != "encrypted_content" || items[2].Parts[1].Text != "" {
		t.Error("reasoning lost summary or exposed opaque body as text")
	}
	if items[4].CallID != "id-7" || items[4].ToolName != "[text]" || items[4].Parts[0].Text != "[text]" || items[4].Parts[0].Source.BlockPath != "$.tool_calls[0].arguments" {
		t.Error("nested call lost argument bytes or source path")
	}
	if len(items[7].Relations) != 1 || items[7].Relations[0].Kind != model.RelationResultOf || items[7].Relations[0].ToItem != items[4].ID || items[7].Outcome != model.OutcomeUnknown {
		t.Error("result join lost or reply invented tool success")
	}
	if items[8].Outcome != model.OutcomeCompleted || items[8].CallID != "id-7867" {
		t.Error("backend native status or call identity lost")
	}
}

func TestGrokTelemetryRetainsNativeRowsWithoutConversationClaims(t *testing.T) {
	raw := fixtureBytes(t, "capture-events")
	input := inputBytes(raw)
	input.Closed = true
	callerTurn := "unrelated"
	input.Scope.NativeTurnID = &callerTurn
	state, batch, err := parseFileView(parsedView{}, input)
	rows := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Observations) != len(rows) {
		t.Fatal("telemetry disappeared or became transcript content")
	}
	for i, obs := range batch.Observations {
		if obs.Kind != model.ObservationMetadata || obs.Scope.NativeTurnID != nil || obs.Outcome != "" {
			t.Error("telemetry claimed conversation outcome or turn")
		}
		if rpcDigest(fieldsOf(obs.Detail)) != rpcDigest(fieldsOf(rows[i])) {
			t.Error("telemetry fields lost")
		}
		if len(obs.Times) != 1 || obs.Times[0].Value != nil || obs.Times[0].SourceField != "ts" || string(obs.Times[0].Raw) != `"[text]"` {
			t.Error("scrubbed native timestamp was dropped or invented")
		}
	}
	if !hasRPCDiagnostic(batch, "invalid_timestamp") {
		t.Error("invalid scrubbed timestamp was not diagnosed")
	}
}

func TestGrokUnknownAuxiliaryRetainsNativeDetail(t *testing.T) {
	input := inputBytes([]byte(`{"type":"future_history","id":"native","content":"retained","count":9007199254740993}`))
	input.Closed = true
	state, batch, err := parseFileView(parsedView{}, input)
	if err != nil || len(state.Items) != 0 || len(batch.Observations) != 1 || !hasRPCDiagnostic(batch, "unknown_auxiliary_record") {
		t.Fatal("unknown native row lost")
	}
	if !bytes.Contains(batch.Observations[0].Detail, []byte("9007199254740993")) || documentString(fieldsOf(batch.Observations[0].Detail), "content") != "retained" {
		t.Error("unknown detail lost exact values")
	}
}

func TestGrokAuxiliaryClocksScopeAndCheckpoint(t *testing.T) {
	capture, observed := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
	input := inputBytes([]byte("{\"type\":\"user\",\"content\":\"same\",\"synthetic_reason\":\"provider_continuation\"}\n"))
	input.CaptureAt, input.ObservedAt = &capture, &observed
	callerTurn := "unrelated"
	input.Scope.NativeTurnID = &callerTurn
	state, _, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state)
	var restored parsedView
	if err = json.Unmarshal(before, &restored); err != nil {
		t.Fatal(err)
	}
	next, _, err := parseFileView(restored, input)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(before, after) {
		t.Error("auxiliary reader mutated prior checkpoint")
	}
	items := controlItems(next)
	if len(items) != 2 {
		t.Fatal("identical model rows were guessed to be replay")
	}
	for _, item := range items {
		if !item.Synthetic || item.Scope.NativeTurnID != nil || item.CreatedAt != nil || len(item.Times) != 2 || item.Times[0].Class != model.TimeCapture || item.Times[1].Class != model.TimeObserved {
			t.Error("auxiliary origin, scope or clocks lost")
		}
	}
}

func TestGrokModelCallJoinRequiresUnambiguousSourceEvidence(t *testing.T) {
	input := inputBytes([]byte(`{"type":"assistant","content":" ","tool_calls":[{"id":"same","name":"Read","arguments":"  {\"path\":\"x\"}  "}]}` + "\n"))
	state, _, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if controlItems(state)[0].Text != " " || controlItems(state)[1].Text != `  {"path":"x"}  ` {
		t.Fatal("native text or encoded arguments trimmed")
	}
	state, batch, err := parseFileView(state, input)
	if err != nil || !hasRPCDiagnostic(batch, "ambiguous_model_call_id") {
		t.Fatal("repeated model call ID not diagnosed")
	}
	input.Bytes = []byte("{\"type\":\"tool_result\",\"tool_call_id\":\"same\",\"content\":\"result\"}\n")
	state, batch, err = parseFileView(state, input)
	items := controlItems(state)
	if err != nil || !hasRPCDiagnostic(batch, "unresolved_model_tool_result") || len(items[len(items)-1].Relations) != 0 {
		t.Fatal("result chose an ambiguous call")
	}

	input.Source.Incarnation = "another-source"
	state, batch, err = parseFileView(parsedView{}, input)
	if err != nil || !hasRPCDiagnostic(batch, "unresolved_model_tool_result") || len(controlItems(state)[0].Relations) != 0 {
		t.Fatal("result borrowed a call from another source")
	}
	_, batch, err = parseWireView(parsedView{}, inputBytes([]byte(`{"type":"assistant","content":"model file row"}`)))
	if err != nil || len(batch.Changes) != 0 || !hasRPCDiagnostic(batch, "unsupported_envelope") {
		t.Fatal("model file row was interpreted as ACP wire content")
	}
}
