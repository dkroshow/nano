package codex

import (
	"encoding/json"
	"testing"
)

func TestCodexWhitespaceDeltasRemainContent(t *testing.T) {
	for _, method := range []string{"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/commandExecution/outputDelta"} {
		t.Run(method, func(t *testing.T) {
			var state parsedView
			for _, delta := range []string{"A", " ", "B"} {
				raw, _ := json.Marshal(map[string]any{"method": method, "params": map[string]any{"threadId": "th", "turnId": "t", "itemId": "a", "summaryIndex": 0, "contentIndex": 0, "delta": delta}})
				var err error
				state, _, err = parseWireView(state, inputFor(raw))
				if err != nil {
					t.Fatal(err)
				}
			}
			items := orderedItems(state)
			if len(items) != 1 {
				t.Fatalf("items=%d", len(items))
			}
			text := items[0].Text
			if method == "item/commandExecution/outputDelta" {
				for _, part := range items[0].Parts {
					if part.Type == "tool_output" {
						text = part.Text
					}
				}
			}
			if text != "A B" {
				t.Fatalf("whitespace delta lost: %q", text)
			}
		})
	}
}

func TestCodexWhitespaceContentPartKeepsNativeText(t *testing.T) {
	raw := []byte(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A"},{"type":"output_text","text":" "},{"type":"output_text","text":"B"}]}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || len(items[0].Parts) != 3 || items[0].Parts[1].Text != " " || items[0].Text != "A\n \nB" {
		t.Fatal("whitespace-only native part erased")
	}
}
