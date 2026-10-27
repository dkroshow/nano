package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func parseFileCase(t *testing.T, name string) (parsedView, model.Batch) {
	t.Helper()
	raw, err := os.ReadFile("testdata/research/" + name + "/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	input := inputFor(raw)
	input.Closed = true
	state, batch, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	return state, batch
}

func TestCodexFileOriginKeepsNativeContent(t *testing.T) {
	state, _ := parseFileCase(t, "edge-task-notification-and-abort-wrapper")
	items := orderedItems(state)
	if len(items) != 2 || !strings.Contains(items[0].Text, "Example worker result.") || !strings.Contains(items[1].Text, "Previous action interrupted.") {
		t.Fatal("origin classification erased native system content")
	}
	for _, item := range items {
		if item.Type != model.ItemNotice || !item.Synthetic || item.Role != model.RoleSystem {
			t.Fatal("injected content became a human prompt")
		}
	}
	state, batch := parseFileCase(t, "edge-image-only-and-unknown-content")
	items = orderedItems(state)
	if len(items) != 2 || items[0].Type != model.ItemUserMessage || items[0].Synthetic || items[0].Parts[0].Type != "input_image" || items[0].Parts[0].Availability != model.AvailableReference {
		t.Fatal("image-only prompt lost native user origin or media reference")
	}
	if items[1].Parts[0].Type != "future_media" || !strings.Contains(string(items[1].Parts[0].Detail), "opaque-example") || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unknown_content_part" {
		t.Fatal("unknown content lacks inspectable native detail and diagnosis")
	}
}

func TestCodexFileAgentCommunicationKeepsSidechainClassification(t *testing.T) {
	for _, name := range []string{"file-response-item-agent-message", "capture-agent-message"} {
		state, _ := parseFileCase(t, name)
		items := orderedItems(state)
		if len(items) != 1 || items[0].Type != model.ItemAgentMessage || !items[0].Sidechain || len(items[0].FilterTags) != 1 || items[0].FilterTags[0] != "collaboration" {
			t.Fatalf("%s lost native inter-agent classification", name)
		}
	}
	state, _ := parseFileCase(t, "file-event-msg-agent-message")
	items := orderedItems(state)
	if len(items) != 1 || items[0].Sidechain || len(items[0].FilterTags) != 0 {
		t.Fatal("ordinary agent output became inter-agent communication")
	}
}

func TestCodexFileCompactionAndOpaqueImageVariants(t *testing.T) {
	for _, name := range []string{"capture-compaction", "file-compacted"} {
		state, batch := parseFileCase(t, name)
		items := orderedItems(state)
		if len(items) != 1 || items[0].Type != model.ItemCompaction || items[0].Text != "[redacted]" || !strings.Contains(string(items[0].Detail), "replacement_history") || len(batch.Diagnostics) != 0 {
			t.Fatalf("%s erased compaction text or replayed replacement history", name)
		}
	}
	for _, name := range []string{"schema-response-item-compaction", "schema-response-item-context-compaction", "schema-response-item-image-generation-call"} {
		state, batch := parseFileCase(t, name)
		items := orderedItems(state)
		if len(items) != 1 || len(batch.Diagnostics) != 0 || items[0].Outcome != model.OutcomeUnknown {
			t.Fatalf("%s lost known variant or invented execution", name)
		}
		if name == "schema-response-item-image-generation-call" {
			if items[0].Type != model.ItemToolCall || len(items[0].Parts) != 1 || items[0].Parts[0].Text != "example" {
				t.Fatal("opaque image result was decoded or dropped")
			}
		} else if items[0].Type != model.ItemCompaction || items[0].Text != "" {
			t.Fatal("compaction invented readable content")
		}
	}
}
