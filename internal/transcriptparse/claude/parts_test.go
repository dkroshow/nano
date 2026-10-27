package claude

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeMixedMediaKeepsTypedUserParts(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "mixed-content-and-images")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 4 || len(state.Turns) != 1 {
		t.Fatal("mixed native parts lost")
	}
	for i, kind := range []string{"text", "text", "image", "document"} {
		if items[i].Type != model.ItemUserMessage || items[i].Role != model.RoleUser || len(items[i].Parts) != 1 || items[i].Parts[0].Type != kind || *items[i].Parts[0].Index != i || len(items[i].Parts[0].Detail) == 0 {
			t.Fatalf("part %d lost type/role/native content", i)
		}
	}
}

func TestClaudeMalformedContentKeepsNativePositions(t *testing.T) {
	raw := []byte(`{"type":"user","uuid":"u","message":{"content":["A",42,{"type":"text","text":"B"},null,""]}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 5 {
		t.Fatalf("got %d parts; malformed or empty entries vanished", len(items))
	}
	for i, item := range items {
		if len(item.Parts) != 1 || *item.Parts[0].Index != i || item.Parts[0].Source.BlockPath != fmt.Sprintf("$.message.content[%d]", i) {
			t.Fatal("native block positions shifted")
		}
	}
	for _, i := range []int{1, 3} {
		var detail map[string]json.RawMessage
		if json.Unmarshal(items[i].Parts[0].Detail, &detail) != nil || len(detail["nativeValue"]) == 0 || items[i].Type != model.ItemProviderEvent {
			t.Fatal("malformed native value not inspectable")
		}
	}
	malformed := 0
	for _, d := range batch.Diagnostics {
		if d.Code == "invalid_content_block" {
			malformed++
		}
	}
	if malformed != 2 {
		t.Fatalf("malformed diagnostics=%d", malformed)
	}
	if items[4].Type != model.ItemUserMessage || items[4].Text != "" {
		t.Fatal("empty native string not retained as text")
	}
}

func TestClaudeImageOnlyUserStartsPrompt(t *testing.T) {
	raw := []byte(`{"type":"user","uuid":"u","message":{"content":[{"type":"image","source":{"type":"url","url":"https://example.invalid/image.png"}}]}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Type != model.ItemUserMessage || items[0].Parts[0].Availability != model.AvailableReference || len(state.Turns) != 1 {
		t.Fatal("image prompt lost its user role/reference")
	}
}

func TestClaudeWhitespaceDeltaAndBareSourcePath(t *testing.T) {
	var state parsedView
	for _, row := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"A"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"B"}}}`,
	} {
		var err error
		state, _, err = parseWireView(state, inputFor([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "A B" {
		t.Fatal("whitespace-only chunk disappeared")
	}
	file, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "bare-string-message")))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range file.Items {
		if item.Parts[0].Source.BlockPath != "$.message.content" {
			t.Fatal("bare string given a nonexistent native array index")
		}
	}
}
