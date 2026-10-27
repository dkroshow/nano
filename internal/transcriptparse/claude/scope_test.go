package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeParentToolScope(t *testing.T) {
	raw, err := os.ReadFile("testdata/parent-tool-scope/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var state parsedView
	for _, row := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		saved, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var restored parsedView
		if err := json.Unmarshal(saved, &restored); err != nil {
			t.Fatal(err)
		}
		next, batch, err := parseWireView(restored, inputFor(row))
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Diagnostics) != 0 {
			t.Fatalf("diagnostics: %+v", batch.Diagnostics)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(saved, after) {
			t.Fatal("prior checkpoint mutated")
		}
		state = next
	}
	items := orderedItems(state)
	if len(items) != 2 || items[0].Text != "Parent tail" || items[1].Text != "Child" {
		t.Fatalf("child contaminated parent content: got %d items", len(items))
	}
	parent, child := state.Turns[items[0].TurnKey], state.Turns[items[1].TurnKey]
	if len(state.Turns) != 2 || parent.Key == child.Key || parent.Outcome != model.OutcomeUnknown || child.Outcome != model.OutcomeCompleted {
		t.Fatalf("child result settled parent: parent=%+v child=%+v", parent, child)
	}
	if parent.Usage == nil || parent.Usage.Input == nil || *parent.Usage.Input != 7 || child.Usage == nil || child.Usage.Input == nil || *child.Usage.Input != 3 {
		t.Fatalf("usage crossed scope: parent=%+v child=%+v", parent.Usage, child.Usage)
	}
	if len(parent.FinalItemRefs) != 0 || len(child.FinalItemRefs) != 1 || child.FinalItemRefs[0] != items[1].ID || child.Scope.NativeTurnID != nil {
		t.Fatal("child finality or native turn identity crossed scope")
	}
	if parent.Scope.NativeParentCallID != nil || child.Scope.NativeParentCallID == nil || *child.Scope.NativeParentCallID != "child-call" {
		t.Fatal("native parent call evidence lost")
	}
}

func TestClaudeUnidentifiedSidechainCannotChangeParent(t *testing.T) {
	for _, reader := range []string{"file", "wire"} {
		t.Run(reader, func(t *testing.T) {
			var state parsedView
			rows := []string{
				`{"type":"user","message":{"content":"Parent prompt"}}`,
				`{"type":"assistant","message":{"id":"parent-response","content":"Parent answer","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}`,
				`{"type":"assistant","uuid":"unbound-child-event","isSidechain":true,"message":{"id":"child-response","content":"Child answer","stop_reason":"end_turn","usage":{"input_tokens":90,"output_tokens":80}}}`,
			}
			var last model.Batch
			for _, row := range rows {
				saved, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				var restored parsedView
				if err := json.Unmarshal(saved, &restored); err != nil {
					t.Fatal(err)
				}
				input := inputFor([]byte(row))
				if reader == "file" {
					input.Bytes = append(input.Bytes, '\n')
					state, last, err = parseFileView(restored, input)
				} else {
					state, last, err = parseWireView(restored, input)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			parentFound := false
			for _, item := range state.Items {
				if item.Text != "Parent answer" {
					continue
				}
				parentFound = true
				turn := state.Turns[item.TurnKey]
				if turn.Usage == nil || turn.Usage.Input == nil || *turn.Usage.Input != 2 || turn.Usage.Output == nil || *turn.Usage.Output != 3 {
					t.Fatal("unidentified child changed parent usage")
				}
				if len(turn.FinalItemRefs) != 1 || turn.FinalItemRefs[0] != item.ID {
					t.Fatal("unidentified child changed parent final answer")
				}
			}
			if !parentFound {
				t.Fatal("parent answer was discarded")
			}
			if len(last.Diagnostics) != 1 || last.Diagnostics[0].Code != "unbound_sidechain_scope" {
				t.Fatal("missing child scope was not diagnosed")
			}
			retained := false
			for _, observation := range last.Observations {
				retained = retained || observation.NativeEventID == "unbound-child-event" && observation.Scope.NativeThreadID == nil && bytes.Contains(observation.Detail, []byte("child-response"))
			}
			if !retained {
				t.Fatal("unidentified child evidence was discarded")
			}
		})
	}
}

func TestClaudeMalformedParentCallDoesNotTargetParent(t *testing.T) {
	rows := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Parent"}}}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputFor([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	row := []byte(`{"type":"stream_event","parent_tool_use_id":42,"event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"wrong"}}}`)
	state, batch, err := parseWireView(state, inputFor(row))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "Parent" || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "invalid_parent_call_scope" {
		t.Fatal("malformed native scope targeted parent content")
	}
}

func TestClaudeWorkerFileKeepsParentSeparate(t *testing.T) {
	prefix := []byte(`{"type":"user","uuid":"parent-u","sessionId":"session-example","message":{"content":"Parent question"}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputFor(append(prefix, fixture(t, "worker-file-and-parent-scope")...)))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 3 || !items[1].Sidechain || !items[2].Sidechain || items[1].Scope.NativeThreadID == nil || *items[1].Scope.NativeThreadID != "worker-example" || items[1].Scope.ParentNativeID == nil || *items[1].Scope.ParentNativeID != "session-example" {
		t.Fatalf("worker scope lost: %+v", items)
	}
	parent, worker := state.Turns[items[0].TurnKey], state.Turns[items[1].TurnKey]
	if parent.Key == worker.Key || parent.Usage != nil || len(parent.Completion) != 0 || worker.Usage == nil || *worker.Usage.Input != 2 {
		t.Fatal("worker completion/usage contaminated parent")
	}
	for _, item := range items {
		for _, obs := range item.Observations {
			if !reflect.DeepEqual(obs.Scope, item.Scope) {
				t.Fatal("observation scope mismatch")
			}
		}
	}
}

func TestClaudeFirstNativeSessionBindsSourceLocalPrompt(t *testing.T) {
	var state parsedView
	var firstID string
	for i, row := range bytes.Split(bytes.TrimSpace(fixture(t, "paired-split-wire")), []byte{'\n'}) {
		input := inputFor(row)
		input.Scope.NativeThreadID = nil
		var err error
		state, _, err = parseWireView(state, input)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			items := orderedItems(state)
			if len(items) != 1 || items[0].Scope.NativeThreadID != nil {
				t.Fatal("first prompt should have unknown native scope")
			}
			firstID = items[0].ID
		}
	}
	items := orderedItems(state)
	if len(items) != 3 || len(state.Turns) != 1 || items[0].ID != firstID {
		t.Fatal("native session arrival split source-local turn or changed item identity")
	}
	for _, item := range items {
		if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != "session-example" {
			t.Fatal("native session not bound")
		}
	}
	if items[0].Observations[0].Scope.NativeThreadID != nil {
		t.Fatal("binding rewrote original unknown observation")
	}
}

func TestClaudeBindingKeepsEarlierUsageAndReplay(t *testing.T) {
	first := `{"type":"assistant","uuid":"a1","message":{"id":"m1","content":[{"type":"text","text":"Early"}],"usage":{"input_tokens":2}}}`
	rows := []string{first, `{"type":"system","subtype":"init","session_id":"session-example"}`, first, `{"type":"assistant","uuid":"a2","message":{"id":"m2","content":[{"type":"text","text":"Later"}],"usage":{"input_tokens":3}}}`}
	var state parsedView
	for _, row := range rows {
		input := inputFor([]byte(row + "\n"))
		input.Scope.NativeThreadID = nil
		var err error
		state, _, err = parseFileView(state, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	items := orderedItems(state)
	if len(items) != 2 || len(state.Turns) != 1 || len(items[0].Relations) != 1 || items[0].Relations[0].Kind != model.RelationReplayOf {
		t.Fatal("native binding lost UUID replay evidence")
	}
	for _, turn := range state.Turns {
		if turn.Usage == nil || *turn.Usage.Input != 5 {
			t.Fatal("binding discarded earlier scoped usage")
		}
	}
}

func TestClaudeBindingEmitsEveryItemAndPreservesCheckpoint(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"a","message":{"id":"m","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}}` + "\n")
	input := inputFor(raw)
	input.Scope.NativeThreadID = nil
	state, _, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(state)
	var restored parsedView
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	input.Bytes = []byte(`{"type":"system","subtype":"init","session_id":"session-example"}` + "\n")
	_, batch, err := parseFileView(restored, input)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(saved, after) {
		t.Fatal("native binding mutated prior checkpoint")
	}
	ids := map[string]bool{}
	for _, change := range batch.Changes {
		if change.Item != nil {
			ids[change.Item.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatal("native binding failed to emit every item revision")
	}
}

func TestClaudeRememberedSessionStaysWithinConnection(t *testing.T) {
	input := inputFor([]byte(`{"type":"system","subtype":"init","session_id":"native-a"}`))
	input.Scope.NativeThreadID, input.Scope.ConnectionID = nil, "connection-a"
	state, _, err := parseWireView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Scope.ConnectionID = "connection-b"
	input.Bytes = []byte(`{"type":"user","message":{"content":"Unbound"}}`)
	state, _, err = parseWireView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Scope.NativeThreadID != nil {
		t.Fatal("another connection supplied native session identity")
	}
	input.Scope.ConnectionID = "connection-a"
	input.Bytes = []byte(`{"type":"user","message":{"content":"Bound"}}`)
	state, _, err = parseWireView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	items = orderedItems(state)
	if len(items) != 2 || items[1].Scope.NativeThreadID == nil || *items[1].Scope.NativeThreadID != "native-a" {
		t.Fatal("original connection forgot its native session")
	}
}

func TestClaudeNestedSessionDoesNotReplaceRootMemory(t *testing.T) {
	var state parsedView
	input := inputFor(nil)
	input.Scope.NativeThreadID = nil
	for _, row := range []string{
		`{"type":"system","subtype":"init","session_id":"root"}`,
		`{"type":"system","subtype":"init","session_id":"child","parent_tool_use_id":"call"}`,
		`{"type":"user","message":{"content":"Root prompt"}}`,
	} {
		input.Bytes = []byte(row)
		var err error
		state, _, err = parseWireView(state, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	item := orderedItems(state)[0]
	if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != "root" {
		t.Fatal("nested native session relabelled root content")
	}
	provided := "caller-native"
	input.Scope.NativeThreadID = &provided
	input.Bytes = []byte(`{"type":"user","message":{"content":"Explicit scope"}}`)
	state, _, err := parseWireView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	item = orderedItems(state)[1]
	if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != provided {
		t.Fatal("remembered session overrode caller native scope")
	}
}
