package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestLiveCommandOutputKeepsUTF8HeadTailAndBoundsCumulativeState(t *testing.T) {
	state := State{}
	apply := func(frame any) model.Item {
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		var batch model.Batch
		state, batch, err = ParseWire(state, model.Input{
			Bytes:  raw,
			Source: model.SourceRef{Kind: "wire", Label: "live", Incarnation: "test", Direction: "recv"},
		})
		if err != nil {
			t.Fatal(err)
		}
		for index := len(batch.Changes) - 1; index >= 0; index-- {
			if batch.Changes[index].Item != nil && batch.Changes[index].Item.Type == model.ItemCommand {
				return *batch.Changes[index].Item
			}
		}
		return model.Item{}
	}
	apply(map[string]any{"method": "item/started", "params": map[string]any{
		"threadId": "thread", "turnId": "turn", "item": map[string]any{
			"id": "command", "type": "commandExecution", "command": "generate output", "status": "inProgress",
		},
	}})

	delta := strings.Repeat("é", 96)
	var item model.Item
	for index := 0; index < 1000; index++ {
		item = apply(map[string]any{"method": "item/commandExecution/outputDelta", "params": map[string]any{
			"threadId": "thread", "turnId": "turn", "itemId": "command", "delta": delta,
		}})
	}
	output := commandOutputPart(t, item)
	wantBytes := int64(1000 * len(delta))
	if len(output.Text) > liveCommandOutputBytes || !utf8.ValidString(output.Text) ||
		!strings.Contains(output.Text, liveCommandOutputMarker) {
		t.Fatalf("bounded output bytes=%d valid=%t marker=%t", len(output.Text), utf8.ValidString(output.Text), strings.Contains(output.Text, liveCommandOutputMarker))
	}
	if output.OriginalBytes == nil || *output.OriginalBytes != wantBytes || !item.ContentTruncated || item.OriginalBytes != wantBytes {
		t.Fatalf("truncation metadata part=%v item=(%t,%d), want %d", output.OriginalBytes, item.ContentTruncated, item.OriginalBytes, wantBytes)
	}
	if len(item.Observations) > 2*liveCommandObservationEdge {
		t.Fatalf("retained observations=%d, cap=%d", len(item.Observations), 2*liveCommandObservationEdge)
	}
}

func TestLiveCommandCompletionCannotRestoreUnboundedOutput(t *testing.T) {
	output := strings.Repeat("head-", 100000) + "終端"
	raw, err := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": "thread", "turnId": "turn", "item": map[string]any{
			"id": "command", "type": "commandExecution", "command": "generate output",
			"aggregatedOutput": output, "status": "completed", "exitCode": 0,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, batch, err := ParseWire(State{}, model.Input{
		Bytes: raw, Source: model.SourceRef{Kind: "wire", Label: "live", Incarnation: "test", Direction: "recv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var item model.Item
	for _, change := range batch.Changes {
		if change.Item != nil {
			item = *change.Item
		}
	}
	part := commandOutputPart(t, item)
	if len(part.Text) > liveCommandOutputBytes || !strings.HasSuffix(part.Text, "終端") ||
		!strings.Contains(part.Text, liveCommandOutputMarker) || item.OriginalBytes != int64(len(output)) {
		t.Fatalf("completed output bytes=%d suffix=%t marker=%t original=%d", len(part.Text), strings.HasSuffix(part.Text, "終端"), strings.Contains(part.Text, liveCommandOutputMarker), item.OriginalBytes)
	}
}

func TestSavedCommandOutputParsingRemainsComplete(t *testing.T) {
	output := strings.Repeat("saved-output-", 7000)
	raw, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{
		"type": "item_completed", "item": map[string]any{
			"id": "command", "type": "commandExecution", "command": "saved command",
			"aggregatedOutput": output, "status": "completed", "exitCode": 0,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	_, batch, err := ParseFile(State{}, model.Input{
		Bytes: raw, Closed: true, Source: model.SourceRef{Kind: "file", Label: "saved", Incarnation: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range batch.Changes {
		if change.Item == nil || change.Item.Type != model.ItemCommand {
			continue
		}
		part := commandOutputPart(t, *change.Item)
		if part.Text != output || part.Availability == model.AvailableTruncated || change.Item.ContentTruncated {
			t.Fatalf("saved output was truncated: bytes=%d availability=%s itemTruncated=%t", len(part.Text), part.Availability, change.Item.ContentTruncated)
		}
		return
	}
	t.Fatal("saved command item not parsed")
}

func TestLiveCommandOutputLimitDoesNotApplyToAgentMessages(t *testing.T) {
	text := strings.Repeat("agent-message-", 7000)
	raw, err := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": "thread", "turnId": "turn", "item": map[string]any{
			"id": "agent", "type": "agentMessage", "text": text,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, batch, err := ParseWire(State{}, model.Input{
		Bytes: raw, Source: model.SourceRef{Kind: "wire", Label: "live", Incarnation: "test", Direction: "recv"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range batch.Changes {
		if change.Item == nil || change.Item.Type != model.ItemAgentMessage {
			continue
		}
		if change.Item.Text != text || change.Item.ContentTruncated {
			t.Fatalf("agent message was changed by command-output policy: bytes=%d truncated=%t", len(change.Item.Text), change.Item.ContentTruncated)
		}
		return
	}
	t.Fatal("agent message not parsed")
}

func commandOutputPart(t *testing.T, item model.Item) model.Part {
	t.Helper()
	for _, part := range item.Parts {
		if part.Type == "tool_output" {
			return part
		}
	}
	t.Fatalf("command item has no output part: %+v", item)
	return model.Part{}
}
