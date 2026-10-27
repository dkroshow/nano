package claude

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A new prompt retires settled assembly for the preceding turn. The most recent
// turn stays available for trailing usage, duration, and native result records;
// a stop_reason alone does not prevent a subsequent tool from reopening it.
// Closed native identities belong to the consumer's index. A later unmarked
// UUID produces fresh evidence, which the consumer resolves to its stored row.
// The wire keeps a settled turn whole: a pane shows its live rows until the
// saved file catches up. Only a file checkpoint applies the retention rule.
func (state *State) evictSettled(wire bool) {
	keep := map[string]bool{}
	for _, key := range state.ActiveTurns {
		keep[key] = true
	}
	for _, request := range state.Controls {
		if request.Response == nil {
			keep[state.Items[request.ItemID].TurnKey] = true
		}
	}
	for key, turn := range state.Turns {
		if !keep[key] && settledOutcome(turn.Outcome) {
			delete(state.Turns, key)
		}
	}
	for key := range state.TurnPrompts {
		if _, exists := state.Turns[key]; !exists {
			delete(state.TurnPrompts, key)
		}
	}
	for id, item := range state.Items {
		if item.TurnKey != "" {
			if _, exists := state.Turns[item.TurnKey]; !exists {
				delete(state.Items, id)
			}
		}
	}
	if !wire {
		state.retainReemittable(func(reemit map[string]bool) {
			for _, request := range state.Controls {
				reemit[request.ItemID] = true
			}
			for _, receipt := range state.Captures {
				for _, facet := range receipt.Facets {
					reemit[facet.ItemID] = true
				}
			}
		})
	}
	for key, request := range state.Controls {
		if request.ItemID != "" {
			if _, exists := state.Items[request.ItemID]; !exists {
				delete(state.Controls, key)
			}
		}
	}
	for key, message := range state.Messages {
		if message.TurnKey != "" {
			if _, exists := state.Turns[message.TurnKey]; !exists {
				delete(state.Messages, key)
			}
		}
	}
	for key, message := range state.ActiveMessages {
		if _, exists := state.Messages[message]; !exists {
			delete(state.ActiveMessages, key)
		}
	}
	for key, ids := range state.Rows {
		retained := false
		for _, id := range ids {
			if _, exists := state.Items[id]; exists {
				retained = true
				break
			}
		}
		if !retained {
			delete(state.Rows, key)
		}
	}
	for key, receipt := range state.RowEvents {
		_, hasTurn := state.Turns[receipt.TurnKey]
		_, hasItem := state.Items[receipt.ItemID]
		if !hasTurn && !hasItem {
			delete(state.RowEvents, key)
		}
	}
	for key, receipt := range state.Captures {
		retained := false
		for _, facet := range receipt.Facets {
			_, hasItem := state.Items[facet.ItemID]
			_, hasTurn := state.Turns[facet.TurnKey]
			retained = retained || hasItem || hasTurn
		}
		if !retained {
			delete(state.Captures, key)
		}
	}
	for key, id := range state.Calls {
		_, exists := state.Items[id]
		if id == "" {
			// An empty target records an ambiguous call in an open turn.
			var parts []string
			if json.Unmarshal([]byte(key), &parts) == nil && len(parts) == 9 {
				turn, _, _ := strings.Cut(parts[8], "/")
				_, exists = state.Turns[turn]
			}
		}
		if !exists {
			delete(state.Calls, key)
		}
	}
}

// A settled turn kept for trailing records retains only the items the reader
// can still re-emit: its final answers, which a reopening tool retracts; the
// calls a late result links to; and whatever a pending control or capture
// still names. Its other output belongs to the consumer's rows. Before this,
// every file kept its whole latest turn: 7 GB of 9.3 GB of parser state.
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
	for id, item := range state.Items {
		if turn, exists := state.Turns[item.TurnKey]; exists && settledOutcome(turn.Outcome) && !reemit[id] {
			delete(state.Items, id)
		}
	}
}

func settledOutcome(outcome model.Outcome) bool {
	return outcome == model.OutcomeCompleted || outcome == model.OutcomeFailed || outcome == model.OutcomeInterrupted
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
		d.Message = "Open Claude continuation exceeds 4 MiB; unfinished evidence remains intact"
		batch.Diagnostics = append(batch.Diagnostics, d)
	}
}

// A closed source cannot revise any assembly in this incarnation. Its emitted
// rows remain in the consumer; only the close marker and source position remain.
// This does not turn an incomplete transcript into a completed provider turn.
func (state State) closedCheckpoint() State {
	return State{Version: state.Version, Source: state.Source, Framing: state.Framing,
		Frame: state.Frame, Limit: state.Limit, NextItem: state.NextItem, NextTurn: state.NextTurn,
	}
}
