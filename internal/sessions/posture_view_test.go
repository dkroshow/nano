package sessions

import (
	"testing"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// A resumed codex session re-seeds its per-turn overrides from the fold, so a
// click-resume and a boot reattach converge. A pre-log thread (nil fold)
// falls back to the attachment record's stored tier — the boot special case
// retired into this one seam — while any recorded history makes the log the
// only truth.
func TestSeedCodexTurnOverridesFromFold(t *testing.T) {
	folded := map[string]hostedstore.PostureEntry{
		postureFieldModel:       {Field: postureFieldModel, Value: "gpt-5.3-codex", Source: postureSourceOperator},
		postureFieldEffort:      {Field: postureFieldEffort, Value: "high", Source: postureSourceCreation},
		postureFieldServiceTier: {Field: postureFieldServiceTier, Value: "priority", Source: postureSourceOperator},
	}

	codex := sessionWithOwners(&Session{provider: "codex",
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	codex.stateOwner.seedCodexTurnOverrides(folded, "ignored-legacy")
	if codex.stateOwner.turnModel != "gpt-5.3-codex" || codex.stateOwner.turnEffort != "high" || codex.stateOwner.turnServiceTier != "priority" {
		t.Fatalf("codex overrides = %q/%q/%q, want the folded posture seeded",
			codex.stateOwner.turnModel, codex.stateOwner.turnEffort, codex.stateOwner.turnServiceTier)
	}

	legacy := sessionWithOwners(&Session{provider: "codex",
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	legacy.stateOwner.seedCodexTurnOverrides(nil, "priority")
	if legacy.stateOwner.turnServiceTier != "priority" {
		t.Fatalf("legacy tier = %q, want the pre-log record honored on a nil fold", legacy.stateOwner.turnServiceTier)
	}

	cleared := sessionWithOwners(&Session{provider: "codex",
		stateOwner: State{
			turnServiceTier: "stale",
			lifecycleState:  testLifecycleSnapshot("ready", false)}})
	cleared.stateOwner.seedCodexTurnOverrides(map[string]hostedstore.PostureEntry{}, "stale-record")
	if cleared.stateOwner.turnServiceTier != "stale" {
		t.Fatalf("cleared-history tier = %q, want the record ignored once any history exists", cleared.stateOwner.turnServiceTier)
	}
}

// The application sibling supplies real canonical folds. This assertion keeps
// the private stale-cache replacement check with the session that owns it.
func TestCanonicalResumeClearsStaleSessionOverrides(t *testing.T) {
	sess := sessionWithOwners(&Session{
		stateOwner: State{turnModel: "stale", turnServiceTier: "stale"}})
	sess.stateOwner.seedCodexCanonicalOverrides(hostedstore.MetadataView{})
	if sess.stateOwner.turnModel != "" || sess.stateOwner.turnServiceTier != "" {
		t.Fatalf("Codex resume = %q/%q, want cleared canonical values", sess.stateOwner.turnModel, sess.stateOwner.turnServiceTier)
	}
}
