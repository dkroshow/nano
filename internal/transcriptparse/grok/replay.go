package grok

import "github.com/dkroshow/nano/internal/transcriptparse/model"

func (state State) hasReplayTarget(scope model.Scope, event string) bool {
	if event == "" {
		return false
	}
	if state.Events[scopedKey(scope, "event", event)] != "" {
		return true
	}
	for _, obs := range state.Turns[scopedKey(scope, "turn", "")].Observations {
		if obs.NativeEventID == event {
			return true
		}
	}
	return false
}

// Replay is explicit native evidence even when its earlier target is absent
// from this capture. The consumer resolves the endpoint or retains the incoming
// observation with an unresolved/ambiguous diagnostic; no turn is reopened.
func emitNativeReplay(batch *model.Batch, obs model.Observation, update decodedUpdate) {
	obs.Scope.ItemID, obs.Scope.TurnKey = "", ""
	obs.Kind, obs.Text = model.ObservationSnapshot, update.Text
	switch update.Kind {
	case kindUserMessage, kindAgentMessage, kindReasoning:
		obs.Kind = model.ObservationDelta
	case kindTerminal:
		obs.Kind, obs.Outcome = model.ObservationStop, terminalOutcome(update.Status)
	case kindToolCall, kindToolUpdate:
		obs.Text, obs.Outcome = update.Title, toolOutcome(update.Status)
		if update.Kind == kindToolUpdate {
			obs.Kind = model.ObservationProgress
		}
	}
	batch.Observations = append(batch.Observations, obs)
	relation := model.Relation{Kind: model.RelationReplayOf, Scope: obs.Scope, FromObservation: obs.ID,
		ToNativeObservation: &model.NativeObservationRef{Scope: obs.Scope, Namespace: "native.eventId", Value: mustJSON(update.EventID)},
		ObservationRefs:     []string{obs.ID}}
	batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
}
