package history

import (
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestLiveCallIdentityKeepsNamedAndUnnamedFacetsSeparate(t *testing.T) {
	var index ParsedItemIdentities
	first := identityTestItem()
	first.NativeItemID, first.Observations = "", nil
	nativeTurn := "native-turn"
	first.Scope.Provider, first.Scope.NativeTurnID, first.Scope.TurnKey = model.ProviderCodex, &nativeTurn, "before-eviction"
	first.Type, first.Role, first.ToolName = model.ItemCommand, model.RoleAssistant, "exec_command"
	index.Remember(first, "invocation", false)
	late := first
	late.ID, late.Scope.ItemID, late.Scope.TurnKey = "late", "late", "after-eviction"
	late.ToolName = ""
	if id, _, conflict := index.Resolve(late, ""); id != "" || conflict != "" {
		t.Fatal("unnamed execution matched a named invocation through its call ID")
	}
	index.Remember(late, "execution", false)
	later := late
	later.ID, later.Scope.ItemID, later.Scope.TurnKey = "later", "later", "after-another-eviction"
	if id, _, conflict := index.Resolve(later, ""); id != "execution" || conflict != "" {
		t.Fatal("compatible late execution did not retain its own facet")
	}
	later.ToolName = "exec_command"
	if id, _, conflict := index.Resolve(later, ""); id != "invocation" || conflict != "" {
		t.Fatal("compatible invocation did not retain its own facet")
	}
}

func TestLiveIdentityReplayDoesNotClaimLaterTurnCall(t *testing.T) {
	var index ParsedItemIdentities
	original := identityTestItem()
	original.NativeItemID = ""
	original.Scope.TurnKey = "first"
	original.Observations[0].Scope = original.Scope
	index.Remember(original, "view", false)
	late := original
	late.ID, late.Scope.ItemID, late.Scope.TurnKey = "later-local", "later-local", "later"
	late.Observations = append([]model.Observation(nil), original.Observations...)
	late.Observations[0].ID, late.Observations[0].Scope = "later-observation", late.Scope
	id, replay, conflict := index.Resolve(late, "")
	if id != "view" || len(replay) != 1 || conflict != "" {
		t.Fatal("live lookup did not resolve the earlier event")
	}
	index.Remember(late, id, len(replay) != 0)
	next := late
	next.ID, next.Scope.ItemID, next.Observations = "new-result", "new-result", nil
	if id, _, conflict := index.Resolve(next, ""); id != "" || conflict != "" {
		t.Fatal("a replay made the later turn claim an old call")
	}
	index.Retain(nil)
	if id, _, _ := index.Resolve(original, ""); id != "" {
		t.Fatal("evicted live view retained native identity receipts")
	}
}

func TestLiveIdentityRetainedReplayDoesNotFreezeLaterRevision(t *testing.T) {
	var index ParsedItemIdentities
	first := identityTestItem()
	first.Observations[0].ID = "first"
	index.Remember(first, "view", false)
	replay := first
	replay.Observations = append([]model.Observation(nil), first.Observations...)
	late := first.Observations[0]
	late.ID = "late"
	replay.Observations = append(replay.Observations, late)
	id, relations, conflict := index.Resolve(replay, "")
	if id != "view" || conflict != "" || !index.ReplayOnly(replay, id, relations) {
		t.Fatal("retained original observation hid an exact replay")
	}
	revision := late
	revision.ID = "revision"
	revision.NativeEventID = "new-event"
	revision.NativeEventDigest = "new-digest"
	replay.Observations = append(replay.Observations, revision)
	if index.ReplayOnly(replay, id, relations) {
		t.Fatal("an older replay froze a new revision")
	}
}
