package codex

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The latest turn can still receive usage or snapshots. Earlier settled turns
// belong to the consumer, except while a pending request needs their assembly.
// The wire keeps a settled turn whole: a pane shows its live rows until the
// saved file catches up. Only a file checkpoint applies the retention rule.
func (state *State) evictSettled(wire bool) {
	keep := map[string]bool{}
	for _, key := range state.ActiveTurns {
		keep[key] = true
	}
	for _, request := range state.Requests {
		if request.ReplyRef == "" && request.ResolutionRef == "" {
			keep[request.Scope.TurnKey] = true
			keep[state.Items[request.ItemID].TurnKey] = true
			if request.Scope.NativeTurnID != nil {
				keep[state.NativeTurns[scopedKey(request.Scope, "turn", *request.Scope.NativeTurnID)]] = true
			}
		}
	}
	for key, turn := range state.Turns {
		if !keep[key] && settledOutcome(turn.Outcome) {
			state.dropTurn(key)
		}
	}
	for id, item := range state.Items {
		if item.TurnKey != "" {
			if _, exists := state.Turns[item.TurnKey]; !exists {
				state.dropItem(id)
			}
		}
	}
	if !wire {
		state.retainReemittable(func(reemit map[string]bool) {
			for _, request := range state.Requests {
				reemit[request.ItemID] = true
			}
			// A serialization twin still to come merges into its item; evicting
			// the item would make the twin a second row.
			for _, ids := range state.Twins {
				for _, id := range ids {
					reemit[id] = true
				}
			}
			// Without native turn ids, the next prompt starts a turn only if
			// the active turn already holds one (file.go); keep that prompt.
			for id, item := range state.Items {
				if item.Type == model.ItemUserMessage && state.Turns[item.TurnKey].Scope.NativeTurnID == nil {
					reemit[id] = true
				}
			}
		})
		state.retainRevisable()
	}
	for key, id := range state.NativeTurns {
		if _, exists := state.Turns[id]; !exists {
			delete(state.NativeTurns, key)
		}
	}
	for turnKey, count := range state.UsageCounts {
		if _, exists := state.Turns[turnKey]; !exists {
			state.fillUsage(turnKey)
			for i := 0; i < count; i++ {
				delete(state.Usage, usageKey(turnKey, i))
				state.markChanged("usage", usageKey(turnKey, i))
			}
			delete(state.UsageCounts, turnKey)
		}
	}
	for _, index := range []map[string]string{state.NativeItems, state.UpdateItems, state.Calls} {
		for key, id := range index {
			_, exists := state.Items[id]
			if id == "" {
				// Empty targets preserve ambiguity while their turn is retained.
				exists = state.keyHasTurn(key)
			}
			if !exists {
				delete(index, key)
			}
		}
	}
	for key := range state.Twins {
		if !state.keyHasTurn(key) {
			delete(state.Twins, key)
		}
	}
	for key := range state.NewestItems {
		if !state.keyHasTurn(key) {
			delete(state.NewestItems, key)
		}
	}
	for key, request := range state.Requests {
		if request.ItemID != "" {
			if _, exists := state.Items[request.ItemID]; !exists {
				delete(state.Requests, key)
			}
		}
	}
}

// A settled turn kept for trailing records retains only the items the reader
// can still re-emit: its final answers, the calls a late result links to, and
// whatever a pending request still names (see the Claude reader).
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
			state.dropItem(id)
		}
	}
}

// An open turn keeps only the items a later record can still revise: the
// ones a settled turn keeps (retainReemittable), the newest item of each type
// (twin matching and the turn's final answer read it), every item a native
// item_completed record made or can join (user and agent messages and
// reasoning, whichever copy came first), and whatever an update, a process
// stream or a request still names. Codex completes a native item again under
// its id (a growing reasoning record up to about a thousand records later);
// response rows never repeat an id, and a call's output follows the call, so
// the rest belongs to the consumer. Without this an open turn kept every
// item it had produced, and each append re-saved all of them.
func (state *State) retainRevisable() {
	keep := map[string]bool{}
	for _, turn := range state.Turns {
		for _, id := range turn.FinalItemRefs {
			keep[id] = true
		}
	}
	for _, index := range []map[string]string{state.Calls, state.NewestItems, state.UpdateItems} {
		for _, id := range index {
			keep[id] = true
		}
	}
	for _, ids := range state.Twins {
		for _, id := range ids {
			keep[id] = true
		}
	}
	for _, request := range state.Requests {
		keep[request.ItemID], keep[request.ResultItemID] = true, true
	}
	for _, output := range state.Processes {
		keep[output.ItemID] = true
	}
	for id, item := range state.Items {
		turn, exists := state.Turns[item.TurnKey]
		if !exists || settledOutcome(turn.Outcome) || keep[id] {
			continue
		}
		if item.SourcePayloadType == "item_completed" || completedTwinType(item.Type) {
			continue
		}
		state.dropItem(id)
	}
}

func (state State) keyHasTurn(key string) bool {
	var parts []string
	if json.Unmarshal([]byte(key), &parts) != nil || len(parts) != 8 {
		return false
	}
	turn, _, _ := strings.Cut(parts[7], "/")
	_, exists := state.Turns[turn]
	return exists
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
	}
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
		d.Message = "Open Codex continuation exceeds 4 MiB; unfinished evidence remains intact"
		batch.Diagnostics = append(batch.Diagnostics, d)
	}
}

// A saver that keeps the state between calls writes only the large map
// entries a call set or deleted; every other field is small and is saved
// whole. Every change to Items, Turns and Usage goes through these helpers.
func (state *State) markChanged(field, key string) {
	if state.changed == nil {
		return
	}
	if state.changed[field] == nil {
		state.changed[field] = map[string]bool{}
	}
	state.changed[field][key] = true
}

func (state *State) setItem(item model.Item) {
	state.Items[item.ID] = item
	state.markChanged("items", item.ID)
}

func (state *State) dropItem(id string) {
	delete(state.Items, id)
	state.markChanged("items", id)
}

func (state *State) setTurn(turn model.Turn) {
	state.Turns[turn.Key] = turn
	state.markChanged("turns", turn.Key)
}

func (state *State) dropTurn(key string) {
	delete(state.Turns, key)
	state.markChanged("turns", key)
}

// Changed names, by checkpoint field ("items", "turns", "usage"), the entries
// set or deleted since ForgetChanged. It reports false when the state cannot
// say: one restored from JSON or closed, which a saver writes whole.
func (state State) Changed() (map[string][]string, bool) {
	if state.changed == nil {
		return nil, false
	}
	out := make(map[string][]string, len(state.changed))
	for field, keys := range state.changed {
		for key := range keys {
			out[field] = append(out[field], key)
		}
	}
	return out, true
}

// ForgetChanged starts tracking changes afresh, after a saver has written
// the whole state or its changes.
func (state State) ForgetChanged() State {
	state.changed = map[string]map[string]bool{}
	return state
}

// HasOpenTurn reports a turn that can still change: one with no settled outcome.
func (state State) HasOpenTurn() bool {
	for _, turn := range state.Turns {
		if !settledOutcome(turn.Outcome) {
			return true
		}
	}
	return false
}
