package codex

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"strings"
)

func (state *State) apply(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) model.Disposition {
	if source.Kind == "wire" {
		return state.wire(batch, input, source, row)
	}
	return state.file(batch, input, source, row)
}

func (state *State) scope(input model.Input, fields map[string]json.RawMessage) model.Scope {
	scope := input.Scope
	scope.Kind, scope.Provider, scope.Source, scope.Incarnation = model.ScopeThread, model.ProviderCodex, input.Source.Label, input.Source.Incarnation
	scope.TurnKey, scope.ItemID = "", ""
	if id := rawString(fields, "thread_id", "threadId"); id != "" {
		scope.NativeThreadID = &id
	} else if id := state.NativeThreads[scope.ConnectionID]; scope.NativeThreadID == nil && id != "" {
		scope.NativeThreadID = &id
	}
	if id := rawString(fields, "turn_id", "turnId"); id != "" {
		scope.NativeTurnID = &id
	}
	return scope
}
func threadKey(scope model.Scope) string {
	scope.NativeTurnID = nil
	return scopedKey(scope, "thread", "")
}

func (state *State) turn(scope model.Scope, source model.SourceRef, activate, newPrompt bool) model.Turn {
	group := threadKey(scope)
	if scope.NativeTurnID != nil {
		key := scopedKey(scope, "turn", *scope.NativeTurnID)
		if id := state.NativeTurns[key]; id != "" {
			turn := state.Turns[id]
			if activate {
				state.ActiveTurns[group] = id
				state.takePendingSettings(&turn, group)
				state.Turns[id] = turn
			}
			return turn
		}
	} else if id := state.ActiveTurns[group]; id != "" && !newPrompt {
		return state.Turns[id]
	}
	state.NextTurn++
	turn := model.Turn{Key: sourceKey(source, "turn"), Ordinal: state.NextTurn, Scope: scope, Outcome: model.OutcomeUnknown, Status: model.TurnUnknown, Capture: model.CaptureUnknown, CompletionEvidence: model.CompletionNone}
	turn.Scope.Kind, turn.Scope.TurnKey = model.ScopeTurn, turn.Key
	if scope.NativeTurnID != nil {
		turn.NativeTurnID = *scope.NativeTurnID
		state.NativeTurns[scopedKey(scope, "turn", *scope.NativeTurnID)] = turn.Key
	}
	if activate || newPrompt {
		state.takePendingSettings(&turn, group)
	}
	state.Turns[turn.Key] = turn
	if activate || newPrompt || state.ActiveTurns[group] == "" {
		state.ActiveTurns[group] = turn.Key
	}
	return turn
}

func (state *State) takePendingSettings(turn *model.Turn, group string) {
	if pending, ok := state.PendingSettings[group]; ok {
		turn.ServiceTier = pending.Tier
		turn.Observations = append(append([]model.Observation(nil), turn.Observations...), pending.Observation)
		delete(state.PendingSettings, group)
	}
}
func (state *State) putTurn(batch *model.Batch, turn model.Turn) {
	state.Turns[turn.Key] = turn
	batch.Changes = append(batch.Changes, model.Change{Turn: &turn})
}
func observation(source model.SourceRef, scope model.Scope, row map[string]json.RawMessage) model.Observation {
	return model.Observation{ID: sourceKey(source, "row"), Scope: scope, Source: source, Kind: model.ObservationMetadata, Capture: model.CaptureComplete, Detail: mustJSON(row)}
}
func bindObservation(obs model.Observation, scope model.Scope) model.Observation {
	obs.Scope = scope
	obs.Times = append([]model.TimeObservation(nil), obs.Times...)
	for i := range obs.Times {
		obs.Times[i].Scope = scope
		obs.Times[i].ObservationRef = obs.ID
	}
	return obs
}
func (state *State) emit(batch *model.Batch, turn model.Turn, obs model.Observation, next model.Item, twin bool) model.Item {
	nativeKey := scopedKey(turn.Scope, "item", next.NativeItemID)
	id := next.ID
	if next.NativeItemID != "" {
		id = state.NativeItems[nativeKey]
	}
	twinKey := twinBucket(turn, next.Role, next.Type, next.Text)
	newestKey := scopedKey(turn.Scope, "newest", turn.Key+"/"+string(next.Type))
	twinID := ""
	if id == "" && twin && next.Text != "" {
		candidates := state.Twins[twinKey]
		// Codex writes an item's two copies next to each other, so a candidate
		// of these types is open only while it is the newest item of its type
		// in the turn. An older lone copy with equal text is a different item;
		// it still counts toward ambiguity, so neither is ever guessed.
		open := len(candidates) == 1 && (!newestClosesTwins(next.Type) || state.NewestItems[newestKey] == candidates[0])
		if open && serializationTwinCompatible(state.Items[candidates[0]], next) {
			id, twinID = candidates[0], candidates[0]
			state.Twins[twinKey] = nil
		} else if len(candidates) > 1 {
			for _, candidate := range candidates {
				if state.Items[candidate].SourceRowType != next.SourceRowType {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_serialization_twin", obs.Source))
					break
				}
			}
		}
	}
	item, exists := state.Items[id]
	if exists && twin && twinID == "" && next.Text != "" && item.Text != next.Text {
		// Codex completes a growing Reasoning record again under its native
		// id. An unmatched twin candidate follows its newest content, so the
		// response copy matches the final summary.
		oldKey := twinBucket(turn, item.Role, item.Type, item.Text)
		for index, candidate := range state.Twins[oldKey] {
			if candidate == item.ID {
				state.Twins[oldKey] = append(append([]string(nil), state.Twins[oldKey][:index]...), state.Twins[oldKey][index+1:]...)
				state.Twins[twinKey] = append(append([]string(nil), state.Twins[twinKey]...), item.ID)
				break
			}
		}
	}
	if !exists {
		item = next
		state.NextItem++
		item.ID = sourceKey(obs.Source, "item")
		item.SourceSeq = state.NextItem
		item.Scope = turn.Scope
		item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
		item.TurnKey, item.TurnOrdinal = turn.Key, turn.Ordinal
		item.SourceLine, item.SourceLineSHA256 = int(obs.Source.Line), obs.Source.SHA256
		item.Text = ""
		if newestClosesTwins(item.Type) {
			state.NewestItems[newestKey] = item.ID
		}
		if twin && next.Text != "" {
			state.Twins[twinKey] = append(append([]string(nil), state.Twins[twinKey]...), item.ID)
		}
	}
	if next.NativeItemID != "" {
		// A joined serialization twin keeps its first native id; the other
		// copy's id is an alias. History binds an item to one exact native id,
		// which an earlier commit may already hold. Other items take the
		// newest id.
		if item.NativeItemID == "" || !completedTwinType(item.Type) {
			item.NativeItemID = next.NativeItemID
		}
		state.NativeItems[nativeKey] = item.ID
		found := false
		for _, alias := range item.Aliases {
			if alias.Namespace == "codex.itemId" && string(alias.Value) == string(mustJSON(next.NativeItemID)) {
				found = true
			}
		}
		if !found {
			item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: "codex.itemId", Value: mustJSON(next.NativeItemID), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}})
		}
	}
	if next.CallID != "" {
		item.CallID = next.CallID
	}
	if next.ToolName != "" {
		item.ToolName = next.ToolName
	}
	if next.Detail != nil {
		item.Detail = next.Detail
	}
	if next.Parts != nil {
		item.Parts = next.Parts
	}
	if next.Phase != "" {
		item.Phase = next.Phase
	}
	obs = bindObservation(obs, item.Scope)
	obs.Text = next.Text
	var diagnostics []model.Diagnostic
	item, diagnostics = assemble.Observe(item, obs)
	if liveWireSource(obs.Source) && item.Type == model.ItemCommand {
		item = boundLiveCommandItem(item)
	}
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	if obs.Kind == model.ObservationProgress {
		item.Relations = append(append([]model.Relation(nil), item.Relations...), model.Relation{
			Kind: model.RelationProgressOf, Scope: item.Scope, FromObservation: obs.ID,
			ToItem: item.ID, ObservationRefs: []string{obs.ID},
		})
	}
	if twinID != "" && len(item.Observations) > 1 {
		earlier := item.Observations[0]
		item.Relations = append(append([]model.Relation(nil), item.Relations...), model.Relation{Kind: model.RelationSerializationTwin, Scope: item.Scope, FromObservation: obs.ID, ToObservation: earlier.ID, ObservationRefs: []string{earlier.ID, obs.ID}})
		if item.Type == model.ItemUserMessage && (item.SourcePayloadType == "item_completed" || next.SourcePayloadType == "item_completed") {
			batch.RepairHints = append(batch.RepairHints, model.RepairHintCodexCompletedUserTwin)
		}
	}
	if item.Phase == "final_answer" {
		item.Finality = model.Finality{State: model.FinalityFinal, Basis: model.BasisNative, ObservationRefs: []string{obs.ID}}
		item.IsFinalAnswer = true
	}
	callKey := scopedKey(turn.Scope, "call", turn.Key+"/"+item.CallID)
	if item.CallID != "" {
		if item.Category == model.CategoryToolCall && !fileActionKind(item.SourcePayloadType) {
			if prior, exists := state.Calls[callKey]; exists && prior != item.ID {
				// A later invocation cannot erase ambiguity in the native call ID.
				state.Calls[callKey] = ""
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_call_id", obs.Source))
			} else {
				state.Calls[callKey] = item.ID
			}
		}
		if item.Type == model.ItemToolResult && len(item.Relations) == 0 {
			item, diagnostics = assemble.LinkResult(state.Items[state.Calls[callKey]], item)
			batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
		}
	}
	state.Items[item.ID] = item
	batch.Changes = append(batch.Changes, model.Change{Item: &item})
	batch.Observations = append(batch.Observations, obs)
	turn = state.Turns[turn.Key]
	if turn.StartSourceSeq == 0 {
		turn.StartSourceSeq = item.SourceSeq
	}
	if item.SourceSeq > turn.EndSourceSeq {
		turn.EndSourceSeq = item.SourceSeq
	}
	state.putTurn(batch, turn)
	return item
}

// serializationTwinCompatible accepts only the pairs Codex writes for one
// item: an event copy and a response_item copy of the same type. The
// completed-event pairs are UserMessage (reader v35), and AgentMessage and
// Reasoning (reader v36). Completed user events can carry attachments, so
// equal captions alone must not join multimodal prompts; completed agent
// messages follow the same text-only rule.
func serializationTwinCompatible(previous, next model.Item) bool {
	if previous.SourceRowType == next.SourceRowType || previous.Type != next.Type {
		return false
	}
	if previous.SourcePayloadType != "item_completed" && next.SourcePayloadType != "item_completed" {
		return true
	}
	completed, response := previous, next
	if next.SourcePayloadType == "item_completed" {
		completed, response = next, previous
	}
	if response.SourceRowType != "response_item" || !completedTwinType(completed.Type) {
		return false
	}
	textTypes := map[string]bool{"text": true, "input_text": true}
	if completed.Type == model.ItemAgentMessage {
		textTypes = map[string]bool{"text": true, "Text": true, "output_text": true}
	}
	if completed.Type != model.ItemReasoning {
		for _, item := range []model.Item{previous, next} {
			for _, part := range item.Parts {
				if !textTypes[part.Type] {
					return false
				}
			}
		}
	}
	return true
}

// completedTwinType names the item types whose completed event record has a
// response_item serialization twin.
func completedTwinType(itemType model.ItemType) bool {
	return itemType == model.ItemUserMessage || itemType == model.ItemAgentMessage || itemType == model.ItemReasoning
}

// newestClosesTwins names the item types whose twin candidate stays open only
// while it is the newest item of its type in the turn.
func newestClosesTwins(itemType model.ItemType) bool {
	return itemType == model.ItemAgentMessage || itemType == model.ItemReasoning
}

// twinBucket keys serialization-twin candidates by turn, role, item type and
// text, so an equal text of another type never makes a pair ambiguous.
func twinBucket(turn model.Turn, role model.Role, itemType model.ItemType, text string) string {
	return scopedKey(turn.Scope, "twin", turn.Key+"/"+string(role)+"/"+string(itemType)+"/"+text)
}
func (state *State) terminal(batch *model.Batch, turn model.Turn, obs model.Observation, outcome model.Outcome, reason string) {
	obs = bindObservation(obs, turn.Scope)
	obs.Kind, obs.Outcome = model.ObservationStop, outcome
	turn.Observations = append(append([]model.Observation(nil), turn.Observations...), obs)
	turn.Completion = append(append([]model.CompletionFact(nil), turn.Completion...), model.CompletionFact{Scope: turn.Scope, Outcome: outcome, Basis: model.BasisNative, ObservationRefs: []string{obs.ID}, Reason: reason})
	turn.Outcome, turn.Status = outcome, model.TurnUnknown
	switch outcome {
	case model.OutcomeCompleted:
		turn.Status = model.TurnCompleted
	case model.OutcomeInterrupted, model.OutcomeCancelled:
		turn.Status = model.TurnInterrupted
	case model.OutcomeRunning:
		turn.Status = model.TurnInProgress
	}
	turn.CompletionEvidence = model.CompletionTaskComplete
	if outcome == model.OutcomeInterrupted {
		turn.CompletionEvidence = model.CompletionTurnAborted
	}
	turn.Times = append(append([]model.TimeObservation(nil), turn.Times...), obs.Times...)
	turn.FinalItemRefs, turn.FinalItemID = nil, ""
	var final model.Item
	for _, item := range state.Items {
		if item.TurnKey == turn.Key && item.Type == model.ItemAgentMessage && item.SourceSeq > final.SourceSeq {
			final = item
		}
	}
	if outcome == model.OutcomeCompleted && final.ID != "" {
		if final.Finality.State != model.FinalityFinal {
			final.Finality = model.Finality{State: model.FinalityCandidate, Basis: model.BasisDerived, ObservationRefs: []string{obs.ID}}
			state.Items[final.ID] = final
			batch.Changes = append(batch.Changes, model.Change{Item: &final})
		}
		turn.FinalItemRefs = []string{final.ID}
		turn.FinalItemID = final.ID
	}
	batch.Observations = append(batch.Observations, obs)
	state.putTurn(batch, turn)
}
func nativeOutcome(payload map[string]json.RawMessage) model.Outcome {
	switch strings.ToLower(rawString(payload, "status")) {
	case "failed", "error":
		return model.OutcomeFailed
	case "declined":
		return model.OutcomeDeclined
	case "cancelled", "canceled":
		return model.OutcomeCancelled
	case "interrupted":
		return model.OutcomeInterrupted
	case "inprogress", "in_progress", "running":
		return model.OutcomeRunning
	}
	for _, field := range []string{"exitCode", "exit_code"} {
		if raw, ok := payload[field]; ok && string(raw) != "null" {
			var exit int64
			if json.Unmarshal(raw, &exit) == nil {
				if exit != 0 {
					return model.OutcomeFailed
				}
				return model.OutcomeCompleted
			}
		}
	}
	if raw, ok := payload["success"]; ok {
		if success, valid := nativeBool(raw); valid {
			if success {
				return model.OutcomeCompleted
			}
			return model.OutcomeFailed
		}
	}
	switch strings.ToLower(rawString(payload, "status")) {
	case "completed", "success":
		return model.OutcomeCompleted
	}
	return model.OutcomeUnknown
}
