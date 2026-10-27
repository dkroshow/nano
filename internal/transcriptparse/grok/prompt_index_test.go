package grok

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestNativePromptIndicesSeparateConsecutivePrompts(t *testing.T) {
	raw, err := os.ReadFile("testdata/indexed-prompts/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"file", "wire"} {
		t.Run(mode, func(t *testing.T) {
			var state parsedView
			// Serializing at each boundary also checks that the archived unknown turn
			// remains available when a later replay carries only its event identity.
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
				input := inputBytes(line)
				if mode == "wire" {
					state, _, err = parseWireView(state, input)
				} else {
					input.Bytes = append(append([]byte(nil), line...), '\n')
					state, _, err = parseFileView(state, input)
				}
				if err != nil {
					t.Fatal(err)
				}
				saved, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(saved, &state); err != nil {
					t.Fatal(err)
				}
			}
			items := make([]model.Item, 0, len(state.Items))
			for _, item := range state.Items {
				items = append(items, item)
			}
			sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
			if len(items) != 3 || len(state.Turns) != 2 {
				t.Fatalf("distinct indexed prompts collapsed: %d items, %d turns", len(items), len(state.Turns))
			}
			if items[0].Text != "First prompt." || items[1].Text != "Second prompt." || items[2].Text != "Second answer." {
				t.Fatalf("prompt content changed: %q / %q / %q", items[0].Text, items[1].Text, items[2].Text)
			}
			if items[0].TurnKey == items[1].TurnKey || items[1].TurnKey != items[2].TurnKey {
				t.Fatal("native prompt boundary lost")
			}
			if items[0].Scope.NativeTurnID != nil || items[1].Scope.NativeTurnID == nil || *items[1].Scope.NativeTurnID != "p2" {
				t.Fatal("late native prompt claimed the wrong user")
			}
			if len(items[0].Observations) != 3 || len(items[0].Relations) != 1 || items[0].Relations[0].Kind != model.RelationReplayOf {
				t.Fatal("first indexed prompt replay changed content or lost evidence")
			}
		})
	}
}
