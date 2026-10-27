package claude

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func interruptionBlocks(blocks []map[string]json.RawMessage) bool {
	if len(blocks) != 1 || rawString(blocks[0], "type") != "text" {
		return false
	}
	text := strings.TrimSpace(rawString(blocks[0], "text"))
	return text == "[Request interrupted by user]" || text == "[Request interrupted by user for tool use]" || strings.HasPrefix(text, "<turn_aborted>") && strings.HasSuffix(text, "</turn_aborted>")
}

func nativeNotice(row, body map[string]json.RawMessage, blocks []map[string]json.RawMessage, role string) bool {
	if rawBool(row, "isMeta") || rawBool(row, "isCompactSummary") || rawString(body, "model") == "<synthetic>" {
		return true
	}
	if role != "user" {
		return false
	}
	switch rawString(rawObject(row, "origin"), "kind") {
	case "human":
		return false
	case "task-notification", "coordinator":
		return true
	}
	return blocksInjected(blocks)
}

func envelopeDetail(row map[string]json.RawMessage) json.RawMessage {
	fields := map[string]json.RawMessage{}
	for _, key := range []string{"type", "origin", "entrypoint", "isMeta", "isCompactSummary", "isSidechain", "isApiErrorMessage", "error", "apiErrorStatus", "parentUuid", "logicalParentUuid"} {
		if raw, ok := row[key]; ok {
			fields[key] = raw
		}
	}
	return mustJSON(fields)
}

func (state *State) retractNamed(batch *model.Batch, observation model.Observation, row map[string]json.RawMessage) {
	var uuids []string
	if json.Unmarshal(row["retractedMessageUuids"], &uuids) != nil {
		return
	}
	for _, uuid := range uuids {
		ids := state.Rows[scopedKey(observation.Scope, "row", uuid)]
		if len(ids) == 0 {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unresolved_retraction", observation.Source))
			continue
		}
		for _, id := range ids {
			state.retractItem(batch, id, observation, model.BasisNative)
		}
	}
}

func (state *State) retractItem(batch *model.Batch, id string, evidence model.Observation, basis model.Basis) {
	item, ok := state.Items[id]
	if !ok {
		return
	}
	evidence.ID += "/retract/" + id
	evidence.Scope, evidence.Kind = item.Scope, model.ObservationMetadata
	evidence.Times = append([]model.TimeObservation(nil), evidence.Times...)
	for i := range evidence.Times {
		evidence.Times[i].Scope = item.Scope
		evidence.Times[i].ObservationRef = evidence.ID
	}
	item.Observations = append(append([]model.Observation(nil), item.Observations...), evidence)
	item.Finality = model.Finality{State: model.FinalityRetracted, Basis: basis, ObservationRefs: []string{evidence.ID}}
	item.IsFinalAnswer = false
	state.Items[id] = item
	batch.Observations = append(batch.Observations, evidence)
	batch.Changes = append(batch.Changes, model.Change{Item: &item})
	if turn, exists := state.Turns[item.TurnKey]; exists {
		refs := make([]string, 0, len(turn.FinalItemRefs))
		for _, ref := range turn.FinalItemRefs {
			if ref != id {
				refs = append(refs, ref)
			}
		}
		turn.FinalItemRefs = refs
		if turn.FinalItemID == id {
			turn.FinalItemID = ""
			if len(refs) > 0 {
				turn.FinalItemID = refs[len(refs)-1]
			}
		}
		state.putTurn(batch, turn)
	}
}

func (state *State) reopenProvisional(batch *model.Batch, turn model.Turn, evidence model.Observation) {
	turn = state.Turns[turn.Key]
	if turn.Outcome != model.OutcomeCompleted || len(turn.Completion) == 0 {
		return
	}
	completion := turn.Completion[len(turn.Completion)-1]
	if completion.Basis != model.BasisDerived || completion.Reason != "stop_reason" {
		return
	}
	for _, fact := range turn.Completion {
		if fact.Basis == model.BasisNative {
			return
		}
	}
	for _, id := range turn.FinalItemRefs {
		state.retractItem(batch, id, evidence, model.BasisDerived)
	}
	turn = state.Turns[turn.Key]
	evidence.ID += "/continued"
	evidence.Scope, evidence.Kind, evidence.Outcome = turn.Scope, model.ObservationProgress, model.OutcomeRunning
	evidence.Detail = mustJSON(map[string]string{"reason": "tool_activity_after_provisional_stop"})
	evidence.Times = append([]model.TimeObservation(nil), evidence.Times...)
	for i := range evidence.Times {
		evidence.Times[i].Scope = turn.Scope
		evidence.Times[i].ObservationRef = evidence.ID
	}
	turn.Observations = append(append([]model.Observation(nil), turn.Observations...), evidence)
	turn.Outcome, turn.Status, turn.CompletionEvidence = model.OutcomeRunning, model.TurnInProgress, model.CompletionNone
	batch.Observations = append(batch.Observations, evidence)
	state.putTurn(batch, turn)
}
