package sessions

import (
	"encoding/json"
	"time"

	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type codexLiveAdapter struct {
	reader codex.State
}

func newCodexLiveAdapter() *codexLiveAdapter {
	return &codexLiveAdapter{}
}

func (a *codexLiveAdapter) apply(dir protocol.Direction, line []byte, at time.Time) model.Batch {
	next, batch, err := codex.ParseWire(a.reader, liveParseInput(dir, line, at))
	if err != nil {
		return model.Batch{}
	}
	a.reader = next
	return batch
}

func liveParseInput(dir protocol.Direction, line []byte, at time.Time) model.Input {
	return model.Input{Bytes: line, Source: model.SourceRef{Kind: "wire", Label: "live", Incarnation: "adapter", Direction: string(dir)}, ObservedAt: &at}
}

// The current browser DTO keeps its native card type and bounded native detail.
// Text, identity, completion and outcome come from the module's typed item.
func parsedLiveItem(parsed model.Item, at time.Time) *liveProjectionItem {
	id := threadvalues.FirstNonEmpty(parsed.NativeItemID, parsed.ID)
	types := map[model.ItemType]string{
		model.ItemUserMessage: "userMessage", model.ItemAgentMessage: "agentMessage", model.ItemReasoning: "reasoning",
		model.ItemCommand: "commandExecution", model.ItemToolCall: "toolUse", model.ItemToolResult: "toolResult",
		model.ItemFileChange: "fileChange", model.ItemPlan: "plan", model.ItemCompaction: "contextCompaction",
		model.ItemNotice: "providerEvent", model.ItemProviderEvent: "providerEvent",
	}
	detail := make(map[string]any)
	_ = json.Unmarshal(parsed.Detail, &detail)
	if detail == nil {
		detail = make(map[string]any)
	}
	typeName := types[parsed.Type]
	// File records use message/UserMessage/AgentMessage; the browser's message
	// channel requires canonical names. Native tool details remain unchanged.
	if parsed.Scope.Provider == model.ProviderCodex && parsed.Type != model.ItemUserMessage && parsed.Type != model.ItemAgentMessage {
		if native, ok := detail["type"].(string); ok && native != "" {
			typeName = native
		}
	}
	if parsed.CreatedAt != nil {
		at = *parsed.CreatedAt
	}
	item := newLiveItem(id, typeName, at)
	item.Item = detail
	item.Text, item.Role, item.Category = parsed.Text, string(parsed.Role), string(parsed.Category)
	item.NativeItemID, item.Phase = parsed.NativeItemID, parsed.Phase
	item.ToolName, item.CallID, item.Done = parsed.ToolName, parsed.CallID, parsed.Complete
	item.ContentRef, item.ContentTruncated, item.OriginalBytes = parsed.ContentRef, parsed.ContentTruncated, int(parsed.OriginalBytes)
	if parsed.Scope.NativeTurnID != nil {
		item.TurnID = *parsed.Scope.NativeTurnID
	}
	if parsed.Status != "" && parsed.Status != model.ItemStatusUnknown {
		item.Status = string(parsed.Status)
	} else if item.Done {
		item.Status = "completed"
	}
	item.Item["type"], item.Item["text"] = typeName, item.Text
	for _, part := range parsed.Parts {
		if part.Type == "tool_output" {
			item.Item["aggregatedOutput"] = part.Text
		}
	}
	return item
}
