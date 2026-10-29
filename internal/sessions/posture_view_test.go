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

	codex := sessionWithOwners(&Session{
		state: State{
			lifecycleState: testLifecycleSnapshot("ready", false)},
		identity: attachmentIdentity{provider: "codex"},
	})
	codex.state.seedCodexTurnOverrides(folded, "ignored-legacy")
	if codex.state.turnModel != "gpt-5.3-codex" || codex.state.turnEffort != "high" || codex.state.turnServiceTier != "priority" {
		t.Fatalf("codex overrides = %q/%q/%q, want the folded posture seeded",
			codex.state.turnModel, codex.state.turnEffort, codex.state.turnServiceTier)
	}

	legacy := sessionWithOwners(&Session{
		state: State{
			lifecycleState: testLifecycleSnapshot("ready", false)},
		identity: attachmentIdentity{provider: "codex"},
	})
	legacy.state.seedCodexTurnOverrides(nil, "priority")
	if legacy.state.turnServiceTier != "priority" {
		t.Fatalf("legacy tier = %q, want the pre-log record honored on a nil fold", legacy.state.turnServiceTier)
	}

	cleared := sessionWithOwners(&Session{
		state: State{
			turnServiceTier: "stale",
			lifecycleState:  testLifecycleSnapshot("ready", false)},
		identity: attachmentIdentity{provider: "codex"},
	})
	cleared.state.seedCodexTurnOverrides(map[string]hostedstore.PostureEntry{}, "stale-record")
	if cleared.state.turnServiceTier != "stale" {
		t.Fatalf("cleared-history tier = %q, want the record ignored once any history exists", cleared.state.turnServiceTier)
	}
}

// The application sibling supplies real canonical folds. This assertion keeps
// the private stale-cache replacement check with the session that owns it.
func TestCanonicalResumeClearsStaleSessionOverrides(t *testing.T) {
	sess := sessionWithOwners(&Session{
		state: State{turnModel: "stale", turnServiceTier: "stale"}})
	sess.state.seedCodexCanonicalOverrides(hostedstore.MetadataView{})
	if sess.state.turnModel != "" || sess.state.turnServiceTier != "" {
		t.Fatalf("Codex resume = %q/%q, want cleared canonical values", sess.state.turnModel, sess.state.turnServiceTier)
	}
}
