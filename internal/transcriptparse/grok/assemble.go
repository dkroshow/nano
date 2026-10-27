package grok

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The kind/status/tool mappings are extracted from history/transcript_grok.go.
// Ordered observations replace that parser's adjacent-text deduplication.
func (state *State) apply(batch *model.Batch, input model.Input, source model.SourceRef, update decodedUpdate) {
	if source.BlockPath == "$.msg" {
		source.BlockPath += ".params.update"
	} else if source.BlockPath == "" {
		source.BlockPath = "$.params.update"
	}
	scope := scopeFrom(input, update, model.ScopeTurn)
	promptIndex, hasPromptIndex := nativePromptIndex(update)
	if hasPromptIndex {
		state.separateIndexedPrompt(scope, promptIndex, update)
	}
	if update.Kind == kindMetadata {
		scope.Kind, scope.NativeTurnID = model.ScopeThread, nil
	} else if !update.IsReplay || state.hasReplayTarget(scope, update.EventID) {
		scope = state.bindScope(batch, scope, update, source)
	}
	observation := model.Observation{
		ID: sourceKey(source, "update"), Scope: scope, Source: source,
		NativeEventID: update.EventID, Detail: mustJSON(update.Payload), Capture: model.CaptureComplete,
	}
	if update.EventID != "" {
		observation.NativeEventDigest = updateEventDigest(update)
	}
	observation.Times = updateTimes(input, update, scope, observation.ID)
	for i := range observation.Times {
		stamp := &observation.Times[i]
		if stamp.Value != nil && (stamp.Value.Year() < 0 || stamp.Value.Year() > 9999) {
			stamp.Value = nil
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("timestamp_out_of_range", source))
		}
	}
	state.checkTimes(batch, observation)
	if update.IsReplay && update.EventID != "" && !state.hasReplayTarget(scope, update.EventID) {
		emitNativeReplay(batch, observation, update)
		return
	}
	if update.Kind == kindTask {
		state.applyTask(batch, source, observation, update)
		return
	}
	if update.Kind == kindMetadata || update.Kind == kindHook || update.Kind == kindHiddenUser || update.Kind == kindSubagentProgress || update.Kind == kindSubagentDiagnostic || update.Kind == kindUnclassified {
		observation.Kind = model.ObservationMetadata
		if update.Kind == kindUnclassified {
			observation.Kind = model.ObservationUnknown
		}
		if scope.NativeTurnID == nil {
			observation.Scope.Kind = model.ScopeThread
		}
		for i := range observation.Times {
			observation.Times[i].Scope = observation.Scope
		}
		batch.Observations = append(batch.Observations, observation)
		return
	}
	if update.Kind == kindTerminal && scope.NativeTurnID == nil {
		observation.Scope.Kind = model.ScopeThread
		for i := range observation.Times {
			observation.Times[i].Scope = observation.Scope
		}
		observation.Kind = model.ObservationStop
		observation.Outcome = terminalOutcome(update.Status)
		batch.Observations = append(batch.Observations, observation)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unscoped_terminal", source))
		return
	}
	group := state.turnGroup(scope)
	turn, exists := state.Turns[group]
	if !exists {
		state.NextTurn++
		turn = model.Turn{
			Key: sourceKey(source, "turn"), Ordinal: state.NextTurn, Scope: scope,
			Capture: model.CaptureUnknown, Outcome: model.OutcomeUnknown,
			Status: model.TurnUnknown, CompletionEvidence: model.CompletionNone,
		}
		if scope.NativeTurnID != nil {
			turn.NativeTurnID = *scope.NativeTurnID
		}
		turn.Scope.TurnKey = turn.Key
	}
	scope.TurnKey = turn.Key
	observation.Scope = scope
	if update.Kind == kindTerminal {
		state.applyTerminal(batch, group, turn, observation, update)
		return
	}
	itemType, category, role := model.ItemProviderEvent, model.CategoryUnclassified, model.RoleSystem
	observation.Kind = model.ObservationSnapshot
	observation.Text = update.Text
	switch update.Kind {
	case kindUserMessage:
		itemType, category, role = model.ItemUserMessage, model.CategoryMessages, model.RoleUser
		observation.Kind = model.ObservationDelta
	case kindAgentMessage:
		itemType, category, role = model.ItemAgentMessage, model.CategoryMessages, model.RoleAssistant
		observation.Kind = model.ObservationDelta
	case kindReasoning:
		itemType, category, role = model.ItemReasoning, model.CategoryReasoning, model.RoleAssistant
		observation.Kind = model.ObservationDelta
	case kindToolCall, kindToolUpdate:
		itemType, category, role = model.ItemToolCall, model.CategoryToolCall, model.RoleAssistant
		observation.Text = update.Title
		observation.Outcome = toolOutcome(update.Status)
		if update.Kind == kindToolUpdate {
			observation.Kind = model.ObservationProgress
		}
		if update.Kind == kindToolUpdate && isTerminal(observation.Outcome) {
			itemType, category, role = model.ItemToolResult, model.CategoryToolResult, model.RoleTool
			observation.Kind = model.ObservationResult
			complete := true
			observation.Complete = &complete
		}
	case kindPlan:
		itemType, category, role = model.ItemPlan, model.CategorySystem, model.RoleAssistant
		complete := true
		observation.Complete = &complete
	case kindSubagentSpawn, kindSubagentFinish:
		itemType, category, role = model.ItemNotice, model.CategorySubagent, model.RoleSystem
		if update.Subagent != nil {
			observation.Text = update.Subagent.Description
		}
	}
	parts, hasParts := updateParts(update, source)
	if itemType == model.ItemToolResult {
		if text := partsText(parts); text != "" {
			observation.Text = text
		}
	}
	if itemType == model.ItemPlan {
		observation.Text = partsText(parts)
	}
	eventKey := scopedKey(scope, "event", update.EventID)
	itemID := ""
	if update.EventID != "" {
		itemID = state.Events[eventKey]
	}
	callKey := scopedKey(scope, string(itemType), update.CallID)
	if itemID == "" && update.CallID != "" {
		itemID = state.Calls[callKey]
	}
	if itemID == "" && observation.Kind == model.ObservationDelta {
		candidate := state.Items[state.Segments[group]]
		if candidate.Type == itemType {
			itemID = candidate.ID
		}
	}
	item, exists := state.Items[itemID]
	if !exists {
		state.NextItem++
		itemID = sourceKey(source, string(itemType))
		item = model.Item{
			ID: itemID, SourceKey: itemID, SourceSeq: state.NextItem, Scope: scope,
			TurnKey: turn.Key, TurnOrdinal: turn.Ordinal, Type: itemType, Category: category, Role: role,
			CallID: update.CallID, ToolName: update.ToolName, Detail: mustJSON(update.Payload),
			SourceLine: int(source.Line), SourceRowType: "session/update", SourcePayloadType: string(update.Kind),
			SourceLineSHA256: source.SHA256, Synthetic: isSyntheticPromptID(update.PromptID),
		}
		item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
		if update.CallID != "" {
			item.Aliases = []model.Alias{{Namespace: "grok.toolCallId", Value: mustJSON(update.CallID), Authority: model.BasisNative, ObservationRefs: []string{observation.ID}}}
		}
		if turn.StartSourceSeq == 0 {
			turn.StartSourceSeq = item.SourceSeq
		}
	}
	if itemType == model.ItemUserMessage {
		if key := contentSubmissionID(parts); key != "" {
			found := false
			for _, alias := range item.Aliases {
				found = found || alias.Namespace == "nano.submissionId" && string(alias.Value) == string(mustJSON(key))
			}
			if !found {
				item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: "nano.submissionId", Value: mustJSON(key), Authority: model.BasisNative, ObservationRefs: []string{observation.ID}})
			}
		}
	}
	if hasPromptIndex {
		found := false
		for _, alias := range item.Aliases {
			if alias.Namespace == "grok.promptIndex" && string(alias.Value) == string(mustJSON(promptIndex)) {
				found = true
			}
		}
		if !found {
			item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: "grok.promptIndex", Value: mustJSON(promptIndex), Authority: model.BasisNative, ObservationRefs: []string{observation.ID}})
		}
	}
	if update.IsReplay && !hasReplayAlias(item) {
		item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: "grok.isReplay", Value: mustJSON(true), Authority: model.BasisNative, ObservationRefs: []string{observation.ID}})
	}
	observation.Scope = item.Scope
	for i := range observation.Times {
		observation.Times[i].Scope = item.Scope
	}
	var diagnostics []model.Diagnostic
	priorRelations := len(item.Relations)
	item, diagnostics = assemble.Observe(item, observation)
	contentChanged := len(diagnostics) == 0 && len(item.Relations) == priorRelations
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	batch.Observations = append(batch.Observations, observation)
	if update.EventID != "" {
		state.Events[eventKey] = item.ID
	}
	if update.CallID != "" {
		state.Calls[callKey] = item.ID
	}
	if itemType == model.ItemToolResult && len(item.Relations) == 0 {
		call := state.Items[state.Calls[scopedKey(scope, string(model.ItemToolCall), update.CallID)]]
		item, diagnostics = assemble.LinkResult(call, item)
		batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	}
	if exists && contentChanged && itemType == model.ItemToolCall && update.Kind == kindToolUpdate {
		item.Relations = append(append([]model.Relation(nil), item.Relations...), model.Relation{
			Kind: model.RelationProgressOf, Scope: item.Scope, FromObservation: observation.ID,
			ToItem: item.ID, ObservationRefs: []string{observation.ID},
		})
	}
	if contentChanged {
		if hasParts {
			if observation.Kind == model.ObservationDelta {
				item.Parts = append(append([]model.Part(nil), item.Parts...), parts...)
			} else {
				item.Parts = parts
			}
		}
		item.Detail = mustJSON(update.Payload)
		if observation.Kind == model.ObservationDelta {
			state.Segments[group] = item.ID
		} else {
			delete(state.Segments, group)
		}
		if itemType == model.ItemAgentMessage {
			state.LastAssistant[group] = item.ID
		}
	}
	state.Items[item.ID] = item
	if item.SourceSeq > turn.EndSourceSeq {
		turn.EndSourceSeq = item.SourceSeq
	}
	state.Turns[group] = turn
	batch.Changes = append(batch.Changes, model.Change{Item: &item}, model.Change{Turn: &turn})
}

func (state *State) applyTerminal(batch *model.Batch, group string, turn model.Turn, observation model.Observation, update decodedUpdate) {
	observation.Kind = model.ObservationStop
	observation.Scope = turn.Scope
	for i := range observation.Times {
		observation.Times[i].Scope = turn.Scope
	}
	observation.Outcome = terminalOutcome(update.Status)
	for _, prior := range turn.Observations {
		if observation.NativeEventID == "" || prior.NativeEventID != observation.NativeEventID || prior.Kind != model.ObservationStop {
			continue
		}
		turn.Observations = append(append([]model.Observation(nil), turn.Observations...), observation)
		batch.Observations = append(batch.Observations, observation)
		if !assemble.SameEvidence(prior, observation) {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("native_terminal_conflict", observation.Source))
		} else {
			relation := model.Relation{Kind: model.RelationReplayOf, Scope: turn.Scope, FromObservation: observation.ID, ToObservation: prior.ID, ObservationRefs: []string{prior.ID, observation.ID}}
			batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
		}
		state.Turns[group] = turn
		batch.Changes = append(batch.Changes, model.Change{Turn: &turn})
		return
	}
	turn.Observations = append(append([]model.Observation(nil), turn.Observations...), observation)
	turn.Completion = append(append([]model.CompletionFact(nil), turn.Completion...), model.CompletionFact{
		Scope: turn.Scope, Outcome: observation.Outcome, Basis: model.BasisNative,
		ObservationRefs: []string{observation.ID}, Reason: update.Status,
	})
	turn.Outcome = observation.Outcome
	if !update.IsReplay && isTerminal(turn.Outcome) {
		key := sessionKey(turn.Scope)
		if running, ok := state.RunningPrompts[key]; ok && running.Scope.NativeTurnID != nil && turn.Scope.NativeTurnID != nil && *running.Scope.NativeTurnID == *turn.Scope.NativeTurnID {
			delete(state.RunningPrompts, key)
		}
	}
	turn.Status = model.TurnUnknown
	turn.CompletionEvidence = model.CompletionStopReason
	switch turn.Outcome {
	case model.OutcomeCompleted:
		turn.Status = model.TurnCompleted
	case model.OutcomeInterrupted, model.OutcomeCancelled:
		turn.Status = model.TurnInterrupted
		turn.CompletionEvidence = model.CompletionTurnAborted
	}
	turn.Times = append(append([]model.TimeObservation(nil), turn.Times...), observation.Times...)
	for _, stamp := range observation.Times {
		if stamp.Class == model.TimeNative && stamp.Value != nil {
			value := *stamp.Value
			turn.CompletedAt = &value
			break
		}
	}
	if len(update.Usage) > 0 && string(update.Usage) != "null" {
		usage, valid := usageObservation(update.Usage, turn.Scope, observation.ID)
		if !valid {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_usage", observation.Source))
		}
		turn.UsageObservations = append(append([]model.UsageObservation(nil), turn.UsageObservations...), usage)
		var diagnostics []model.Diagnostic
		turn, diagnostics = assemble.SummarizeUsage(turn)
		batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	}
	batch.Observations = append(batch.Observations, observation)
	if turn.Outcome != model.OutcomeCompleted {
		for _, id := range turn.FinalItemRefs {
			item := state.Items[id]
			item.IsFinalAnswer = false
			item.Finality = model.Finality{State: model.FinalityRetracted, Basis: model.BasisDerived, ObservationRefs: []string{observation.ID}}
			state.Items[id] = item
			batch.Changes = append(batch.Changes, model.Change{Item: &item})
		}
		turn.FinalItemRefs, turn.FinalItemID = nil, ""
	}
	if turn.Outcome == model.OutcomeCompleted {
		if id := state.LastAssistant[group]; id != "" {
			item := state.Items[id]
			complete := true
			closure := model.Observation{
				ID: observation.ID + "/content-stop", Kind: model.ObservationStop, Scope: item.Scope,
				Source: observation.Source, Complete: &complete,
				Detail: mustJSON(map[string]any{"basis": "derived", "observationRefs": []string{observation.ID}}),
			}
			var diagnostics []model.Diagnostic
			item, diagnostics = assemble.Observe(item, closure)
			batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
			batch.Observations = append(batch.Observations, closure)
			item.Finality = model.Finality{State: model.FinalityFinal, Basis: model.BasisDerived, ObservationRefs: []string{observation.ID}}
			item.IsFinalAnswer = true
			state.Items[id] = item
			turn.FinalItemID = id
			turn.FinalItemRefs = []string{id}
			batch.Changes = append(batch.Changes, model.Change{Item: &item})
		}
	}
	state.Turns[group] = turn
	batch.Changes = append(batch.Changes, model.Change{Turn: &turn})
}

func updateTimes(input model.Input, update decodedUpdate, scope model.Scope, ref string) []model.TimeObservation {
	meaning := model.TimeEvent
	if update.Kind == kindTerminal {
		meaning = model.TimeTerminal
	}
	event := model.TimeObservation{
		Scope: scope, Class: model.TimeNative, SourceField: "params._meta.agentTimestampMs",
		Precision: "millisecond", Meaning: meaning, ObservationRef: ref,
	}
	if update.AgentTimestampMS != nil {
		value := time.UnixMilli(*update.AgentTimestampMS).UTC()
		event.Value = &value
		event.Raw = mustJSON(*update.AgentTimestampMS)
	}
	times := []model.TimeObservation{event}
	if update.FileTimestamp != nil {
		value := time.Unix(*update.FileTimestamp, 0).UTC()
		times = append(times, model.TimeObservation{
			Scope: scope, Class: model.TimeNative, Value: &value, Raw: mustJSON(*update.FileTimestamp),
			SourceField: "timestamp", Precision: "second", Meaning: model.TimeWrite, ObservationRef: ref,
		})
	}
	for _, clock := range []struct {
		value   *time.Time
		class   model.TimeClass
		field   string
		meaning model.TimeMeaning
	}{{input.CaptureAt, model.TimeCapture, "caller.captureAt", model.TimeWrite}, {input.ObservedAt, model.TimeObserved, "caller.observedAt", model.TimeEvent}} {
		if clock.value != nil {
			value := clock.value.UTC()
			times = append(times, model.TimeObservation{
				Scope: scope, Value: &value, Class: clock.class, SourceField: clock.field,
				Precision: "caller_supplied", Meaning: clock.meaning, ObservationRef: ref,
			})
		}
	}
	return times
}

func toolOutcome(status string) model.Outcome {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "in_progress", "running":
		return model.OutcomeRunning
	case "completed", "success":
		return model.OutcomeCompleted
	case "failed", "error":
		return model.OutcomeFailed
	case "cancelled", "canceled":
		return model.OutcomeCancelled
	case "interrupted":
		return model.OutcomeInterrupted
	default:
		return model.OutcomeUnknown
	}
}

func terminalOutcome(status string) model.Outcome {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "end_turn":
		return model.OutcomeCompleted
	case "provider_error":
		return model.OutcomeFailed
	}
	switch outcome := toolOutcome(status); outcome {
	case model.OutcomeFailed, model.OutcomeCancelled, model.OutcomeInterrupted:
		return outcome
	default:
		return model.OutcomeUnknown
	}
}

func isTerminal(outcome model.Outcome) bool {
	return outcome == model.OutcomeCompleted || outcome == model.OutcomeFailed || outcome == model.OutcomeCancelled || outcome == model.OutcomeInterrupted
}

// Counter extraction follows history.grokTokenUsage; all native samples and
// model buckets are retained instead of reducing immediately to a turn total.
func usageObservation(raw json.RawMessage, scope model.Scope, ref string) (model.UsageObservation, bool) {
	var native struct {
		Incomplete *bool                      `json:"usageIsIncomplete"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	valid := json.Unmarshal(raw, &native) == nil
	values, countersValid := usageValues(raw)
	valid = valid && countersValid
	usage := model.UsageObservation{
		ID: ref + "/usage", Scope: scope, Grain: model.UsageTurn, SampleID: scope.TurnKey,
		Semantics: "snapshot", Values: values, Incomplete: native.Incomplete, ObservationRefs: []string{ref},
	}
	if native.ModelUsage != nil {
		usage.ModelUsage = map[string]model.TokenUsage{}
		for name, raw := range native.ModelUsage {
			values, bucketValid := usageValues(raw)
			valid = valid && bucketValid
			usage.ModelUsage[name] = values
		}
	}
	return usage, valid
}

func usageValues(raw json.RawMessage) (model.TokenUsage, bool) {
	usage := model.TokenUsage{Native: append(json.RawMessage(nil), raw...), Contract: model.UsageContractPerTurnV1}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return usage, false
	}
	valid := true
	for _, field := range []struct {
		name  string
		value **int64
	}{
		{"inputTokens", &usage.Input}, {"outputTokens", &usage.Output}, {"totalTokens", &usage.Total},
		{"reasoningTokens", &usage.Reasoning}, {"cachedReadTokens", &usage.CacheRead}, {"cacheCreationTokens", &usage.CacheWrite},
	} {
		raw, ok := fields[field.name]
		if !ok || string(raw) == "null" {
			continue
		}
		var value int64
		if json.Unmarshal(raw, &value) != nil {
			valid = false
			continue
		}
		*field.value = &value
	}
	usage.Cached = usage.CacheRead
	return usage, valid
}
