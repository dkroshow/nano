package threadindex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

// A lost terminal event settles only from an exact saved turn id. Claude saved
// turns share no id with the live turn, and a conflicted catalog identity proves
// nothing. Boundary: the saved-turn query over the committed catalog rows.
func TestSavedTurnEvidenceReadsOnlyExactCodexAndGrokTurns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	completed := time.Date(2026, 10, 1, 1, 0, 1, 0, time.UTC)
	rows := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "native"}},
		{"timestamp": "2026-10-01T01:00:00Z", "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "turn-1"}},
		{"type": "response_item", "payload": map[string]any{"id": "final-1", "type": "message", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "saved final"}}}},
		{"timestamp": completed.Format(time.RFC3339Nano), "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-1"}},
	}
	var data []byte
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "rollout-native.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := newTestCatalogService(ctx, store, Options{Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: root}}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.RequestRefresh()
	var id string
	waitForCatalog(t, func() bool {
		id, err = service.ResolveThreadReference(ctx, "native")
		if err != nil {
			return false
		}
		_, ok, err := service.ReadSavedTurnStatus(ctx, id, "turn-1")
		return err == nil && ok
	})
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, tc := range []struct {
		provider       string
		conflict, want bool
	}{{"codex", false, true}, {"grok", false, true}, {"claude", false, false}, {"codex", true, false}} {
		if _, err := db.Exec(`UPDATE threads SET conflict=? WHERE id=?`, tc.conflict, id); err != nil {
			t.Fatal(err)
		}
		got, ok := service.SavedTurnEvidence(ctx, tc.provider, "native", "turn-1")
		if ok != tc.want || (ok && (got.Turn.ID != "turn-1" || got.Turn.Status != "completed" || got.Turn.CompletedAtMs != completed.UnixMilli())) {
			t.Fatalf("%s conflict=%v: evidence=%+v, %v", tc.provider, tc.conflict, got, ok)
		}
	}
}
