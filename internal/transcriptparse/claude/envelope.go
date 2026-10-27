package claude

import (
	"bytes"
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A message row contains evidence outside its content blocks. Retain that
// envelope once, without copying content into every block's native detail.
func (state *State) messageEnvelope(batch *model.Batch, base model.Observation, turn model.Turn, row, body map[string]json.RawMessage) model.Observation {
	obs := base
	obs.ID += "/envelope"
	obs.Kind = model.ObservationMetadata
	if turn.Key != "" {
		obs.Scope = turn.Scope
	}
	fields, message := copyMap(row), copyMap(body)
	delete(message, "content")
	fields["message"] = mustJSON(message)
	obs.Detail = mustJSON(fields)
	obs.Times = append([]model.TimeObservation(nil), obs.Times...)
	for i := range obs.Times {
		obs.Times[i].Scope, obs.Times[i].ObservationRef = obs.Scope, obs.ID
	}
	batch.Observations = append(batch.Observations, obs)
	if turn.Key != "" {
		turn.Observations = append(append([]model.Observation(nil), turn.Observations...), obs)
		if effort := rawString(row, "effort"); effort != "" {
			turn.Effort = effort
		}
		if tier := rawString(rawObject(body, "usage"), "service_tier"); tier != "" {
			turn.ServiceTier = tier
		}
		state.putTurn(batch, turn)
	}
	return obs
}

func envelopeAliases(item model.Item, row map[string]json.RawMessage, ref string) model.Item {
	for _, field := range []struct{ name, namespace string }{{"uuid", "claude.rowUUID"}, {"promptId", "claude.promptId"}, {"requestId", "claude.apiRequestId"}, {"request_id", "claude.apiRequestId"}} {
		if rawString(row, field.name) == "" {
			continue
		}
		value := row[field.name]
		found := false
		for _, alias := range item.Aliases {
			if alias.Namespace == field.namespace && bytes.Equal(alias.Value, value) {
				found = true
				break
			}
		}
		if !found {
			item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: field.namespace, Value: mustJSON(value), Authority: model.BasisNative, ObservationRefs: []string{ref}})
		}
	}
	return item
}
