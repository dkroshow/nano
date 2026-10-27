package codex

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func threadSnapshotMethod(method string) bool {
	switch method {
	case "thread/start", "thread/read", "thread/resume", "thread/fork":
		return true
	}
	return false
}

func snapshotThread(row map[string]json.RawMessage) map[string]json.RawMessage {
	thread := rawObject(rawObject(row, "result"), "thread")
	if rawString(thread, "id") == "" {
		return nil
	}
	var turns []json.RawMessage
	if json.Unmarshal(thread["turns"], &turns) != nil || string(thread["turns"]) == "null" {
		return nil
	}
	return thread
}

func (state *State) threadSnapshot(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage, method string) model.Disposition {
	thread := snapshotThread(row)
	if thread == nil {
		return state.unknown(batch, obs, "malformed_thread_snapshot")
	}
	id := rawString(thread, "id")
	if method != "thread/fork" && obs.Scope.NativeThreadID != nil && *obs.Scope.NativeThreadID != id {
		return state.unknown(batch, obs, "rpc_scope_conflict")
	}
	scope := obs.Scope
	scope.Kind, scope.NativeThreadID, scope.NativeTurnID, scope.TurnKey, scope.ItemID = model.ScopeThread, &id, nil, "", ""
	if parent := rawString(thread, "parentThreadId"); parent != "" {
		scope.ParentNativeID = &parent
	}
	obs = bindObservation(obs, scope)
	obs.Detail = mustJSON(map[string]any{"kind": "thread_snapshot", "method": method, "requestId": row["id"], "thread": selectFields(thread, "id", "parentThreadId", "createdAt", "updatedAt", "status", "name")})
	batch.Observations = append(batch.Observations, obs)
	// Loading persisted turns cannot identify the turn for later unaddressed
	// traffic on a multiplexed connection.
	group := threadKey(scope)
	active, hadActive := state.ActiveTurns[group]
	defer func() {
		if hadActive {
			state.ActiveTurns[group] = active
		} else {
			delete(state.ActiveTurns, group)
		}
	}()
	var turns []json.RawMessage
	_ = json.Unmarshal(thread["turns"], &turns)
	for index, raw := range turns {
		turnObs := snapshotObservation(obs, fmt.Sprintf("result.thread.turns[%d]", index))
		var payload map[string]json.RawMessage
		if json.Unmarshal(raw, &payload) != nil || rawString(payload, "id") == "" {
			turnObs.Detail = raw
			state.unknown(batch, turnObs, "malformed_snapshot_turn")
			continue
		}
		nativeID := rawString(payload, "id")
		turnScope := scope
		turnScope.NativeTurnID = &nativeID
		priorID := state.NativeTurns[scopedKey(turnScope, "turn", nativeID)]
		turn := state.turn(turnScope, turnObs.Source, false, false)
		turnObs = bindObservation(turnObs, turn.Scope)
		turnObs.Detail = mustJSON(selectFields(payload, "id", "status", "itemsView", "startedAt", "completedAt", "durationMs", "error"))
		turnObs.Times = append(turnObs.Times, nativeTurnTimes(payload, turn.Scope, turnObs.ID)...)
		var items []json.RawMessage
		view := rawString(payload, "itemsView")
		validItems := json.Unmarshal(payload["items"], &items) == nil && string(payload["items"]) != "null"
		coverage := model.CapturePartial
		if validItems && view == "full" {
			coverage = model.CaptureComplete
		}
		if !validItems {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_snapshot_items", turnObs.Source))
		}
		for itemIndex, itemRaw := range items {
			itemObs := snapshotObservation(obs, fmt.Sprintf("%s.items[%d]", turnObs.Source.BlockPath, itemIndex))
			var itemPayload map[string]json.RawMessage
			if json.Unmarshal(itemRaw, &itemPayload) != nil || rawString(itemPayload, "id") == "" {
				itemObs = bindObservation(itemObs, turn.Scope)
				itemObs.Detail = itemRaw
				state.unknown(batch, itemObs, "malformed_snapshot_item")
				continue
			}
			state.snapshotItem(batch, turn, itemObs, itemPayload, view)
		}
		turn = state.Turns[turn.Key]
		if priorID == "" {
			turn.Capture = coverage
		}
		outcome := nativeOutcome(payload)
		if turn.Outcome == model.OutcomeUnknown && outcome != model.OutcomeUnknown && outcome != model.OutcomeRunning {
			state.terminal(batch, turn, turnObs, outcome, "thread_snapshot")
		} else {
			if turn.Outcome == model.OutcomeUnknown {
				turn.Outcome = outcome
				if outcome == model.OutcomeRunning {
					turn.Status = model.TurnInProgress
				}
			}
			// Reply order does not prove the snapshot was produced after a live
			// terminal or delta. Keep conflicting status as evidence only.
			if priorID != "" && outcome != model.OutcomeUnknown && turn.Outcome != outcome {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("snapshot_turn_status_unproven", turnObs.Source))
			}
			turn.Observations = append(append([]model.Observation(nil), turn.Observations...), turnObs)
			turn.Times = append(append([]model.TimeObservation(nil), turn.Times...), turnObs.Times...)
			batch.Observations = append(batch.Observations, turnObs)
			state.putTurn(batch, turn)
		}
	}
	if len(turns) > 0 {
		return model.RecordSemantic
	}
	return model.RecordNonContent
}

func snapshotObservation(parent model.Observation, path string) model.Observation {
	obs := parent
	obs.Source.BlockPath = path
	obs.ID = sourceKey(obs.Source, "snapshot")
	obs.Detail = nil
	return bindObservation(obs, obs.Scope)
}

func (state *State) snapshotItem(batch *model.Batch, turn model.Turn, obs model.Observation, payload map[string]json.RawMessage, view string) {
	item := nativeItem(payload, obs.Source)
	obs.Detail, obs.Text = mustJSON(payload), item.Text
	obs.Times = append(obs.Times, nativeItemTimes(payload, turn.Scope, obs.ID)...)
	batch.Diagnostics = append(batch.Diagnostics, itemDiagnostic(item, payload, obs.Source)...)
	prior := state.Items[state.NativeItems[scopedKey(turn.Scope, "item", item.NativeItemID)]]
	if prior.ID != "" {
		// A persisted snapshot is not an item/completed notification. Without
		// coverage offsets, even a longer prefix can overlap received deltas.
		obs.Kind = model.ObservationMetadata
		if prior.Text != item.Text || !reflect.DeepEqual(partEvidence(prior.Parts), partEvidence(item.Parts)) {
			obs.Capture = model.CapturePartial
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("snapshot_delta_overlap_unproven", obs.Source))
		}
		state.observeRequestItem(batch, prior, obs)
		return
	}
	obs.Kind, obs.Outcome = model.ObservationSnapshot, nativeOutcome(payload)
	if view != "full" {
		obs.Capture = model.CapturePartial
	}
	if obs.Outcome != model.OutcomeUnknown && obs.Outcome != model.OutcomeRunning {
		complete := true
		obs.Complete = &complete
	}
	state.emit(batch, turn, obs, item, false)
}

func partEvidence(parts []model.Part) []model.Part {
	if len(parts) == 0 {
		return nil
	}
	out := append([]model.Part(nil), parts...)
	for i := range out {
		out[i].Source = model.SourceRef{}
	}
	return out
}
