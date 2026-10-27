package codex

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) realtime(batch *model.Batch, obs model.Observation, method string, params map[string]json.RawMessage) model.Disposition {
	obs.Detail = mustJSON(map[string]any{"method": method, "params": params})
	if rawString(params, "threadId") == "" {
		return state.unknown(batch, obs, "missing_native_thread")
	}
	scope := notificationScope(obs.Scope, method, params)
	scope.Kind, scope.NativeTurnID = model.ScopeThread, nil
	obs = bindObservation(obs, scope)
	switch method {
	case "thread/realtime/transcript/delta", "thread/realtime/outputAudio/delta":
		// Flat transcript events have no part ID; audio's optional item ID alone
		// does not identify a captured baseline or prove its chunk ordering.
		obs.Kind, obs.Text = model.ObservationDelta, nativeText(params, "delta")
		obs.Capture = model.CapturePartial
		batch.Observations = append(batch.Observations, obs)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unresolved_realtime_part", obs.Source))
		return model.RecordUnknown
	case "thread/realtime/itemAdded":
		// The checked-in schema declares item as arbitrary JSON, not ThreadItem.
		return state.unknown(batch, obs, "unverified_realtime_item")
	case "thread/realtime/transcript/done", "thread/realtime/error":
		item := model.Item{Type: model.ItemProviderEvent, Category: model.CategoryUnclassified, Text: nativeText(params, "text"), Detail: obs.Detail, SourceRowType: "wire", SourcePayloadType: method}
		if method == "thread/realtime/error" {
			item.Type, item.Role, item.Category, item.Text = model.ItemNotice, model.RoleSystem, model.CategoryError, nativeText(params, "message")
		} else {
			switch rawString(params, "role") {
			case "user":
				item.Type, item.Role, item.Category = model.ItemUserMessage, model.RoleUser, model.CategoryMessages
			case "assistant":
				item.Type, item.Role, item.Category = model.ItemAgentMessage, model.RoleAssistant, model.CategoryMessages
			default:
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_realtime_role", obs.Source))
			}
		}
		item.Parts = []model.Part{{Type: "text", Text: item.Text, Availability: model.AvailableInline, Source: obs.Source}}
		complete := true
		obs.Kind, obs.Text, obs.Complete = model.ObservationSnapshot, item.Text, &complete
		state.emitScopedItem(batch, obs, item)
		return model.RecordSemantic
	default:
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	}
}
