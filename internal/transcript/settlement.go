package transcript

import (
	"encoding/json"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// SavedRow is one user row read from the provider's saved transcript, with
// whether its saved turn has ended.
type SavedRow struct {
	Item     model.Item
	Terminal bool
}

// Settlement is the outcome of checking one stored prompt against saved rows.
// Only SettlementSettled permits settling; every other outcome keeps the
// prompt published.
type Settlement string

const (
	SettlementSettled         Settlement = "settled"
	SettlementMissingIdentity Settlement = "missing_identity"
	SettlementNotSaved        Settlement = "not_saved"
	SettlementNotTerminal     Settlement = "not_terminal"
	SettlementAmbiguous       Settlement = "ambiguous"
)

// SavedSettlement decides whether saved rows prove that the provider saved
// entry's user row. The proof is the entry's own exact binding, the same rule
// that joins a live prompt to its provider row: a native event id (Claude's
// stamped row UUID, Codex's client message id) or a native turn id (Codex
// turn, Grok prompt). Every matching row must lie in one saved turn, no other
// stored prompt may claim those rows by the same identity, and that turn must
// have ended. A row can carry several prompts' own events: Claude saves
// prompts it merged as one row. Text, clocks and positions play no part.
//
// Prompts steered into one Codex turn share it, so an exact event outranks a
// turn: a row another stored prompt matches by its own event is that prompt's,
// and a binding with only a turn claims the turn's one remaining row. Two
// remaining rows leave it ambiguous.
func SavedSettlement(entry turns.OutboxEntry, stored []turns.OutboxEntry, saved []SavedRow) Settlement {
	settlement, _ := SavedSettlementTurn(entry, stored, saved)
	return settlement
}

// SavedSettlementTurn is SavedSettlement with the saved turn whose rows
// decided it. Prompts settled into one saved turn were answered together.
func SavedSettlementTurn(entry turns.OutboxEntry, stored []turns.OutboxEntry, saved []SavedRow) (Settlement, string) {
	binding := entry.Binding
	if binding == nil || binding.Scope.NativeThreadID == nil || *binding.Scope.NativeThreadID == "" ||
		binding.EventID == "" && (binding.Scope.NativeTurnID == nil || *binding.Scope.NativeTurnID == "") {
		return SettlementMissingIdentity, ""
	}
	turnOnly := binding.EventID == ""
	claimedByEvent := func(row model.Item) bool {
		for _, other := range stored {
			if other.ActionID != entry.ActionID && other.Binding != nil && other.Binding.EventID != "" && matchesSubmission(row, other.Binding) {
				return true
			}
		}
		return false
	}
	turn, terminal := "", false
	rows := map[string]bool{}
	for _, row := range saved {
		if !matchesSubmission(row.Item, binding) || turnOnly && claimedByEvent(row.Item) {
			continue
		}
		for _, other := range stored {
			if other.ActionID == entry.ActionID || !matchesSubmission(row.Item, other.Binding) {
				continue
			}
			if !turnOnly && other.Binding.EventID != binding.EventID {
				// A turn-only claim never outranks this entry's own event, and
				// a row that carries another prompt's own event too saved both:
				// Claude merges prompts queued together into one row.
				continue
			}
			return SettlementAmbiguous, ""
		}
		if turn != "" && row.Item.TurnKey != turn {
			return SettlementAmbiguous, ""
		}
		turn, terminal = row.Item.TurnKey, row.Terminal
		rows[row.Item.ID] = true
	}
	if turnOnly && len(rows) > 1 {
		return SettlementAmbiguous, ""
	}
	switch {
	case turn == "":
		return SettlementNotSaved, ""
	case !terminal:
		return SettlementNotTerminal, turn
	default:
		return SettlementSettled, turn
	}
}

func matchesSubmission(item model.Item, binding *turns.Binding) bool {
	if binding == nil || item.Type != model.ItemUserMessage || item.Role != model.RoleUser || item.Synthetic {
		return false
	}
	if binding.EventID != "" {
		// Claude's stream-json user UUID survives into the saved JSONL, while
		// the shim execution turn does not. Exact event matching therefore uses
		// native message scope (thread + child/tool parent), not the transport
		// turn. Other providers retain their native turn in this scope.
		if !observesEvent(item, binding.EventID) {
			return false // cheap exit before encoding scope keys
		}
		itemScope, itemOK := nativeItemScopeKey(item.Scope)
		bindingScope, bindingOK := nativeItemScopeKey(binding.Scope)
		if !itemOK || !bindingOK || itemScope != bindingScope {
			return false
		}
		for _, observation := range item.Observations {
			observedScope, ok := nativeItemScopeKey(observation.Scope)
			if ok && observedScope == itemScope && observation.NativeEventID == binding.EventID {
				return true
			}
		}
		return false
	}
	if turn := binding.Scope.NativeTurnID; turn != nil && (item.Scope.NativeTurnID == nil || *item.Scope.NativeTurnID != *turn) {
		return false // the scope keys below differ; exit before encoding them
	}
	itemScope, itemOK := nativeScopeKey(item.Scope)
	bindingScope, bindingOK := nativeScopeKey(binding.Scope)
	if !itemOK || !bindingOK || itemScope != bindingScope {
		return false
	}
	if binding.NativeItemID != "" {
		return binding.NativeItemID == item.ExactNativeItemID()
	}
	if binding.Scope.NativeTurnID != nil {
		return true
	}
	turn := item.Scope.TurnKey
	if turn == "" {
		turn = item.TurnKey
	}
	return binding.Scope.TurnKey != "" && binding.Scope.TurnKey == turn &&
		binding.Scope.Source != "" && binding.Scope.Source == item.Scope.Source &&
		binding.Scope.Incarnation != "" && binding.Scope.Incarnation == item.Scope.Incarnation &&
		binding.Scope.ConnectionID == item.Scope.ConnectionID
}

func observesEvent(item model.Item, eventID string) bool {
	for _, observation := range item.Observations {
		if observation.NativeEventID == eventID {
			return true
		}
	}
	return false
}

// nativeScopeKey is a scope's native identity: provider, thread, turn and
// parent call. JSON tuple encoding keeps ids containing separators from
// aliasing another scope; no provider content enters it.
func nativeScopeKey(scope model.Scope) (string, bool) {
	if scope.Provider == "" || scope.NativeThreadID == nil || *scope.NativeThreadID == "" {
		return "", false
	}
	if scope.NativeTurnID != nil && *scope.NativeTurnID == "" || scope.NativeParentCallID != nil && *scope.NativeParentCallID == "" {
		return "", false
	}
	encoded, _ := json.Marshal([]any{scope.Provider, *scope.NativeThreadID, scope.NativeTurnID, scope.NativeParentCallID})
	return string(encoded), true
}

// Claude's shim turn identifies one Nano execution, but Claude's saved JSONL
// does not retain that value. Exact message-block and row-event identities are
// provider identities without the transport turn.
func nativeItemScopeKey(scope model.Scope) (string, bool) {
	if scope.Provider == model.ProviderClaude {
		scope.NativeTurnID = nil
	}
	return nativeScopeKey(scope)
}
