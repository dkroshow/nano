package grok

import (
	"reflect"
	"sort"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A missing prompt can inherit only a same-session event or unique call. A
// later explicit prompt can bind an open user segment; synthetic continuations
// cannot claim it. Old observations retain the identity known when they arrived.
func (state *State) bindScope(batch *model.Batch, scope model.Scope, update decodedUpdate, source model.SourceRef) model.Scope {
	if scope.NativeTurnID == nil && update.Kind == kindUserMessage && !update.IsReplay {
		if running, ok := state.RunningPrompts[sessionKey(scope)]; ok && running.Text != "" && running.Text == update.Text && running.Scope.NativeTurnID != nil && !isSyntheticPromptID(*running.Scope.NativeTurnID) {
			scope.NativeTurnID = running.Scope.NativeTurnID
			batch.Observations = append(batch.Observations, model.Observation{ID: sourceKey(source, "queue-binding"), Kind: model.ObservationMetadata, Scope: scope, Source: source, Capture: model.CaptureComplete, Detail: mustJSON(map[string]any{"basis": "derived", "rule": "same-session-exact-queue-text", "queueObservationRef": running.ObservationRef})})
		}
	}
	if scope.NativeTurnID == nil && update.EventID != "" {
		if item := state.Items[state.Events[scopedKey(scope, "event", update.EventID)]]; item.ID != "" {
			scope.NativeTurnID = item.Scope.NativeTurnID
			scope.TurnKey = item.TurnKey
			return scope
		}
	}
	if scope.NativeTurnID == nil && update.CallID != "" && update.Kind == kindToolUpdate {
		var candidate *model.Scope
		ambiguous := false
		for _, item := range state.Items {
			if item.Type != model.ItemToolCall || item.CallID != update.CallID || !sameSession(scope, item.Scope) {
				continue
			}
			if candidate != nil && !reflect.DeepEqual(candidate.NativeTurnID, item.Scope.NativeTurnID) {
				ambiguous = true
				break
			}
			copied := item.Scope
			candidate = &copied
		}
		if candidate != nil && !ambiguous {
			scope.NativeTurnID = candidate.NativeTurnID
			scope.TurnKey = candidate.TurnKey
		} else if ambiguous {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_call_scope", source))
		}
	}
	if scope.NativeTurnID == nil || isSyntheticPromptID(*scope.NativeTurnID) {
		return scope
	}
	group := scopedKey(scope, "turn", "")
	if _, exists := state.Turns[group]; exists {
		return scope
	}
	unbound := scope
	unbound.NativeTurnID = nil
	oldGroup := scopedKey(unbound, "turn", "")
	turn, exists := state.Turns[oldGroup]
	if !exists || turn.Outcome != model.OutcomeUnknown {
		return scope
	}
	user := state.Items[state.Segments[oldGroup]]
	if user.Type != model.ItemUserMessage || !sameSession(scope, user.Scope) || hasReplayAlias(user) {
		return scope
	}
	turn.Scope.NativeTurnID = scope.NativeTurnID
	turn.NativeTurnID = *scope.NativeTurnID
	binding := model.Observation{ID: sourceKey(source, "prompt-binding"), Kind: model.ObservationMetadata, Scope: turn.Scope, Source: source, Capture: model.CaptureComplete, Detail: mustJSON(map[string]any{"basis": "derived", "rule": "same-session-open-user-before-explicit-prompt", "promptId": scope.NativeTurnID})}
	turn.Observations = append(append([]model.Observation(nil), turn.Observations...), binding)
	delete(state.Turns, oldGroup)
	state.Turns[group] = turn
	if id := state.Segments[oldGroup]; id != "" {
		state.Segments[group] = id
		delete(state.Segments, oldGroup)
	}
	items := make([]model.Item, 0)
	for _, item := range state.Items {
		if item.TurnKey == turn.Key {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
	for _, item := range items {
		item.Scope.NativeTurnID = scope.NativeTurnID
		state.Items[item.ID] = item
		for _, obs := range item.Observations {
			if obs.NativeEventID != "" {
				state.Events[scopedKey(item.Scope, "event", obs.NativeEventID)] = item.ID
			}
		}
		copy := item
		batch.Changes = append(batch.Changes, model.Change{Item: &copy})
	}
	batch.Observations = append(batch.Observations, binding)
	batch.Changes = append(batch.Changes, model.Change{Turn: &turn})
	return scope
}
func sameSession(a, b model.Scope) bool {
	return a.Provider == b.Provider && a.Source == b.Source && a.Incarnation == b.Incarnation && a.ConnectionID == b.ConnectionID && reflect.DeepEqual(a.NativeThreadID, b.NativeThreadID)
}

func hasReplayAlias(item model.Item) bool {
	for _, alias := range item.Aliases {
		if alias.Namespace == "grok.isReplay" && string(alias.Value) == "true" {
			return true
		}
	}
	return false
}
