package codex

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) resolveRequest(batch *model.Batch, obs model.Observation, params map[string]json.RawMessage) model.Disposition {
	obs.Detail = mustJSON(map[string]any{"method": "serverRequest/resolved", "params": selectFields(params, "requestId", "threadId")})
	id, valid := rpcID(params["requestId"])
	if !valid {
		return state.unknown(batch, obs, "invalid_rpc_id")
	}
	if obs.Source.Direction == "send" {
		return state.unknown(batch, obs, "unexpected_rpc_direction")
	}
	key := rpcKey(obs.Scope, "server", id)
	request, found := state.Requests[key]
	if !found {
		return state.unknown(batch, obs, "unresolved_rpc_resolution")
	}
	if !rpcScopeCompatible(obs.Scope, request.Scope) {
		return state.unknown(batch, obs, "rpc_scope_conflict")
	}
	if request.ResolutionRef != "" {
		state.replyReceipt(batch, obs, request.Scope, request.ItemID, request.ResolutionRef)
		return model.RecordNonContent
	}
	obs = bindObservation(obs, request.Scope)
	relation := model.Relation{Kind: model.RelationProgressOf, Scope: request.Scope, FromObservation: obs.ID, ObservationRefs: []string{request.ObservationRef, obs.ID}}
	if request.ItemID == "" {
		relation.ToObservation = request.ObservationRef
		batch.Observations = append(batch.Observations, obs)
		batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
	} else {
		item := state.Items[request.ItemID]
		// Resolution closes the RPC lifetime, but says nothing about the
		// decision. A separately captured answer keeps its known outcome.
		if request.ReplyRef == "" {
			obs.Outcome = model.OutcomeUnknown
		}
		relation.ToItem = item.ID
		item.Relations = append(append([]model.Relation(nil), item.Relations...), relation)
		state.observeRequestItem(batch, item, obs)
	}
	request.ResolutionRef = obs.ID
	state.Requests[key] = request
	return model.RecordNonContent
}

func rpcScopeCompatible(a, b model.Scope) bool {
	for _, pair := range [][2]*string{{a.NativeThreadID, b.NativeThreadID}, {a.NativeTurnID, b.NativeTurnID}} {
		if pair[0] != nil && pair[1] != nil && *pair[0] != *pair[1] {
			return false
		}
	}
	return true
}
