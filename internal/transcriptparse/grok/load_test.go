package grok

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokLoadRunningPromptSurvivesStraddlingReplay(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"id":2,"method":"session/load","params":{"sessionId":"s1"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range bytes.Split(bytes.TrimSpace(fixtureBytes(t, "replay-straddles-load")), []byte{'\n'}) {
		before, _ := json.Marshal(state)
		var restored parsedView
		if err := json.Unmarshal(before, &restored); err != nil {
			t.Fatal(err)
		}
		var batch model.Batch
		state, batch, err = parseWireView(restored, inputBytes(row))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("load/replay: %v %+v", err, batch.Diagnostics)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(before, after) {
			t.Error("load changed prior checkpoint")
		}
	}
	if len(state.Continuation.RunningPrompts) != 1 {
		t.Fatal("load response lost running prompt evidence")
	}
	for _, running := range state.Continuation.RunningPrompts {
		if running.Scope.NativeTurnID == nil || *running.Scope.NativeTurnID != "p2" || running.Scope.NativeThreadID == nil || *running.Scope.NativeThreadID != "s1" {
			t.Error("replay claimed the running prompt")
		}
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown || len(turn.Completion) != 0 {
			t.Error("load invented native completion")
		}
	}
}

func TestGrokLoadWithoutRunningPromptClearsOnlyBinding(t *testing.T) {
	rows := []string{
		`{"method":"_x.ai/queue/changed","params":{"sessionId":"s1","runningPromptId":"p1","runningText":"ask"}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"unfinished"}},"_meta":{"promptId":"p1","eventId":"e1"}}}`,
		`{"id":2,"method":"session/load","params":{"sessionId":"s1"}}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	state, batch, err := parseWireView(state, inputBytes(fixtureBytes(t, "load-without-running-prompt")))
	if err != nil || len(batch.Diagnostics) != 0 || len(state.Continuation.RunningPrompts) != 0 || len(state.Turns) != 1 {
		t.Fatalf("empty load state: %v %+v", err, batch.Diagnostics)
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown || len(turn.Completion) != 0 || turn.CompletedAt != nil {
			t.Error("missing running prompt invented interruption or native end time")
		}
	}
}

func TestGrokRepeatedLoadReplyCannotOverwriteNewQueueState(t *testing.T) {
	rows := []string{
		`{"id":2,"method":"session/load","params":{"sessionId":"s1"}}`,
		`{"id":2,"result":{"_meta":{"x.ai/runningPromptId":"p2"}}}`,
		`{"method":"_x.ai/queue/changed","params":{"sessionId":"s1","runningPromptId":"p3","runningText":"new"}}`,
		`{"result":{"_meta":{"x.ai/runningPromptId":"p2"}},"id":2}`,
	}
	var state parsedView
	var last model.Batch
	for _, row := range rows {
		var err error
		state, last, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil || len(last.Diagnostics) != 0 {
			t.Fatalf("load replay: %v %+v", err, last.Diagnostics)
		}
	}
	for _, running := range state.Continuation.RunningPrompts {
		if running.Scope.NativeTurnID == nil || *running.Scope.NativeTurnID != "p3" {
			t.Error("old load reply erased newer queue state")
		}
	}
	found := false
	for _, change := range last.Changes {
		if change.Relation != nil && change.Relation.Kind == model.RelationReplayOf {
			found = true
		}
	}
	if !found {
		t.Error("load reply replay lost evidence")
	}
}

func TestGrokLoadWithUnknownTextCannotBindAnEmptyUserChunk(t *testing.T) {
	rows := []string{
		`{"id":2,"method":"session/load","params":{"sessionId":"s1"}}`,
		`{"id":2,"result":{"_meta":{"x.ai/runningPromptId":"p2"}}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":""}},"_meta":{"eventId":"empty"}}}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range state.Items {
		if item.Scope.NativeTurnID != nil {
			t.Error("absent load text became exact prompt text evidence")
		}
	}
}
