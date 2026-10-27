package claude

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func sameConnection(a, b model.Scope) bool {
	return a.Provider == b.Provider && a.Source == b.Source && a.Incarnation == b.Incarnation && a.ConnectionID == b.ConnectionID
}

// Within one source connection, the first explicit root session ID can label
// preceding source-local evidence.
// Known sessions and nested/worker identities never supply this binding proof.
func (state *State) bindFirstNativeThread(batch *model.Batch, sourceScope model.Scope, native string, source model.SourceRef) {
	unknown := state.Catalog[catalogKey(sourceScope)] != nil
	// Source-wide title/preview precedence survives intervening worker traffic.
	// This moves only the continuation cache; it does not reattribute earlier
	// observations or relax the item/turn binding guards below.
	if prior := state.Catalog[catalogKey(sourceScope)]; prior != nil {
		delete(state.Catalog, catalogKey(sourceScope))
		bound := sourceScope
		bound.NativeThreadID = &native
		state.Catalog[catalogKey(bound)] = prior
	}
	for _, turn := range state.Turns {
		if !sameConnection(turn.Scope, sourceScope) {
			continue
		}
		if turn.Scope.NativeThreadID != nil {
			return
		}
		unknown = true
	}
	for _, item := range state.Items {
		if sameConnection(item.Scope, sourceScope) {
			if item.Scope.NativeThreadID != nil {
				return
			}
			unknown = true
		}
	}
	for _, control := range state.Controls {
		if sameConnection(control.Request.Scope, sourceScope) {
			if control.Request.Scope.NativeThreadID != nil {
				return
			}
			unknown = true
		}
	}
	if !unknown {
		return
	}
	bindKey := func(key string) string {
		var parts []json.RawMessage
		if json.Unmarshal([]byte(key), &parts) != nil || len(parts) != 9 || !bytes.Equal(parts[4], []byte("null")) {
			return key
		}
		var provider, label, incarnation, connection string
		_ = json.Unmarshal(parts[0], &provider)
		_ = json.Unmarshal(parts[1], &label)
		_ = json.Unmarshal(parts[2], &incarnation)
		_ = json.Unmarshal(parts[3], &connection)
		if provider != string(sourceScope.Provider) || label != sourceScope.Source || incarnation != sourceScope.Incarnation || connection != sourceScope.ConnectionID {
			return key
		}
		parts[4] = mustJSON(native)
		return string(mustJSON(parts))
	}
	state.ActiveTurns = rekey(state.ActiveTurns, bindKey)
	state.ActiveMessages = rekey(state.ActiveMessages, bindKey)
	for key, value := range state.ActiveMessages {
		state.ActiveMessages[key] = bindKey(value)
	}
	state.Messages = rekey(state.Messages, bindKey)
	state.Rows = rekey(state.Rows, bindKey)
	state.RowEvents = rekey(state.RowEvents, bindKey)
	state.Calls = rekey(state.Calls, bindKey)
	state.Controls = rekey(state.Controls, bindKey)
	items := make([]model.Item, 0, len(state.Items))
	for _, item := range state.Items {
		if sameConnection(item.Scope, sourceScope) && item.Scope.NativeThreadID == nil {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
	for _, item := range items {
		item.Scope.NativeThreadID = &native
		obs := bindingObservation(source, item.Scope, native, item.ID)
		item.Observations = append(append([]model.Observation(nil), item.Observations...), obs)
		item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: "claude.sessionId", Value: mustJSON(native), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}})
		state.Items[item.ID] = item
		batch.Observations = append(batch.Observations, obs)
		copy := item
		batch.Changes = append(batch.Changes, model.Change{Item: &copy})
	}
	turns := make([]model.Turn, 0, len(state.Turns))
	for _, turn := range state.Turns {
		if sameConnection(turn.Scope, sourceScope) && turn.Scope.NativeThreadID == nil {
			turns = append(turns, turn)
		}
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].Ordinal < turns[j].Ordinal })
	for _, turn := range turns {
		turn.Scope.NativeThreadID = &native
		obs := bindingObservation(source, turn.Scope, native, turn.Key)
		turn.Observations = append(append([]model.Observation(nil), turn.Observations...), obs)
		batch.Observations = append(batch.Observations, obs)
		state.putTurn(batch, turn)
	}
}

func bindingObservation(source model.SourceRef, scope model.Scope, native, key string) model.Observation {
	return model.Observation{ID: sourceKey(source, "thread-binding:"+key), Kind: model.ObservationMetadata, Scope: scope, Source: source, Capture: model.CaptureComplete, Detail: mustJSON(map[string]string{"nativeSessionId": native, "basis": "first_native_session_on_source", "priorScope": "source_local"})}
}

func rekey[V any](values map[string]V, bind func(string) string) map[string]V {
	out := make(map[string]V, len(values))
	for key, value := range values {
		out[bind(key)] = value
	}
	return out
}
