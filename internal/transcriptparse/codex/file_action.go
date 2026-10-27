package codex

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func fileActionKind(kind string) bool {
	switch kind {
	case "exec_command_begin", "exec_command_end", "patch_apply_begin", "patch_apply_end":
		return true
	}
	return false
}

// Native execution events describe progress on a generated invocation. Their
// terminal outcome must not overwrite generation or steal its result binding.
func (state *State) fileAction(batch *model.Batch, obs model.Observation, payload map[string]json.RawMessage) model.Disposition {
	kind := rawString(payload, "type")
	turn := state.turn(obs.Scope, obs.Source, false, false)
	callID := rawString(payload, "call_id")
	family := "command"
	if strings.HasPrefix(kind, "patch_") {
		family = "patch"
	}
	key := scopedKey(turn.Scope, "file-action", turn.Key+"/"+family+"/"+callID)
	previous, seen := state.UpdateItems[key]
	item := state.Items[previous]
	// A new start after terminal evidence can be a reused call ID or a late
	// capture. Neither proves which execution later events describe.
	ambiguous := seen && previous == "" || strings.HasSuffix(kind, "_begin") && item.Complete
	if ambiguous {
		item = model.Item{}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_action_call_id", obs.Source))
	}
	if callID == "" {
		item = model.Item{}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("missing_action_call_id", obs.Source))
	}
	if item.ID == "" {
		item = model.Item{Type: model.ItemCommand, Role: model.RoleAssistant, Category: model.CategoryToolCall, CallID: callID, SourceRowType: "event_msg", SourcePayloadType: kind}
		if family == "patch" {
			item.Type = model.ItemFileChange
		}
		if callID != "" {
			item.Aliases = []model.Alias{{Namespace: "codex.callId", Value: mustJSON(callID), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
		}
	}
	item.Parts = append([]model.Part(nil), item.Parts...)
	item.Detail = mustJSON(payload)
	if command, _ := codexCommandDisplay(payload); command != "" {
		item.Text = command
	}
	if item.Text == "" {
		item.Text = nativeText(payload, "text")
	}
	for _, field := range []string{"command", "changes", "stdout", "stderr", "aggregated_output"} {
		if raw, exists := payload[field]; exists {
			part := model.Part{Type: field, Source: obs.Source, Availability: model.AvailableInline, Detail: raw}
			part.Source.BlockPath = "$.payload." + field
			part.Text = nativeText(payload, field)
			found := false
			for i := range item.Parts {
				if item.Parts[i].Type == field {
					item.Parts[i], found = part, true
					break
				}
			}
			if !found {
				item.Parts = append(item.Parts, part)
			}
		}
	}
	if len(item.Parts) == 0 {
		item.Parts = []model.Part{{Type: family, Text: item.Text, Source: obs.Source, Availability: model.AvailableInline, Detail: mustJSON(payload)}}
	}
	complete := strings.HasSuffix(kind, "_end")
	obs.Kind, obs.Complete, obs.Outcome = model.ObservationSnapshot, &complete, nativeOutcome(payload)
	if !complete && obs.Outcome == model.OutcomeUnknown {
		obs.Outcome = model.OutcomeRunning
	}
	item = state.emit(batch, turn, obs, item, false)
	if callID != "" {
		state.UpdateItems[key] = item.ID
		if ambiguous {
			state.UpdateItems[key] = ""
			return model.RecordSemantic
		}
		call := state.Items[state.Calls[scopedKey(turn.Scope, "call", turn.Key+"/"+callID)]]
		if call.ID != "" {
			item.Relations = append(append([]model.Relation(nil), item.Relations...), model.Relation{Kind: model.RelationProgressOf, Scope: item.Scope, FromObservation: obs.ID, ToItem: call.ID, ObservationRefs: []string{obs.ID}})
			state.Items[item.ID] = item
			batch.Changes = append(batch.Changes, model.Change{Item: &item})
		}
	}
	return model.RecordSemantic
}
