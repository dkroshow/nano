package grok

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Keep each session's latest turn for trailing terminal/usage evidence, plus
// unfinished turns and turns needed by pending requests. Earlier settled output
// and its event/segment indexes belong to the consumer's persisted rows.
// The wire keeps a settled turn whole: a pane shows its live rows until the
// saved file catches up. Only a file checkpoint applies the retention rule.
func (state *State) evictSettled(wire bool) {
	latest := map[string]model.Turn{}
	keep := map[string]bool{}
	for _, turn := range state.Turns {
		key := sessionKey(turn.Scope)
		if turn.Ordinal > latest[key].Ordinal {
			latest[key] = turn
		}
	}
	for _, turn := range latest {
		keep[turn.Key] = true
	}
	for _, request := range state.Requests {
		if request.ReplyRef == "" {
			keep[request.Scope.TurnKey] = true
			keep[state.Items[request.Scope.ItemID].TurnKey] = true
			keep[state.Turns[scopedKey(request.Scope, "turn", "")].Key] = true
		}
	}
	retained := map[string]bool{}
	for group, turn := range state.Turns {
		if !keep[turn.Key] && settledOutcome(turn.Outcome) {
			delete(state.Turns, group)
			delete(state.UnboundPromptIndices, group)
		} else {
			retained[turn.Key] = true
		}
	}
	for id, item := range state.Items {
		if item.TurnKey != "" && !retained[item.TurnKey] {
			delete(state.Items, id)
		}
	}
	if !wire {
		state.retainReemittable(func(reemit map[string]bool) {
			for _, request := range state.Requests {
				reemit[request.Scope.ItemID] = true
			}
			// A replayed or late-bound event finds its item through these; an
			// evicted item would come back as a second row.
			for _, index := range []map[string]string{state.Events, state.Segments, state.Tasks} {
				for _, id := range index {
					reemit[id] = true
				}
			}
		})
	}
	for _, index := range []map[string]string{state.Segments, state.Events, state.Calls, state.LastAssistant, state.Tasks} {
		for key, id := range index {
			if _, exists := state.Items[id]; !exists {
				delete(index, key)
			}
		}
	}
	for key, request := range state.Requests {
		if request.Scope.ItemID != "" {
			if _, exists := state.Items[request.Scope.ItemID]; !exists {
				delete(state.Requests, key)
			}
		}
	}
}

// A settled turn kept for trailing records retains only the items the reader
// can still re-emit: its final answers, the calls a late result links to, and
// whatever a pending request or task still names (see the Claude reader).
func (state *State) retainReemittable(name func(reemit map[string]bool)) {
	reemit := map[string]bool{}
	for _, turn := range state.Turns {
		for _, id := range turn.FinalItemRefs {
			reemit[id] = true
		}
	}
	for _, id := range state.Calls {
		reemit[id] = true
	}
	name(reemit)
	settled := map[string]bool{}
	for _, turn := range state.Turns { // keyed by group, not by turn key
		settled[turn.Key] = settledOutcome(turn.Outcome)
	}
	for id, item := range state.Items {
		if settled[item.TurnKey] && !reemit[id] {
			delete(state.Items, id)
		}
	}
}

func settledOutcome(outcome model.Outcome) bool {
	switch outcome {
	case model.OutcomeCompleted, model.OutcomeFailed, model.OutcomeInterrupted, model.OutcomeCancelled, model.OutcomeDeclined:
		return true
	}
	return false
}

// A closed source cannot revise any assembly in this incarnation. Its emitted
// rows remain in the consumer; only the close marker and source position remain.
// This does not turn an incomplete transcript into a completed provider turn.
func (state State) closedCheckpoint() State {
	return State{Version: state.Version, Source: state.Source, Framing: state.Framing,
		Frame: state.Frame, Limit: state.Limit, NextItem: state.NextItem, NextTurn: state.NextTurn,
		Format: state.Format, DocumentBytes: state.DocumentBytes, DocumentClosed: state.DocumentClosed}
}

const (
	checkpointWarningBytes = 4 << 20
	checkpointMeasureBytes = 64 << 10
)

// Measuring the open continuation encodes the whole state, so measuring on every
// frame costs more as an unsettled turn grows: one 2.5-hour open turn left the
// session's event loop doing nothing else. State cannot grow faster than the
// bytes fed into it, so measure only once the source has added an eighth of the
// last measured size, and never more often than every 64 KiB. A continuation
// that keeps growing is still reported, within an eighth of its size of
// crossing the ceiling.
func (state *State) checkpointDiagnostic(batch *model.Batch) {
	due := state.measured / 8
	if due < checkpointMeasureBytes {
		due = checkpointMeasureBytes
	}
	if state.pending < due {
		return
	}
	state.pending, state.measurements = 0, state.measurements+1
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	state.measured = len(raw)
	if len(raw) > checkpointWarningBytes {
		d := diagnostic("continuation_limit_exceeded", state.Source)
		d.Message = "Open Grok continuation exceeds 4 MiB; unfinished evidence remains intact"
		batch.Diagnostics = append(batch.Diagnostics, d)
	}
}
