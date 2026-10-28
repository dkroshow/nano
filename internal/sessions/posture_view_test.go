package sessions

import (
	"testing"
)

// A resumed codex session re-seeds its per-turn overrides from the fold, so a
// click-resume and a boot reattach converge. A pre-log thread (nil fold)
// falls back to the attachment record's stored tier — the boot special case
// retired into this one seam — while any recorded history makes the log the
// only truth.
func TestSeedCodexTurnOverridesFromFold(t *testing.T) {
	folded := map[string]postureEntry{
		postureFieldModel:       {Field: postureFieldModel, Value: "gpt-5.3-codex", Source: postureSourceOperator},
		postureFieldEffort:      {Field: postureFieldEffort, Value: "high", Source: postureSourceCreation},
		postureFieldServiceTier: {Field: postureFieldServiceTier, Value: "priority", Source: postureSourceOperator},
	}

	codex := &Session{provider: "codex",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	codex.seedCodexTurnOverrides(folded, "ignored-legacy")
	if codex.turnModel != "gpt-5.3-codex" || codex.turnEffort != "high" || codex.turnServiceTier != "priority" {
		t.Fatalf("codex overrides = %q/%q/%q, want the folded posture seeded",
			codex.turnModel, codex.turnEffort, codex.turnServiceTier)
	}

	legacy := &Session{provider: "codex",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	legacy.seedCodexTurnOverrides(nil, "priority")
	if legacy.turnServiceTier != "priority" {
		t.Fatalf("legacy tier = %q, want the pre-log record honored on a nil fold", legacy.turnServiceTier)
	}

	cleared := &Session{provider: "codex",
		turnServiceTier: "stale",
		lifecycleState:  testLifecycleSnapshot("ready", false)}
	cleared.seedCodexTurnOverrides(map[string]postureEntry{}, "stale-record")
	if cleared.turnServiceTier != "stale" {
		t.Fatalf("cleared-history tier = %q, want the record ignored once any history exists", cleared.turnServiceTier)
	}
}

// The application sibling supplies real canonical folds. This assertion keeps
// the private stale-cache replacement check with the session that owns it.
func TestCanonicalResumeProjectionPreservesDesiredIntent(t *testing.T) {
	sess := &Session{turnModel: "stale", turnServiceTier: "stale"}
	sess.seedCodexCanonicalOverrides(hostedMetadataView{})
	if sess.turnModel != "" || sess.turnServiceTier != "" {
		t.Fatalf("Codex resume = %q/%q, want cleared canonical values", sess.turnModel, sess.turnServiceTier)
	}
}
