package grok

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func inputBytes(raw []byte) model.Input {
	return model.Input{Bytes: raw, Source: model.SourceRef{Label: "fixture", Incarnation: "capture-1"}}
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/research/" + name + "/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReadersPreserveIdenticalDeltasAndReplay(t *testing.T) {
	raw := fixtureBytes(t, "duplicate-versus-identical-delta")
	file, batch, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Items) != 1 || len(batch.Records) != 3 {
		t.Fatalf("items %d records %d", len(file.Items), len(batch.Records))
	}
	for _, item := range file.Items {
		if item.Text != "AA" || len(item.Observations) != 3 || len(item.Relations) != 1 || item.Relations[0].Kind != model.RelationReplayOf {
			t.Fatalf("replay assembly: %+v", item)
		}
		if item.Observations[0].NativeEventID != "e1" || item.Observations[1].NativeEventID != "e2" {
			t.Fatal("native time reordered source events")
		}
		if item.NativeItemID != "" {
			t.Fatal("derived segment key presented as native item identity")
		}
	}
	var wire parsedView
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		wire, _, err = parseWireView(wire, inputBytes(line))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(wire.Items) != 1 {
		t.Fatalf("wire items %d", len(wire.Items))
	}
	for _, item := range wire.Items {
		if item.Text != "AA" || len(item.Observations) != 3 {
			t.Fatalf("wire replay: %+v", item)
		}
	}
}

func TestFileReaderEverySplitRestoresCheckpoint(t *testing.T) {
	raw := fixtureBytes(t, "duplicate-versus-identical-delta")
	want, _, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Items) == 0 {
		t.Fatal("reader emitted no semantic items")
	}
	for split := 0; split <= len(raw); split++ {
		first, _, err := parseFileView(parsedView{}, inputBytes(raw[:split]))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		var restored parsedView
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		got, _, err := parseFileView(restored, inputBytes(raw[split:]))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d differs after serialized resume", split)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(after, encoded) {
			t.Fatalf("split %d mutated prior state", split)
		}
	}
}

func TestFileReaderTailMalformedAndOversizeRecover(t *testing.T) {
	valid := bytes.Split(fixtureBytes(t, "missing-identity-and-time"), []byte{'\n'})[0]
	state, batch, err := parseFileView(parsedView{}, inputBytes(valid))
	if err != nil || len(batch.Records) != 0 || state.Continuation.Framing.Checkpoint != 0 {
		t.Fatalf("tail consumed early: %+v, %v", batch, err)
	}
	closed := inputBytes(nil)
	closed.Closed = true
	state, batch, err = parseFileView(state, closed)
	if err != nil || len(state.Items) != 1 || len(batch.Records) != 1 {
		t.Fatalf("closed tail lost: %+v, %v", batch, err)
	}
	limit := len(valid) + 1
	raw := append(bytes.Repeat([]byte{'x'}, limit+10), '\n')
	raw = append(raw, []byte("{bad}\n")...)
	raw = append(raw, valid...)
	raw = append(raw, '\n')
	input := inputBytes(raw)
	input.MaxRowBytes = limit
	state, batch, err = parseFileView(parsedView{}, input)
	if err != nil || len(state.Items) != 1 || len(batch.Records) != 3 || len(batch.Diagnostics) != 2 {
		t.Fatalf("recovery: items %d, %+v, %v", len(state.Items), batch, err)
	}
	if batch.Diagnostics[0].Code != "row_too_large" || batch.Diagnostics[1].Code != "malformed_record" {
		t.Fatal(batch.Diagnostics)
	}
}

func TestWireReaderDoesNotInventNativeTimeOrIdentity(t *testing.T) {
	raw := bytes.Split(fixtureBytes(t, "missing-identity-and-time"), []byte{'\n'})[0]
	input := inputBytes(raw)
	capture, observed := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
	input.CaptureAt, input.ObservedAt = &capture, &observed
	state, _, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 {
		t.Fatalf("items %d", len(state.Items))
	}
	for _, item := range state.Items {
		if item.CreatedAt != nil || item.Scope.NativeTurnID != nil || item.NativeItemID != "" {
			t.Fatalf("invented native facts: %+v", item)
		}
		if len(item.Times) != 3 || item.Times[0].Value != nil || item.Times[1].Class != model.TimeCapture || item.Times[2].Class != model.TimeObserved {
			t.Fatal(item.Times)
		}
	}
}

func TestReaderRejectsSourceReplacement(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputBytes(nil))
	if err != nil {
		t.Fatal(err)
	}
	input := inputBytes(nil)
	input.Source.Incarnation = "replacement"
	_, _, err = parseFileView(state, input)
	if err == nil {
		t.Fatal("accepted changed source incarnation")
	}
	state.Continuation.Version++
	_, _, err = parseFileView(state, inputBytes(nil))
	if err == nil {
		t.Fatal("accepted incompatible checkpoint version")
	}
}

func TestReplayDoesNotReopenAnEarlierSegment(t *testing.T) {
	frames := []string{
		`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"A"}},"_meta":{"eventId":"e1","promptId":"p"}}}`,
		`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"c","title":"run"},"_meta":{"eventId":"e2","promptId":"p"}}}`,
		`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"A"}},"_meta":{"eventId":"e1","promptId":"p"}}}`,
		`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"B"}},"_meta":{"eventId":"e3","promptId":"p"}}}`,
	}
	var state parsedView
	for _, raw := range frames {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(raw)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(state.Items) != 3 {
		t.Fatalf("replay reopened earlier segment: %+v", state.Items)
	}
	for _, item := range state.Items {
		if item.Type == model.ItemAgentMessage && item.Text != "A" && item.Text != "B" {
			t.Fatalf("incorrect segment %q", item.Text)
		}
	}
}

func TestChildFrameCannotInheritParentPrompt(t *testing.T) {
	input := inputBytes([]byte(`{"method":"session/update","params":{"sessionId":"child","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"child"}}}}`))
	parent, prompt := "parent", "parent-prompt"
	input.Scope.NativeThreadID = &parent
	input.Scope.NativeTurnID = &prompt
	state, _, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Scope.NativeTurnID != nil || item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != "child" {
			t.Fatalf("child inherited parent scope: %+v", item.Scope)
		}
	}
}

func TestOutOfRangeNativeTimeKeepsCheckpointSerializable(t *testing.T) {
	input := inputBytes([]byte(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}},"_meta":{"agentTimestampMs":9223372036854775807}}}`))
	state, batch, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "timestamp_out_of_range" {
		t.Fatal(batch.Diagnostics)
	}
	for _, item := range state.Items {
		if item.CreatedAt != nil || string(item.Times[0].Raw) != "9223372036854775807" {
			t.Fatal(item.Times)
		}
	}
}

func TestNativeToolLifecycleDoesNotFailParentTurn(t *testing.T) {
	raw, err := os.ReadFile("testdata/scoped-tool-lifecycle.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	state, batch, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) != 0 {
		t.Fatal(batch.Diagnostics)
	}
	var call, result, answer model.Item
	for _, item := range state.Items {
		switch item.Type {
		case model.ItemToolCall:
			call = item
		case model.ItemToolResult:
			result = item
		case model.ItemAgentMessage:
			answer = item
		}
	}
	if len(call.Observations) != 2 || len(call.Relations) != 1 || call.Relations[0].Kind != model.RelationProgressOf || call.Relations[0].FromObservation != call.Observations[1].ID || call.Relations[0].ToItem != call.ID {
		t.Fatalf("progress evidence: %v", call.Relations)
	}
	if !result.Complete || result.Outcome != model.OutcomeFailed || len(result.Relations) != 1 || result.Relations[0].Kind != model.RelationResultOf || result.Relations[0].ToItem != call.ID {
		t.Fatalf("result outcome %s, relations %v", result.Outcome, result.Relations)
	}
	if !answer.Complete || answer.Finality.State != model.FinalityFinal || answer.Finality.Basis != model.BasisDerived || answer.Phase != "" || len(answer.Finality.ObservationRefs) != 1 {
		t.Fatalf("answer complete=%v finality=%+v phase=%q", answer.Complete, answer.Finality, answer.Phase)
	}
	for _, turn := range state.Turns {
		if turn.NativeTurnID == "p1" {
			if turn.Outcome != model.OutcomeCompleted || turn.FinalItemID != answer.ID || len(turn.Completion) != 1 || turn.Usage == nil || *turn.Usage.Output != 2 || turn.Usage.Total != nil {
				t.Fatalf("parent outcome or usage: %+v", turn)
			}
		} else if turn.NativeTurnID == "p2" {
			if turn.Outcome != model.OutcomeCancelled || turn.FinalItemID != "" || !turn.UsageIncomplete || len(turn.UsageObservations) != 1 || *turn.UsageObservations[0].ModelUsage["model-a"].Input != 10 {
				t.Fatalf("other prompt facts: %+v", turn)
			}
		} else {
			t.Fatalf("unexpected turn %q", turn.NativeTurnID)
		}
	}
}

func TestNativeTerminalOutcomeFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome model.Outcome
	}{{"terminal-cancelled", model.OutcomeCancelled}, {"terminal-error", model.OutcomeFailed}, {"terminal-interrupted", model.OutcomeInterrupted}, {"terminal-missing-reason", model.OutcomeUnknown}} {
		t.Run(tc.name, func(t *testing.T) {
			state, _, err := parseFileView(parsedView{}, inputBytes(fixtureBytes(t, tc.name)))
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Turns) != 1 {
				t.Fatalf("turns %d", len(state.Turns))
			}
			for _, turn := range state.Turns {
				if turn.Outcome != tc.outcome || turn.FinalItemID != "" {
					t.Fatalf("outcome %s final %q", turn.Outcome, turn.FinalItemID)
				}
			}
		})
	}
}

func TestMalformedUsageRetainsValidCounters(t *testing.T) {
	input := inputBytes([]byte(`{"method":"_x.ai/session/update","params":{"sessionId":"s","update":{"sessionUpdate":"turn_completed","prompt_id":"p","stop_reason":"end_turn","usage":{"inputTokens":"wrong","outputTokens":3}}}}`))
	state, batch, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "malformed_usage" {
		t.Fatal(batch.Diagnostics)
	}
	for _, turn := range state.Turns {
		if turn.Usage == nil || turn.Usage.Input != nil || turn.Usage.Output == nil || *turn.Usage.Output != 3 {
			t.Fatal(turn.Usage)
		}
	}
}

func TestPairedLatePromptAndCallBinding(t *testing.T) {
	for _, name := range []string{"updates.jsonl", "wire.jsonl"} {
		raw, err := os.ReadFile("testdata/research/paired-segments-tools-late-prompt/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var state parsedView
		if name == "updates.jsonl" {
			state, _, err = parseFileView(state, inputBytes(raw))
		} else {
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
				state, _, err = parseWireView(state, inputBytes(line))
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Turns) != 1 || len(state.Items) != 5 {
			t.Fatalf("%s turns=%d items=%d", name, len(state.Turns), len(state.Items))
		}
		var user, call, result, final model.Item
		for _, item := range state.Items {
			if item.Scope.NativeTurnID == nil || *item.Scope.NativeTurnID != "p1" {
				t.Fatal("late identity did not bind the same-session provisional item")
			}
			switch item.Type {
			case model.ItemUserMessage:
				user = item
			case model.ItemToolCall:
				call = item
			case model.ItemToolResult:
				result = item
			case model.ItemAgentMessage:
				if item.IsFinalAnswer {
					final = item
				}
			}
		}
		if user.Text != "Run a check." || call.ID == "" || len(call.Observations) != 2 || result.ID == "" || len(result.Relations) != 1 || result.Relations[0].ToItem != call.ID || final.Text != "Check passed." {
			t.Fatal("segment, call or terminal semantics lost")
		}
		for _, turn := range state.Turns {
			if turn.Outcome != model.OutcomeCompleted || turn.Usage == nil || *turn.Usage.Input != 20 {
				t.Fatal("bound prompt terminal/usage lost")
			}
		}
	}
}

func TestGrokTerminalReplayAndConflict(t *testing.T) {
	raw := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Answer"}},"_meta":{"eventId":"a1","promptId":"p1"}}}` + "\n" +
		`{"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":7}},"_meta":{"eventId":"done1"}}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	terminal := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})[1]
	state, batch, err := parseFileView(state, inputBytes(append(append([]byte(nil), terminal...), '\n')))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if len(turn.Completion) != 1 || len(turn.UsageObservations) != 1 || len(turn.Observations) != 2 {
			t.Fatal("replayed terminal duplicated completion or usage")
		}
	}
	found := false
	for _, change := range batch.Changes {
		if change.Relation != nil && change.Relation.Kind == model.RelationReplayOf {
			found = true
		}
	}
	if !found {
		t.Fatal("terminal replay relation absent")
	}
	conflict := bytes.ReplaceAll(terminal, []byte("end_turn"), []byte("provider_error"))
	state, batch, err = parseFileView(state, inputBytes(append(conflict, '\n')))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) == 0 || batch.Diagnostics[0].Code != "native_terminal_conflict" {
		t.Fatal("terminal identity conflict was not diagnosed")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeCompleted || len(turn.UsageObservations) != 1 {
			t.Fatal("conflicting event rewrote accepted outcome")
		}
	}
}

func TestGrokTerminalReplayIgnoresObjectFieldOrder(t *testing.T) {
	raw := []byte(`{"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn"},"_meta":{"eventId":"done1"}}}` + "\n" +
		`{"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"stop_reason":"end_turn","prompt_id":"p1","sessionUpdate":"turn_completed"},"_meta":{"eventId":"done1"}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) != 0 {
		t.Fatal("JSON field order caused a terminal conflict")
	}
	for _, turn := range state.Turns {
		if len(turn.Completion) != 1 || len(turn.Observations) != 2 {
			t.Fatal("terminal replay evidence lost")
		}
	}
}
func TestGrokLaterFailureRetractsDerivedFinality(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/paired-segments-tools-late-prompt/updates.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = parseFileView(state, inputBytes([]byte(`{"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"provider_error"},"_meta":{"eventId":"failure1"}}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeFailed || len(turn.FinalItemRefs) != 0 || turn.FinalItemID != "" {
			t.Fatal("failure retained the successful derived final")
		}
	}
	for _, item := range state.Items {
		if item.IsFinalAnswer {
			t.Fatal("failed prompt still has a derived final answer")
		}
	}
}

func TestGrokLateBindingSurvivesCheckpointAndUserReplay(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/paired-segments-tools-late-prompt/updates.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	var state parsedView
	for _, row := range bytes.SplitAfter(raw, []byte{'\n'}) {
		checkpoint, _ := json.Marshal(state)
		var prior parsedView
		if err = json.Unmarshal(checkpoint, &prior); err != nil {
			t.Fatal(err)
		}
		state, _, err = parseFileView(prior, inputBytes(row))
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(prior)
		if !bytes.Equal(after, checkpoint) {
			t.Fatal("binding mutated a prior checkpoint")
		}
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatal("bound checkpoint differs")
	}
	first := bytes.SplitAfter(raw, []byte{'\n'})[0]
	state, _, err = parseFileView(state, inputBytes(first))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Turns) != 1 {
		t.Fatal("unscoped replay created a new turn")
	}
	for _, item := range state.Items {
		if item.Type == model.ItemUserMessage && (item.Text != "Run a check." || len(item.Relations) != 1) {
			t.Fatal("bound user replay appended text again")
		}
	}
}
