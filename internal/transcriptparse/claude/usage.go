package claude

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func nativeCounter(raw json.RawMessage) (*int64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return nil, true
	}
	return &value, false
}

func decodeUsage(fields map[string]json.RawMessage, camel bool) (model.TokenUsage, bool) {
	normalized := fields
	if camel {
		normalized = copyMap(fields)
		for _, pair := range [][2]string{{"inputTokens", "input_tokens"}, {"outputTokens", "output_tokens"}, {"cacheReadInputTokens", "cache_read_input_tokens"}, {"cacheCreationInputTokens", "cache_creation_input_tokens"}} {
			if raw, exists := fields[pair[0]]; exists {
				normalized[pair[1]] = raw
			}
		}
	}
	invalid := false
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		_, bad := nativeCounter(normalized[key])
		invalid = invalid || bad
	}
	for _, nested := range []struct {
		name     string
		counters []string
	}{
		{"output_tokens_details", []string{"thinking_tokens"}},
		{"cache_creation", []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"}},
	} {
		raw := normalized[nested.name]
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		fields := rawObject(normalized, nested.name)
		if fields == nil {
			invalid = true
			continue
		}
		for _, key := range nested.counters {
			_, bad := nativeCounter(fields[key])
			invalid = invalid || bad
		}
	}
	values := messageUsage(map[string]json.RawMessage{"usage": mustJSON(normalized)})
	if values == nil {
		values = &model.TokenUsage{Contract: model.UsageContractPerTurnV1}
	}
	native := map[string]json.RawMessage{}
	if len(values.Native) > 0 {
		_ = json.Unmarshal(values.Native, &native)
	}
	native["usage"] = mustJSON(fields)
	values.Native = mustJSON(native)
	return *values, invalid
}

func (state *State) resultUsage(batch *model.Batch, turnKey string, observation model.Observation, row map[string]json.RawMessage) {
	fields, buckets := rawObject(row, "usage"), rawObject(row, "modelUsage")
	if len(fields) == 0 && len(buckets) == 0 {
		return
	}
	values, invalid := decodeUsage(fields, false)
	models := map[string]model.TokenUsage{}
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fields := rawObject(buckets, name)
		if fields == nil {
			invalid = true
			continue
		}
		modelValues, bad := decodeUsage(fields, true)
		models[name] = modelValues
		invalid = invalid || bad
	}
	if invalid {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_usage_counter", observation.Source))
	}
	turn := state.Turns[turnKey]
	usage := model.UsageObservation{ID: observation.ID + "/usage", Scope: turn.Scope, Grain: model.UsageTurn, Semantics: "result_aggregate", Values: values, ModelUsage: models, ObservationRefs: []string{observation.ID}}
	turn.UsageObservations = append(append([]model.UsageObservation(nil), turn.UsageObservations...), usage)
	state.putTurn(batch, turn)
}
