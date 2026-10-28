package app

import (
	"encoding/json"

	"context"
	"fmt"
	"maps"

	"testing"
	"time"
)

// A saved resume includes turns older than the retained operations. Looking up
// each of them across the fleet delays readiness even when none needs a write.
// Boundary: the real resume observer over a persisted execution store.
func TestAgentExecutionCodexResumeScansTheStoreOnce(t *testing.T) {
	store := newTestAgentExecutionStore(t, "resume-run")
	t.Cleanup(func() { _ = store.Control().Close() })
	const threadCount = 200
	for thread := range threadCount {
		hostedID := fmt.Sprintf("hosted-%d", thread)
		if _, err := fixtureEnsureSession(store, hostedID, "codex", presentationShared); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := store.Control().CanonicalDatabase().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for thread := range threadCount {
		hostedID := fmt.Sprintf("hosted-%d", thread)
		for turn := range 100 {
			id := fmt.Sprintf("op-%d-%d", thread, turn)
			rec := agentTurnRecord{
				ID: id, HostedThreadID: hostedID, ClientActionID: id, TransportActionID: id, PromptHash: id,
				Provider: "codex", NativeThreadID: fmt.Sprintf("native-%d", thread), ProviderTurnID: fmt.Sprintf("turn-%d-%d", thread, turn),
				Status: agentTurnCompleted, FinalResponse: "already reported", FinalItemID: "saved-answer", RunID: "resume-run",
				CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z", Revision: 1,
			}
			if _, err := tx.Exec(`INSERT INTO hosted_turn_operations(operation_id,hosted_thread_id,client_action_id,transport_action_id,prompt_hash,provider_turn_id,status,final_response,final_item_id,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, rec.ID, rec.HostedThreadID, rec.ClientActionID, rec.TransportActionID, rec.PromptHash, rec.ProviderTurnID, rec.Status, rec.FinalResponse, rec.FinalItemID, rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store, err = newControlAgentExecutionStore(store.Control(), "resume-run")
	if err != nil {
		t.Fatal(err)
	}
	const resumedTurns = 2000
	turns := make([]map[string]any, resumedTurns)
	for i := range turns {
		turns[i] = map[string]any{"id": fmt.Sprintf("turn-0-%d", i), "status": agentTurnCompleted}
	}
	response, err := json.Marshal(turns)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]agentTurnRecord {
		result := map[string]agentTurnRecord{}
		for thread := range threadCount {
			for _, rec := range store.TurnsForHosted(fmt.Sprintf("hosted-%d", thread)) {
				result[rec.ID] = rec
			}
		}
		return result
	}
	before := snapshot()
	bookmark := fixtureWaitChanges(store, context.Background(), "", 0).Bookmark
	sess, fake := newCodexObserverFixture(t, store, "hosted-0", "native-0")
	fake.SetResumeTurns(response)
	started := time.Now()
	startCodexObserverFixture(t, sess, fake)
	took := time.Since(started)
	t.Logf("resume of %d terminal turns over %d retained operations took %s", resumedTurns, len(before), took)
	// Plain runs only: the per-turn path takes several seconds there, while
	// -race on a loaded machine can push the one-pass path toward the bound.
	if !raceEnabled && took > 2*time.Second {
		t.Fatalf("terminal resume took %s for %d saved turns over %d operations", took, resumedTurns, len(before))
	}
	if !maps.Equal(snapshot(), before) || fixtureWaitChanges(store, context.Background(), bookmark, 0).Bookmark != bookmark {
		t.Fatal("already terminal or unknown resume turns changed the execution store")
	}
}
