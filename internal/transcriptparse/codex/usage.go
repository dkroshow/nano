package codex

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"strconv"
)

func usageValues(fields map[string]json.RawMessage) (model.TokenUsage, bool) {
	usage := model.TokenUsage{Contract: model.UsageContractPerTurnV1, Native: mustJSON(fields)}
	invalid := false
	for _, counter := range []struct {
		names  []string
		target **int64
	}{
		{[]string{"input_tokens", "inputTokens"}, &usage.Input}, {[]string{"output_tokens", "outputTokens"}, &usage.Output},
		{[]string{"reasoning_output_tokens", "reasoningOutputTokens"}, &usage.Reasoning}, {[]string{"cached_input_tokens", "cachedInputTokens"}, &usage.CacheRead},
		{[]string{"cache_write_input_tokens", "cacheWriteInputTokens"}, &usage.CacheWrite}, {[]string{"total_tokens", "totalTokens"}, &usage.Total}, {[]string{"model_context_window", "modelContextWindow"}, &usage.ContextWindow},
	} {
		for _, name := range counter.names {
			if raw, ok := fields[name]; ok {
				if string(raw) != "null" {
					var value int64
					if json.Unmarshal(raw, &value) == nil && value >= 0 {
						*counter.target = &value
					} else {
						invalid = true
					}
				}
				break
			}
		}
	}
	if usage.CacheRead != nil {
		value := *usage.CacheRead
		usage.Cached = &value
	}
	return usage, invalid
}
func (state *State) addUsage(batch *model.Batch, turn model.Turn, obs model.Observation, fields map[string]json.RawMessage, grain model.UsageGrain, sample, semantics string) model.Turn {
	if len(fields) == 0 {
		return turn
	}
	values, invalid := usageValues(fields)
	if invalid {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_usage_counter", obs.Source))
	}
	scope := turn.Scope
	if grain == model.UsageThread {
		scope.Kind, scope.TurnKey, scope.NativeTurnID = model.ScopeThread, "", nil
	}
	usage := model.UsageObservation{ID: obs.ID + "/usage/" + string(grain), Scope: scope, Grain: grain, SampleID: sample, Semantics: semantics, Values: values, ObservationRefs: []string{obs.ID}}
	turn.UsageObservations = append(turn.UsageObservations, usage)
	return turn
}

// finishUsage saves the observations a record added to turn.UsageObservations
// and summarizes the turn's whole list.
func (state *State) finishUsage(batch *model.Batch, turn model.Turn, obs model.Observation) {
	for _, usage := range turn.UsageObservations {
		state.appendUsage(turn.Key, usage)
	}
	turn.UsageObservations = state.usageOf(turn.Key)
	var diagnostics []model.Diagnostic
	turn, diagnostics = assemble.SummarizeUsage(turn)
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	state.putTurn(batch, turn)
	batch.Observations = append(batch.Observations, bindObservation(obs, turn.Scope))
}
func (state *State) usageRecord(batch *model.Batch, turn model.Turn, obs model.Observation, payload map[string]json.RawMessage) {
	state.observeUsageTotal(batch, obs, rawObject(payload, "thread_token_usage"))
	turn = state.addUsage(batch, turn, obs, rawObject(payload, "usage"), model.UsageResponse, rawString(payload, "response_id"), "snapshot")
	semantics := "unbound"
	if turn.Scope.NativeTurnID != nil {
		semantics = "snapshot"
	}
	turn = state.addUsage(batch, turn, obs, rawObject(payload, "turn_token_usage"), model.UsageTurn, turn.NativeTurnID, semantics)
	turn = state.addUsage(batch, turn, obs, rawObject(payload, "thread_token_usage"), model.UsageThread, "", "snapshot")
	state.finishUsage(batch, turn, obs)
}
func (state *State) tokenCount(batch *model.Batch, obs model.Observation, payload map[string]json.RawMessage) {
	info := rawObject(payload, "info")
	if info == nil {
		// A rate-limit-only update has no token sample or turn boundary.
		obs.Scope.Kind, obs.Scope.TurnKey, obs.Scope.NativeTurnID = model.ScopeThread, "", nil
		batch.Observations = append(batch.Observations, bindObservation(obs, obs.Scope))
		return
	}
	turn := state.turn(obs.Scope, obs.Source, false, false)
	last, total := rawObject(info, "last_token_usage"), rawObject(info, "total_token_usage")
	if last == nil {
		last = rawObject(info, "last")
	}
	if total == nil {
		total = rawObject(info, "total")
	}
	// A last-call sample has no native response ID. Its last+total tuple is a
	// derived alias for repeated snapshots, never a manufactured native ID.
	signature := sha256.Sum256(mustJSON([]any{last, total}))
	sample := fmt.Sprintf("snapshot:%x", signature[:])
	if len(last) > 0 {
		for _, earlier := range state.usageOf(turn.Key) {
			if earlier.Grain == model.UsageResponse && earlier.SampleID == sample {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("repeated_usage_snapshot", obs.Source))
				break
			}
		}
	}
	state.observeUsageTotal(batch, obs, total)
	turn = state.addUsage(batch, turn, obs, last, model.UsageResponse, sample, "snapshot")
	turn = state.addUsage(batch, turn, obs, total, model.UsageThread, "", "snapshot")
	capacity, invalid := usageValues(info)
	if invalid {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_usage_counter", obs.Source))
	}
	if capacity.ContextWindow != nil {
		for i := range turn.UsageObservations {
			turn.UsageObservations[i].Values.ContextWindow = capacity.ContextWindow
		}
	}
	state.finishUsage(batch, turn, obs)
}

func (state *State) observeUsageTotal(batch *model.Batch, obs model.Observation, fields map[string]json.RawMessage) {
	if len(fields) == 0 {
		return
	}
	next, _ := usageValues(fields)
	key := threadKey(obs.Scope)
	prior := state.UsageTotals[key]
	for _, pair := range [][2]*int64{
		{prior.Input, next.Input}, {prior.Output, next.Output}, {prior.Reasoning, next.Reasoning},
		{prior.CacheRead, next.CacheRead}, {prior.CacheWrite, next.CacheWrite}, {prior.Total, next.Total},
	} {
		if pair[0] != nil && pair[1] != nil && *pair[1] < *pair[0] {
			// A decrease alone cannot distinguish a reset from stale/replayed
			// evidence. Retain it without inventing an epoch or a contribution.
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("usage_counter_decrease", obs.Source))
			break
		}
	}
	state.UsageTotals[key] = next
}

func usageKey(turnKey string, index int) string {
	return turnKey + "#" + strconv.Itoa(index)
}

type turnChange struct {
	turn  *model.Turn
	count int
}

// fillUsage gives each turn change this call emitted the usage list its turn
// had then, building each turn's list once. With a key, it fills only that
// turn's changes, before eviction drops the turn's entries.
func (state *State) fillUsage(key string) {
	lists := map[string][]model.UsageObservation{}
	var pending []turnChange
	for _, change := range state.turnChanges {
		if key != "" && change.turn.Key != key {
			pending = append(pending, change)
			continue
		}
		if change.count == 0 {
			continue
		}
		list, ok := lists[change.turn.Key]
		if !ok {
			list = state.usageOf(change.turn.Key)
			lists[change.turn.Key] = list
		}
		change.turn.UsageObservations = list[:change.count:change.count]
	}
	state.turnChanges = pending
}

func (state *State) appendUsage(turnKey string, usage model.UsageObservation) {
	index := state.UsageCounts[turnKey]
	state.Usage[usageKey(turnKey, index)] = usage
	state.markChanged("usage", usageKey(turnKey, index))
	state.UsageCounts[turnKey] = index + 1
}

// usageOf returns a turn's usage observations in arrival order.
func (state State) usageOf(turnKey string) []model.UsageObservation {
	count := state.UsageCounts[turnKey]
	if count == 0 {
		return nil
	}
	list := make([]model.UsageObservation, count)
	for i := range list {
		list[i] = state.Usage[usageKey(turnKey, i)]
	}
	return list
}
