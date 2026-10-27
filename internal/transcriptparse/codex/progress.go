package codex

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) itemUpdate(batch *model.Batch, obs model.Observation, method string, params map[string]json.RawMessage) model.Disposition {
	id := rawString(params, "itemId")
	if id == "" {
		return state.unknown(batch, obs, "missing_item_id")
	}
	turn := state.turn(obs.Scope, obs.Source, false, false)
	prior := state.Items[state.NativeItems[scopedKey(turn.Scope, "item", id)]]
	item := prior
	if prior.ID == "" {
		item = model.Item{NativeItemID: id, Role: model.RoleAssistant, SourceRowType: "wire", SourcePayloadType: method}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("item_update_without_start", obs.Source))
	}
	item.Parts = append([]model.Part(nil), item.Parts...)
	obs.Detail = mustJSON(params)
	obs.Kind = model.ObservationProgress
	part := model.Part{Availability: model.AvailableInline, Source: obs.Source}
	switch method {
	case "item/plan/delta":
		item.Type, item.Category = model.ItemPlan, model.CategorySystem
		item.Text = nativeText(params, "delta")
		obs.Kind = model.ObservationDelta
		part.Type, part.Text = "text", prior.Text+item.Text
	case "item/fileChange/outputDelta":
		item.Type, item.Category = model.ItemFileChange, model.CategoryToolCall
		obs.Kind = model.ObservationSnapshot
		part.Type, part.Text = "tool_output", nativeText(params, "delta")
		for _, earlier := range item.Parts {
			if earlier.Type == part.Type {
				part.Text = earlier.Text + part.Text
				break
			}
		}
	case "item/fileChange/patchUpdated":
		item.Type, item.Category = model.ItemFileChange, model.CategoryToolCall
		part.Type, part.Detail = "changes", mustJSON(params["changes"])
	case "item/mcpToolCall/progress":
		item.Type, item.Category = model.ItemToolCall, model.CategoryToolCall
		part.Type, part.Text = "progress", nativeText(params, "message")
	case "item/commandExecution/terminalInteraction":
		item.Type, item.Category = model.ItemCommand, model.CategoryToolCall
		part.Type, part.Text, part.Detail = "terminal_input", nativeText(params, "stdin"), mustJSON(params)
	}
	found := false
	for i := range item.Parts {
		if item.Parts[i].Type == part.Type {
			item.Parts[i], found = part, true
			break
		}
	}
	if !found {
		item.Parts = append(item.Parts, part)
	}
	state.emit(batch, turn, obs, item, false)
	return model.RecordSemantic
}
