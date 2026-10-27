package codex

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexDerivedPromptActivatesItsOwnTurn(t *testing.T) {
	var state parsedView
	for number := 1; number <= 3; number++ {
		for _, raw := range []string{
			fmt.Sprintf(`{"type":"event_msg","payload":{"type":"user_message","message":"Prompt %d"}}`, number),
			fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer %d"}]}}`, number),
			`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		} {
			saved, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var restored parsedView
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			state, _, err = parseFileView(restored, model.Input{Bytes: []byte(raw + "\n"), Source: model.SourceRef{Label: "derived-turns", Incarnation: "1"}})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for number := 1; number <= 3; number++ {
		var prompt, answer model.Item
		for _, item := range state.Items {
			if item.Text == fmt.Sprintf("Prompt %d", number) {
				prompt = item
			}
			if item.Text == fmt.Sprintf("Answer %d", number) {
				answer = item
			}
		}
		if prompt.ID == "" || answer.ID == "" || prompt.TurnKey != answer.TurnKey || prompt.TurnOrdinal != number {
			t.Fatalf("prompt %d and its answer do not share their own turn", number)
		}
		turn := state.Turns[prompt.TurnKey]
		if turn.Outcome != model.OutcomeCompleted || turn.NativeTurnID != "" || turn.FinalItemID != answer.ID {
			t.Fatalf("derived turn %d lost its terminal or final answer", number)
		}
	}
}

func TestCodexFileSessionMemoryStaysOnItsConnection(t *testing.T) {
	var state parsedView
	parse := func(connection, raw string, caller *string) model.Batch {
		t.Helper()
		checkpoint, _ := json.Marshal(state)
		var restored parsedView
		if err := json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		input := model.Input{Bytes: []byte(raw + "\n"), Source: model.SourceRef{Label: "fixture", Incarnation: "1"}, Scope: model.Scope{ConnectionID: connection, NativeThreadID: caller}}
		next, batch, err := parseFileView(restored, input)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(restored)
		if string(after) != string(checkpoint) {
			t.Fatal("session memory mutated the prior checkpoint")
		}
		state = next
		return batch
	}
	caller := "caller"
	batch := parse("a", `{"type":"session_meta","payload":{"id":"thread-a"}}`, &caller)
	if len(envelopeObservations(batch)) != 1 || envelopeObservations(batch)[0].Scope.NativeThreadID == nil || *envelopeObservations(batch)[0].Scope.NativeThreadID != "thread-a" {
		t.Error("explicit native session metadata lost its own identity")
	}
	parse("b", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"unbound"}]}}`, nil)
	parse("a", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"remembered"}]}}`, nil)
	parse("a", `{"type":"session_meta","payload":{}}`, nil)
	parse("a", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after empty metadata"}]}}`, nil)
	parse("a", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"caller supplied"}]}}`, &caller)
	for _, item := range state.Items {
		want := "thread-a"
		switch item.Text {
		case "unbound":
			if item.Scope.NativeThreadID != nil {
				t.Error("a new connection inherited another connection's session")
			}
			continue
		case "caller supplied":
			want = caller
		}
		if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != want {
			t.Errorf("%s has wrong native session", item.Text)
		}
	}
}

func TestCodexWireThreadMetadataDoesNotBindMultiplexedTraffic(t *testing.T) {
	input := model.Input{Source: model.SourceRef{Label: "wire", Incarnation: "1"}, Scope: model.Scope{ConnectionID: "connection"}}
	input.Bytes = []byte(`{"method":"thread/started","params":{"thread":{"id":"child","parentThreadId":"parent"}}}`)
	state, batch, err := parseWireView(parsedView{}, input)
	if err != nil || len(envelopeObservations(batch)) != 1 {
		t.Fatalf("thread metadata: %v, %+v", err, batch)
	}
	obs := envelopeObservations(batch)[0]
	if obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != "child" || obs.Scope.ParentNativeID == nil || *obs.Scope.ParentNativeID != "parent" {
		t.Error("thread metadata does not carry its explicit native scope")
	}
	if len(state.Turns) != 0 {
		t.Error("thread start created a turn")
	}
	input.Bytes = []byte(`{"method":"item/started","params":{"item":{"type":"agentMessage","id":"item","text":"unbound"}}}`)
	state, _, err = parseWireView(state, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Scope.NativeThreadID != nil {
			t.Error("thread notification claimed later unscoped traffic on a multiplexed connection")
		}
	}
}

// Without native turn ids, a prompt after an interrupted turn starts the next
// turn only because the reader still holds the interrupted turn's prompt
// (file.go). Retention evicted it and the second prompt joined turn 1.
func TestCodexSettledPromptWithoutNativeTurnIDStartsNextTurn(t *testing.T) {
	rows := []string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"First prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"u1","role":"user","content":"First prompt"}}`,
		`{"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Second prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"u2","role":"user","content":"Second prompt"}}`,
	}
	var state parsedView
	for _, row := range rows {
		saved, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		var restored parsedView
		if err := json.Unmarshal(saved, &restored); err != nil {
			t.Fatal(err)
		}
		var batch model.Batch
		state, batch, err = parseFileView(restored, model.Input{Bytes: []byte(row + "\n"), Source: model.SourceRef{Label: "prompts", Incarnation: "1"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range batch.Changes {
			if change.Item != nil && change.Item.Text == "Second prompt" && change.Item.TurnOrdinal != 2 {
				t.Fatalf("second prompt belongs to turn %d, want 2", change.Item.TurnOrdinal)
			}
		}
	}
}
