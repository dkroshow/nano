package transcript

import (
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A saved row settles a stored prompt only through the prompt's own exact
// provider identity, in exactly one saved turn that has ended. Every other
// case must leave the prompt unsettled so it stays visible, and a decided
// prompt names the saved turn that decided it. Boundary: the provider-neutral
// settlement decision over saved rows.
func TestSavedSettlementRequiresOneExactTerminalTurn(t *testing.T) {
	ptr := func(value string) *string { return &value }
	turnBinding := func(provider model.Provider, turn string) *turns.Binding {
		return &turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: provider, NativeThreadID: ptr("thread"), NativeTurnID: ptr(turn)}}
	}
	claudeBinding := &turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: ptr("thread"), NativeTurnID: ptr("claude:shim:7")}, EventID: "row-uuid"}
	userRow := func(provider model.Provider, nativeTurn, savedTurn string, terminal bool) SavedRow {
		scope := model.Scope{Kind: model.ScopeItem, Provider: provider, Source: "saved", Incarnation: "projection", NativeThreadID: ptr("thread"), TurnKey: savedTurn}
		if nativeTurn != "" {
			scope.NativeTurnID = ptr(nativeTurn)
		}
		return SavedRow{Terminal: terminal, Item: model.Item{ID: savedTurn + "-user", TurnKey: savedTurn, Type: model.ItemUserMessage, Role: model.RoleUser, Scope: scope}}
	}
	claudeRow := func(event, savedTurn string, terminal bool) SavedRow {
		row := userRow(model.ProviderClaude, "", savedTurn, terminal)
		row.Item.Observations = []model.Observation{{NativeEventID: event, Scope: row.Item.Scope, Source: model.SourceRef{BlockPath: "$"}}}
		return row
	}
	entry := func(action string, binding *turns.Binding) turns.OutboxEntry {
		return turns.OutboxEntry{HostedThreadID: "hosted", ActionID: action, Sequence: 1, State: turns.Accepted, Binding: binding}
	}
	// Several Codex prompts can share a turn when one is steered into it; each
	// row then carries its prompt's client id. A legacy prompt bound only by
	// the turn owns the turn's one row that no exact claim takes.
	codexRow := func(id, event string) SavedRow {
		row := userRow(model.ProviderCodex, "turn-a", "saved-a", true)
		row.Item.ID = id
		if event != "" {
			row.Item.Observations = []model.Observation{{NativeEventID: event, Scope: row.Item.Scope}}
		}
		return row
	}
	exact := func(turn, event string) *turns.Binding {
		binding := turnBinding(model.ProviderCodex, turn)
		binding.EventID = event
		return binding
	}
	shared := []SavedRow{codexRow("row-starter", ""), codexRow("row-correction", "client-correction")}
	child := userRow(model.ProviderCodex, "turn-a", "saved-a", true)
	child.Item.Scope.NativeParentCallID = ptr("spawn-call")
	local := &turns.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: ptr("thread"), TurnKey: "local", Source: "wire", Incarnation: "one"}}

	for _, test := range []struct {
		name   string
		prompt turns.OutboxEntry
		others []turns.OutboxEntry
		saved  []SavedRow
		want   Settlement
	}{
		{"codex_native_turn", entry("a", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", true)}, SettlementSettled},
		{"codex_two_user_facets_one_turn", entry("a", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", true), userRow(model.ProviderCodex, "turn-a", "saved-a", true)}, SettlementSettled},
		{"grok_prompt_id", entry("a", turnBinding(model.ProviderGrok, "prompt-a")), nil, []SavedRow{userRow(model.ProviderGrok, "prompt-a", "saved-a", true)}, SettlementSettled},
		{"claude_row_uuid", entry("a", claudeBinding), nil, []SavedRow{claudeRow("row-uuid", "saved-a", true)}, SettlementSettled},
		{"no_binding", entry("a", nil), nil, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", true)}, SettlementMissingIdentity},
		{"adapter_local_turn_only", entry("a", local), nil, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", true)}, SettlementMissingIdentity},
		{"not_saved_yet", entry("a", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{userRow(model.ProviderCodex, "turn-b", "saved-b", true)}, SettlementNotSaved},
		{"other_claude_row", entry("a", claudeBinding), nil, []SavedRow{claudeRow("other-uuid", "saved-a", true)}, SettlementNotSaved},
		{"child_scope_only", entry("a", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{child}, SettlementNotSaved},
		{"turn_in_progress", entry("a", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", false)}, SettlementNotTerminal},
		{"two_saved_turns", entry("a", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", true), userRow(model.ProviderCodex, "turn-a", "saved-b", true)}, SettlementAmbiguous},
		{"row_claimed_by_another_prompt", entry("a", turnBinding(model.ProviderCodex, "turn-a")), []turns.OutboxEntry{entry("b", turnBinding(model.ProviderCodex, "turn-a"))}, []SavedRow{userRow(model.ProviderCodex, "turn-a", "saved-a", true)}, SettlementAmbiguous},
		{"legacy_starter_beside_exact_correction", entry("starter", turnBinding(model.ProviderCodex, "turn-a")), []turns.OutboxEntry{entry("correction", exact("turn-a", "client-correction"))}, shared, SettlementSettled},
		{"exact_correction_beside_legacy_starter", entry("correction", exact("turn-a", "client-correction")), []turns.OutboxEntry{entry("starter", turnBinding(model.ProviderCodex, "turn-a"))}, shared, SettlementSettled},
		{"claim_from_another_turn_excludes_nothing", entry("starter", turnBinding(model.ProviderCodex, "turn-a")), []turns.OutboxEntry{entry("elsewhere", exact("turn-b", "client-correction"))}, shared, SettlementAmbiguous},
		{"two_exact_claims_conflict", entry("correction", exact("turn-a", "client-correction")), []turns.OutboxEntry{entry("again", exact("turn-a", "client-correction"))}, shared, SettlementAmbiguous},
		{"two_unclaimed_rows_one_turn", entry("starter", turnBinding(model.ProviderCodex, "turn-a")), nil, []SavedRow{codexRow("row-starter", ""), codexRow("row-other", "")}, SettlementAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := append([]turns.OutboxEntry{test.prompt}, test.others...)
			got, turn := SavedSettlementTurn(test.prompt, stored, test.saved)
			if got != test.want {
				t.Fatalf("settlement = %q, want %q", got, test.want)
			}
			// The saved turn that decided it is how prompts answered together
			// are found; an undecided prompt names none.
			if wantTurn := map[bool]string{true: "saved-a"}[got == SettlementSettled || got == SettlementNotTerminal]; turn != wantTurn {
				t.Fatalf("saved turn = %q, want %q", turn, wantTurn)
			}
		})
	}
}
