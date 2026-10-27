package claude

import (
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Stream decoding is extracted from claudeLiveAdapter in live_projection.go.
// The stream's active message is scoped; native block indices address items.
func (state *State) stream(batch *model.Batch, base model.Observation, row map[string]json.RawMessage) model.Disposition {
	event := rawObject(row, "event")
	if event == nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_stream_event", base.Source))
		return model.RecordMalformed
	}
	base.NativeEventID = "" // stream wrappers can repeat one UUID across all events.
	prefix := base.Source.BlockPath
	base.Source.BlockPath = nativePath(prefix, "$.event")
	base.Detail = mustJSON(event)
	group := threadKey(base.Scope)
	messageKey := state.ActiveMessages[group]
	if id := rawString(event, "message_id"); id != "" {
		messageKey = scopedKey(base.Scope, "message", id)
	}
	message := state.Messages[messageKey]
	switch rawString(event, "type") {
	case "message_start":
		body := rawObject(event, "message")
		id := rawString(body, "id")
		messageKey = scopedKey(base.Scope, "message", id)
		if id == "" {
			messageKey = sourceKey(base.Source, "message")
		}
		turn := state.ensureTurn(base.Scope, base.Source, false)
		message = state.Messages[messageKey]
		if message.Blocks == nil {
			message = messageState{ID: id, TurnKey: turn.Key, Stream: true, Blocks: map[int]string{}}
		} else if message.TurnKey != "" {
			turn = state.Turns[message.TurnKey]
		}
		state.Messages[messageKey] = message
		state.ActiveMessages[group] = messageKey
		base.Scope = turn.Scope
		base.Times = append([]model.TimeObservation(nil), base.Times...)
		for i := range base.Times {
			base.Times[i].Scope = turn.Scope
		}
		if name := rawString(body, "model"); name != "" && name != "<synthetic>" {
			turn.Model = name
		}
		if tier := rawString(rawObject(body, "usage"), "service_tier"); tier != "" {
			turn.ServiceTier = tier
		}
		turn.Observations = append(append([]model.Observation(nil), turn.Observations...), base)
		state.putTurn(batch, turn)
		state.recordUsage(batch, messageKey, rawObject(body, "usage"), base)
		batch.Observations = append(batch.Observations, base)
	case "content_block_start":
		index, ok := blockIndex(event, "index")
		if !ok || message.Blocks == nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("orphan_block_start", base.Source))
			batch.Observations = append(batch.Observations, base)
			return model.RecordUnknown
		}
		block := rawObject(event, "content_block")
		turn := state.Turns[message.TurnKey]
		base.Source.BlockPath = nativePath(prefix, "$.event.content_block")
		base.Kind = model.ObservationSnapshot
		open := false
		base.Complete = &open
		item := state.blockItem(turn, base, block, message.ID, index, true, message.Blocks[index])
		if len(item.Observations) == 0 {
			item.SourceRowType = "stream_event"
		}
		state.observeBlock(batch, turn, &item, base, block)
		message.Blocks[index] = item.ID
		state.Messages[messageKey] = message
	case "content_block_delta", "content_block_stop":
		index, ok := blockIndex(event, "index")
		item, exists := state.Items[message.Blocks[index]]
		if !ok || !exists {
			base.Kind = model.ObservationUnknown
			batch.Observations = append(batch.Observations, base)
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("orphan_block_event", base.Source))
			return model.RecordUnknown
		}
		base.ID = sourceKey(base.Source, fmt.Sprintf("block:%d", index))
		base.Kind = model.ObservationDelta
		if rawString(event, "type") == "content_block_stop" {
			complete := true
			base.Kind = model.ObservationStop
			base.Complete = &complete
		} else {
			delta := rawObject(event, "delta")
			switch rawString(delta, "type") {
			case "text_delta":
				base.Text = rawText(delta, "text")
			case "thinking_delta":
				base.Text = rawText(delta, "thinking")
			case "input_json_delta":
				base.Text = rawText(delta, "partial_json")
			default:
				base.Kind = model.ObservationMetadata
			}
		}
		state.observeBlock(batch, state.Turns[message.TurnKey], &item, base, event)
	case "message_delta":
		if message.Blocks == nil {
			base.Kind = model.ObservationUnknown
			batch.Observations = append(batch.Observations, base)
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("orphan_message_delta", base.Source))
			return model.RecordUnknown
		}
		state.recordUsage(batch, messageKey, rawObject(event, "usage"), base)
		if !state.stopReason(batch, messageKey, rawString(rawObject(event, "delta"), "stop_reason"), base) {
			batch.Observations = append(batch.Observations, base)
		}
	case "message_stop":
		batch.Observations = append(batch.Observations, base)
	default:
		batch.Observations = append(batch.Observations, base)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_stream_event", base.Source))
		return model.RecordUnknown
	}
	return model.RecordSemantic
}
