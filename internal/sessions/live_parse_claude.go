package sessions

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type claudeLiveAdapter struct {
	reader claude.State
}

func newClaudeLiveAdapter() *claudeLiveAdapter {
	return &claudeLiveAdapter{}
}

func (a *claudeLiveAdapter) Present(item model.Item, at time.Time) *liveProjectionItem {
	return parsedClaudeLiveItem(item, at)
}

func (a *claudeLiveAdapter) Apply(dir protocol.Direction, line []byte, at time.Time) model.Batch {
	next, batch, err := claude.ParseWire(a.reader, liveParseInput(dir, line, at))
	if err != nil {
		return model.Batch{}
	}
	a.reader = next
	return batch
}

func parsedClaudeLiveItem(parsed model.Item, at time.Time) *liveProjectionItem {
	item := parsedLiveItem(parsed, at)
	if parsed.NativeItemID == "" && parsed.Type == model.ItemUserMessage {
		for _, observation := range parsed.Observations {
			if observation.NativeEventID != "" {
				item.ID = observation.NativeEventID
				if len(parsed.Parts) > 0 && parsed.Parts[0].Index != nil && *parsed.Parts[0].Index > 0 {
					item.ID += fmt.Sprintf(":block:%d", *parsed.Parts[0].Index)
				}
				item.NativeItemID = item.ID
				break
			}
		}
	}
	for _, part := range parsed.Parts {
		if part.Type != "tool_use" && part.Type != "server_tool_use" {
			continue
		}
		item.Type = "toolUse"
		var detail map[string]any
		if json.Unmarshal(part.Detail, &detail) == nil && detail != nil {
			item.Item = detail
		}
		item.Item["type"], item.Item["name"], item.Item["toolUseId"] = item.Type, item.ToolName, item.CallID
		item.Item["inputJson"] = part.Text
		break
	}
	return item
}
