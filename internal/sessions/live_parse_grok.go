package sessions

import (
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type grokLiveAdapter struct {
	reader grok.State
}

func newGrokLiveAdapter() *grokLiveAdapter {
	return &grokLiveAdapter{}
}

func (a *grokLiveAdapter) apply(dir protocol.Direction, line []byte, at time.Time) model.Batch {
	next, batch, err := grok.ParseWire(a.reader, liveParseInput(dir, line, at))
	if err != nil {
		return model.Batch{}
	}
	a.reader = next
	return batch
}

func parsedGrokLiveItem(parsed model.Item, at time.Time) *liveProjectionItem {
	item := parsedLiveItem(parsed, at)
	if parsed.Type == model.ItemUserMessage {
		item.Done = true
		// The legacy browser field also carries event identity for user echoes.
		// It does not add a native item ID to the module.
		for _, observation := range parsed.Observations {
			if observation.NativeEventID != "" {
				item.NativeItemID = observation.NativeEventID
				break
			}
		}
	}
	if parsed.IsFinalAnswer {
		item.Phase = "final_answer"
	}
	if parsed.Synthetic && parsed.Type == model.ItemAgentMessage {
		item.Done = true
	}
	if parsed.Type == model.ItemToolCall || parsed.Type == model.ItemToolResult {
		item.Type = "toolUse"
		item.Item["type"], item.Item["name"], item.Item["toolCallId"] = item.Type, item.ToolName, item.CallID
		item.Item["status"] = string(parsed.Outcome)
		if input, ok := item.Item["rawInput"]; ok {
			item.Item["input"] = input
		}
		var output []string
		for _, part := range parsed.Parts {
			if part.Text != "" {
				output = append(output, part.Text)
			}
		}
		if len(output) != 0 {
			item.Item["output"] = strings.Join(output, "\n")
		}
	}
	if parsed.Category == model.CategorySubagent {
		item.Type, item.Done = "subAgentActivity", true
		item.Item["type"] = item.Type
		for native, view := range map[string]string{
			"child_session_id": "childSessionId", "parent_session_id": "parentSessionId",
			"duration_ms": "durationMs", "tool_calls": "toolCalls", "tokens_used": "tokensUsed",
		} {
			if value, ok := item.Item[native]; ok {
				item.Item[view] = value
			}
		}
		if item.Item["childSessionId"] == nil {
			item.Item["childSessionId"] = item.Item["subagent_id"]
		}
	}
	return item
}
