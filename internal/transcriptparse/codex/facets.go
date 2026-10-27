package codex

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) turnFacet(batch *model.Batch, obs model.Observation, method string, params map[string]json.RawMessage) model.Disposition {
	obs.Detail = mustJSON(params)
	if rawString(params, "threadId") == "" || rawString(params, "turnId") == "" {
		return state.unknown(batch, obs, "missing_native_turn")
	}
	obs = bindObservation(obs, notificationScope(obs.Scope, method, params))
	item := model.Item{Role: model.RoleSystem, Category: model.CategorySystem, SourceRowType: "wire", SourcePayloadType: method, Detail: obs.Detail}
	facet := method
	complete := true
	obs.Kind, obs.Complete = model.ObservationSnapshot, &complete
	switch method {
	case "turn/diff/updated":
		item.Type, item.Text = model.ItemFileChange, nativeText(params, "diff")
		item.Parts = []model.Part{{Type: "unified_diff", Text: item.Text, Availability: model.AvailableInline, Source: obs.Source}}
	case "turn/plan/updated":
		item.Type = model.ItemPlan
		var steps []map[string]json.RawMessage
		if json.Unmarshal(params["plan"], &steps) != nil || string(params["plan"]) == "null" {
			return state.unknown(batch, obs, "malformed_plan")
		}
		var lines []string
		if explanation := nativeText(params, "explanation"); explanation != "" {
			lines = append(lines, explanation)
		}
		for _, step := range steps {
			lines = append(lines, nativeText(step, "step"))
		}
		item.Text = strings.Join(lines, "\n")
		item.Parts = []model.Part{{Type: "plan", Detail: params["plan"], Availability: model.AvailableInline, Source: obs.Source}}
	default:
		reviewID := rawString(params, "reviewId")
		if reviewID == "" {
			return state.unknown(batch, obs, "missing_review_id")
		}
		facet = "approval_review/" + reviewID
		item.Type = model.ItemInteraction
		item.Text = nativeText(rawObject(params, "action"), "command", "program", "toolName", "target")
		item.Aliases = []model.Alias{{Namespace: "codex.reviewId", Value: mustJSON(reviewID), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
		item.Parts = []model.Part{{Type: "review_action", Detail: params["action"], Availability: model.AvailableInline, Source: obs.Source}}
		complete = method == "item/autoApprovalReview/completed"
		obs.Times = append(obs.Times, nativeItemTimes(params, obs.Scope, obs.ID)...)
		switch rawString(rawObject(params, "review"), "status") {
		case "inProgress":
			obs.Outcome = model.OutcomeRunning
		case "approved":
			obs.Outcome = model.OutcomeCompleted
		case "denied":
			obs.Outcome = model.OutcomeDeclined
		case "timedOut":
			obs.Outcome = model.OutcomeFailed
		case "aborted":
			obs.Outcome = model.OutcomeCancelled
		default:
			obs.Outcome = model.OutcomeUnknown
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_review_status", obs.Source))
		}
	}
	obs.Text = item.Text
	key := scopedKey(obs.Scope, "update", facet)
	prior := state.Items[state.UpdateItems[key]]
	if prior.ID == "" {
		item = state.emitScopedItem(batch, obs, item)
		state.UpdateItems[key] = item.ID
	} else {
		prior.Detail, prior.Parts = item.Detail, item.Parts
		item = state.observeRequestItem(batch, prior, obs)
	}
	item.Relations = append(append([]model.Relation(nil), item.Relations...), model.Relation{Kind: model.RelationProgressOf, Scope: item.Scope, FromObservation: obs.ID, ToItem: item.ID, ObservationRefs: []string{obs.ID}})
	state.Items[item.ID] = item
	batch.Changes = append(batch.Changes, model.Change{Item: &item})
	return model.RecordSemantic
}
