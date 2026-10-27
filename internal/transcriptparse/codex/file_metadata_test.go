package codex

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexFileMetadataDoesNotInventTurns(t *testing.T) {
	for _, name := range []string{"file-world-state", "file-inter-agent-communication-metadata", "file-event-msg-thread-name-updated", "file-event-msg-thread-goal-updated", "file-event-msg-thread-settings-applied"} {
		state, batch := parseFileCase(t, name)
		if len(state.Items) != 0 || len(state.Turns) != 0 || len(envelopeObservations(batch)) != 1 || len(batch.Diagnostics) != 0 {
			t.Fatalf("%s invented content or turns", name)
		}
		var native, retained map[string]json.RawMessage
		if json.Unmarshal(fixture(t, name), &native) != nil || json.Unmarshal(envelopeObservations(batch)[0].Detail, &retained) != nil || rpcFingerprint(native) != rpcFingerprint(retained) {
			t.Fatal("metadata lost native fields")
		}
	}
}

func TestCodexFileGuardianKeepsReviewIdentity(t *testing.T) {
	state, batch := parseFileCase(t, "file-event-msg-guardian-assessment")
	items := orderedItems(state)
	if len(items) != 1 || len(batch.Diagnostics) != 0 || items[0].Type != model.ItemInteraction || items[0].Role != model.RoleSystem || items[0].Text != "[redacted]" || items[0].Outcome != model.OutcomeRunning || items[0].NativeItemID != "" {
		t.Fatal("review became a generic item or borrowed target identity")
	}
	found := false
	for _, alias := range items[0].Aliases {
		if alias.Namespace == "codex.reviewId" {
			found = true
		}
	}
	if !found {
		t.Fatal("native review alias missing")
	}
	state, _ = parseFileCase(t, "file-event-msg-collab-agent-interaction-end")
	if orderedItems(state)[0].Text != "Example text." {
		t.Fatal("native coordination text lost")
	}
}
