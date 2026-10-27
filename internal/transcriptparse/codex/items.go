package codex

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func reasoningText(parts []model.Part) string {
	var texts []string
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func nativeItem(payload map[string]json.RawMessage, source model.SourceRef) model.Item {
	kind := rawString(payload, "type")
	key := strings.ToLower(kind)
	item := model.Item{Type: model.ItemProviderEvent, Role: model.RoleAssistant, Category: model.CategoryUnclassified, NativeItemID: rawString(payload, "id", "itemId"), ToolName: rawString(payload, "name", "tool"), CallID: rawString(payload, "callId", "call_id"), Phase: rawString(payload, "phase"), Detail: mustJSON(payload), SourcePayloadType: kind, SourceRowType: "wire"}
	switch key {
	case "usermessage":
		item.Type, item.Role, item.Category = model.ItemUserMessage, model.RoleUser, model.CategoryMessages
		item.Text = contentText(payload["content"])
		item.Parts = contentParts(payload["content"], source, "text")
	case "agentmessage":
		item.Type, item.Category = model.ItemAgentMessage, model.CategoryMessages
		item.Text = nativeText(payload, "text")
		item.Parts = []model.Part{{Type: "text", Text: item.Text, Availability: model.AvailableInline, Source: source}}
		if _, present := payload["text"]; !present {
			item.Text = contentText(payload["content"])
			item.Parts = contentParts(payload["content"], source, "text")
		}
	case "reasoning":
		item.Type, item.Category = model.ItemReasoning, model.CategoryReasoning
		summary, content := payload["summary"], payload["content"]
		if summary == nil {
			summary = payload["summary_text"]
		}
		if content == nil {
			content = payload["raw_content"]
		}
		item.Parts = contentParts(summary, source, "summary")
		item.Parts = append(item.Parts, contentParts(content, source, "reasoning")...)
		item.Text = reasoningText(item.Parts)
	case "commandexecution":
		item.Type, item.Category = model.ItemCommand, model.CategoryToolCall
		item.Text, _ = codexCommandDisplay(payload)
		item.Parts = []model.Part{{Type: "command", Text: item.Text, Availability: model.AvailableInline, Source: source}, {Type: "tool_output", Text: nativeText(payload, "aggregatedOutput", "aggregated_output"), Availability: model.AvailableInline, Source: source}}
	case "filechange":
		item.Type, item.Category = model.ItemFileChange, model.CategoryToolCall
		item.Parts = []model.Part{{Type: "changes", Detail: mustJSON(payload["changes"]), Availability: model.AvailableInline, Source: source}}
	case "mcptoolcall", "dynamictoolcall", "collabagenttoolcall", "toolsearchcall":
		item.Type, item.Category = model.ItemToolCall, model.CategoryToolCall
		item.Text = codexToolSummary(item.ToolName, payload)
		item.Parts = []model.Part{{Type: "tool", Detail: mustJSON(payload), Availability: model.AvailableInline, Source: source}}
	case "websearch":
		item.Type, item.Category, item.ToolName = model.ItemToolCall, model.CategoryToolCall, kind
		item.Text = nativeText(payload, "query")
		item.Parts = []model.Part{{Type: "tool", Detail: mustJSON(payload), Availability: model.AvailableInline, Source: source}}
	case "extension":
		if rawString(payload, "kind") == "web.search" {
			item.Type, item.Category, item.ToolName = model.ItemToolCall, model.CategoryToolCall, "web.search"
			item.Text = nativeText(payload, "query")
			item.Parts = []model.Part{{Type: "tool", Detail: mustJSON(payload), Availability: model.AvailableInline, Source: source}}
		}
	case "imageview":
		item.Type, item.Category, item.ToolName = model.ItemToolCall, model.CategoryToolCall, kind
		item.Text = nativeText(payload, "path")
		item.Parts = []model.Part{{Type: "image", Text: item.Text, Availability: model.AvailableReference, Source: source}}
	case "imagegeneration":
		item.Type, item.Category, item.ToolName = model.ItemToolCall, model.CategoryToolCall, kind
		// The string schema does not establish a byte encoding. Keep the result
		// opaque and a saved file path separate; the reader never fetches media.
		item.Parts = []model.Part{{Type: "image_result", Text: nativeText(payload, "result"), Availability: model.AvailableInline, Source: source}}
		if path := nativeText(payload, "savedPath"); path != "" {
			item.Parts = append(item.Parts, model.Part{Type: "image", Text: path, Availability: model.AvailableReference, Source: source})
		}
	case "enteredreviewmode", "exitedreviewmode", "hookprompt", "sleep", "subagentactivity":
		item.Type, item.Role, item.Category = model.ItemNotice, model.RoleSystem, model.CategorySystem
		item.Text = nativeText(payload, "review")
		if key == "hookprompt" {
			item.Parts = contentParts(payload["fragments"], source, "hook_fragment")
			item.Text = contentText(payload["fragments"])
		}
		if key == "subagentactivity" {
			item.Category, item.Text = model.CategorySubagent, codexSubagentSummary(payload)
		}
	case "plan":
		item.Type, item.Category = model.ItemPlan, model.CategorySystem
		item.Text = nativeText(payload, "text")
		item.Parts = []model.Part{{Type: "text", Text: item.Text, Availability: model.AvailableInline, Source: source}}
	case "contextcompaction":
		item.Type, item.Role, item.Category = model.ItemCompaction, model.RoleSystem, model.CategorySystem
		item.Text = "Conversation context compacted"
	}
	return item
}

func itemDiagnostic(item model.Item, payload map[string]json.RawMessage, source model.SourceRef) []model.Diagnostic {
	if item.Type != model.ItemProviderEvent {
		return nil
	}
	code := "unknown_item_type"
	if strings.EqualFold(rawString(payload, "type"), "extension") {
		code = "unknown_extension"
	}
	return []model.Diagnostic{diagnostic(code, source)}
}
