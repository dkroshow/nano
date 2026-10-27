package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexCapturedCompletedItemVariants(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind model.ItemType
		text string
	}{
		{"usermessage", model.ItemUserMessage, "[redacted]"},
		{"agentmessage", model.ItemAgentMessage, "[redacted]"},
		{"reasoning", model.ItemReasoning, "[redacted]\n\n[redacted]"},
		{"commandexecution", model.ItemCommand, "[redacted] [redacted] [redacted]"},
		{"filechange", model.ItemFileChange, ""},
		{"contextcompaction", model.ItemCompaction, "Conversation context compacted"},
		{"extension", model.ItemToolCall, "[redacted]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, "capture-item-completed-"+tc.name)))
			if err != nil || len(batch.Diagnostics) != 0 {
				t.Fatalf("parse: %v %+v", err, batch.Diagnostics)
			}
			items := orderedItems(state)
			if len(items) != 1 {
				t.Fatalf("items %d", len(items))
			}
			item := items[0]
			if item.Type != tc.kind || item.Text != tc.text || !item.Complete || item.NativeItemID == "" {
				t.Errorf("captured %s lost content or identity: %s %q", tc.name, item.Type, item.Text)
			}
			var native map[string]json.RawMessage
			if json.Unmarshal(item.Detail, &native) != nil || len(native["type"]) == 0 {
				t.Error("native completed payload lost")
			}
			if tc.name == "reasoning" && (len(item.Parts) != 2 || item.Parts[0].Index == nil || *item.Parts[1].Index != 1) {
				t.Error("native summary_text parts lost")
			}
			if tc.name == "commandexecution" && (len(item.Parts) < 2 || item.Parts[1].Text != "[redacted]" || item.Outcome != model.OutcomeCompleted) {
				t.Error("native argv/output/execution status lost")
			}
			if tc.name == "extension" && (item.ToolName != "web.search" || len(item.Parts) == 0 || !strings.Contains(string(item.Parts[0].Detail), "text_result")) {
				t.Error("known web extension lost structured results")
			}
			if tc.name == "agentmessage" && item.Finality.State != model.FinalityFinal {
				t.Error("native phase lost")
			}
			for _, turn := range state.Turns {
				if turn.Outcome != model.OutcomeUnknown {
					t.Error("completed item completed its parent turn")
				}
			}
		})
	}
}

func TestCodexWireItemTaxonomyRetainsNativeFacets(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind model.ItemType
		role model.Role
		text string
		tool string
	}{
		{"usermessage", model.ItemUserMessage, model.RoleUser, "", ""},
		{"agentmessage", model.ItemAgentMessage, model.RoleAssistant, "Example text.", ""},
		{"reasoning", model.ItemReasoning, model.RoleAssistant, "", ""},
		{"commandexecution", model.ItemCommand, model.RoleAssistant, "example", ""},
		{"filechange", model.ItemFileChange, model.RoleAssistant, "", ""},
		{"plan", model.ItemPlan, model.RoleAssistant, "Example text.", ""},
		{"mcptoolcall", model.ItemToolCall, model.RoleAssistant, "Tool call: example", "example"},
		{"dynamictoolcall", model.ItemToolCall, model.RoleAssistant, "Tool call: example", "example"},
		{"collabagenttoolcall", model.ItemToolCall, model.RoleAssistant, "Tool call: spawnAgent", "spawnAgent"},
		{"websearch", model.ItemToolCall, model.RoleAssistant, "example", "webSearch"},
		{"imageview", model.ItemToolCall, model.RoleAssistant, "/workspace/example", "imageView"},
		{"imagegeneration", model.ItemToolCall, model.RoleAssistant, "", "imageGeneration"},
		{"enteredreviewmode", model.ItemNotice, model.RoleSystem, "Example text.", ""},
		{"exitedreviewmode", model.ItemNotice, model.RoleSystem, "Example text.", ""},
		{"hookprompt", model.ItemNotice, model.RoleSystem, "", ""},
		{"sleep", model.ItemNotice, model.RoleSystem, "", ""},
		{"subagentactivity", model.ItemNotice, model.RoleSystem, "Sub-agent started", ""},
		{"contextcompaction", model.ItemCompaction, model.RoleSystem, "Conversation context compacted", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, "wire-item-"+tc.name)))
			if err != nil || len(batch.Diagnostics) != 0 {
				t.Fatalf("parse: %v %+v", err, batch.Diagnostics)
			}
			items := orderedItems(state)
			if len(items) != 1 {
				t.Fatalf("items %d", len(items))
			}
			item := items[0]
			if item.Type != tc.kind || item.Role != tc.role || item.Text != tc.text || item.ToolName != tc.tool || !item.Complete || item.NativeItemID != "item-example" || len(item.Detail) == 0 {
				t.Errorf("native facet lost: %s %s %q tool=%q", item.Type, item.Role, item.Text, item.ToolName)
			}
			if tc.name == "imageview" && (len(item.Parts) != 1 || item.Parts[0].Availability != model.AvailableReference || item.Parts[0].Text != "/workspace/example") {
				t.Error("image path claimed inline media")
			}
			if tc.name == "imagegeneration" && (len(item.Parts) != 1 || item.Parts[0].Type != "image_result" || item.Parts[0].Text != "example") {
				t.Error("opaque image result was lost or decoded without native encoding evidence")
			}
		})
	}
}

func TestCodexUnknownCompletedExtensionStaysInspectable(t *testing.T) {
	raw := []byte(`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Extension","kind":"future.tool","id":"item","extra":{"keep":true}}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Type != model.ItemProviderEvent || !strings.Contains(string(items[0].Detail), `"keep":true`) || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unknown_extension" {
		t.Fatal("unknown extension disappeared or claimed known semantics")
	}
}
