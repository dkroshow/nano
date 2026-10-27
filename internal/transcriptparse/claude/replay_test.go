package claude

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// After eviction the parser has no evidence that a UUID occurred earlier.
// History must resolve these two local keys through their exact native identity.
func TestClaudeEvictedReplayEmitsFreshNativeIdentity(t *testing.T) {
	first := `{"type":"assistant","uuid":"a1","apiBlockIndex":0,"message":{"id":"m1","content":[{"type":"text","text":"First"}],"usage":{"input_tokens":5},"stop_reason":"end_turn"}}`
	next := `{"type":"user","uuid":"u2","message":{"content":"Next prompt"}}`
	for _, wire := range []bool{false, true} {
		parse := parseFileView
		suffix := "\n"
		if wire {
			parse = parseWireView
			suffix = ""
		}
		var view parsedView
		for _, row := range []string{first, next} {
			var err error
			view, _, err = parse(view, inputFor([]byte(row+suffix)))
			if err != nil {
				t.Fatal(err)
			}
		}
		prior := orderedItems(view)[0]
		turnBefore, _ := json.Marshal(view.Turns[prior.TurnKey])
		if _, retained := view.Continuation.Items[prior.ID]; retained {
			t.Fatal("closed item remained in continuation")
		}
		saved, _ := json.Marshal(view)
		var restored parsedView
		if err := json.Unmarshal(saved, &restored); err != nil {
			t.Fatal(err)
		}
		after, batch, err := parse(restored, inputFor([]byte(first+suffix)))
		if err != nil {
			t.Fatal(err)
		}
		items := orderedItems(after)
		if len(items) != 3 || items[2].ID == prior.ID || items[2].NativeItemID != prior.NativeItemID || items[2].Text != prior.Text {
			t.Fatal("evicted identity was lost or old local key was reused")
		}
		if len(items[0].Observations) != 1 {
			t.Fatal("parser reopened a persisted item")
		}
		for _, change := range batch.Changes {
			if change.Relation != nil && change.Relation.Kind == model.RelationReplayOf {
				t.Fatal("parser claimed replay without retained evidence")
			}
		}
		turnAfter, _ := json.Marshal(after.Turns[prior.TurnKey])
		if !bytes.Equal(turnBefore, turnAfter) {
			t.Fatal("fresh observation rewrote settled turn or usage")
		}
		originalAfter, _ := json.Marshal(restored)
		if !bytes.Equal(saved, originalAfter) {
			t.Fatal("eviction mutated its input checkpoint")
		}
	}
}

func TestClaudeUUIDConflictDoesNotChangeUsage(t *testing.T) {
	rows := []byte(`{"type":"assistant","uuid":"a1","apiBlockIndex":0,"message":{"id":"m","content":[{"type":"text","text":"Same text"}],"usage":{"input_tokens":5}}}
{"type":"assistant","uuid":"a1","apiBlockIndex":0,"message":{"id":"m","content":[{"type":"text","text":"Same text"}],"usage":{"input_tokens":99}}}
`)
	state, batch, err := parseFileView(parsedView{}, inputFor(rows))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range batch.Diagnostics {
		if d.Code == "native_event_conflict" {
			found = true
		}
	}
	if !found {
		t.Fatal("conflicting UUID metadata accepted")
	}
	for _, turn := range state.Turns {
		if turn.Usage == nil || *turn.Usage.Input != 5 || len(turn.UsageObservations) != 1 {
			t.Fatal("conflicting event changed accepted usage")
		}
	}
}

func TestClaudeTerminalReplayKeepsSingleCompletion(t *testing.T) {
	row := []byte(`{"type":"result","uuid":"r","subtype":"success","usage":{"input_tokens":5}}`)
	state, _, err := parseWireView(parsedView{}, inputFor(row))
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(state)
	var restored parsedView
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	state, batch, err := parseWireView(restored, inputFor(row))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(saved, after) {
		t.Fatal("replay mutated prior checkpoint")
	}
	for _, turn := range state.Turns {
		if len(turn.Completion) != 1 || len(turn.UsageObservations) != 1 {
			t.Fatal("terminal replay repeated completion or usage")
		}
	}
	found := false
	for _, change := range batch.Changes {
		if change.Relation != nil && change.Relation.Kind == model.RelationReplayOf {
			found = true
		}
	}
	if !found {
		t.Fatal("terminal replay evidence absent")
	}
}

func TestClaudeResearchEqualTextWithDistinctUUIDs(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "duplicate-row-versus-equal-text")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || items[0].Text != "Again" || items[1].Text != "Again" || len(items[0].Observations) != 2 || len(items[1].Observations) != 1 {
		t.Fatal("equal text erased real native block or repeated UUID added content")
	}
}
