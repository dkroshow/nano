package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	state "github.com/dkroshow/nano/internal/state/store"
)

func TestQueuedTurnStoreBindProviderTurnPersistsAcceptedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queued-turns.json")
	store, err := newTestQueuedTurnStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := QueuePutSubmission(store, prompts.QueuedTurn{
		ClientActionID: "action-1", Provider: "codex", NativeThreadID: "native-thread",
		Text: "hello", TextHash: sendTextHash("hello"), State: submissionDelivering,
	}); err != nil {
		t.Fatal(err)
	}
	if err := QueueBindProviderTurn(store, "action-1", "turn-1"); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get("action-1")
	if !ok || got.State != submissionAccepted || got.ProviderTurnID != "turn-1" {
		t.Fatalf("bound record = %#v found=%v", got, ok)
	}
	reloaded, err := newTestQueuedTurnStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok = reloaded.Get("action-1")
	if !ok || got.State != submissionAccepted || got.ProviderTurnID != "turn-1" {
		t.Fatalf("reloaded bound record = %#v found=%v", got, ok)
	}
}

func TestQueuedTurnStoreOlderJSONWithoutBindingStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queued-turns.json")
	legacy := `[
  {
    "clientActionId": "legacy-action",
    "provider": "codex",
    "nativeThreadId": "native-thread",
    "text": "old prompt",
    "textHash": "hash",
    "state": "accepted",
    "createdAt": "2026-08-26T05:20:55Z"
  }
]
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(filepath.Dir(path), "nano_threads.sqlite")
	if _, err := state.PrepareStore(context.Background(), state.StorePaths{Destination: dbPath, BackupRoot: filepath.Join(filepath.Dir(path), "backups"), Turns: prompts.LegacyTurnPaths{Queue: path}}, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	db, _, err := testTurnDatabase(t, path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := prompts.LoadQueuedTurns(context.Background(), db)
	if err != nil || len(rows) != 1 {
		t.Fatalf("legacy rows = %+v, %v", rows, err)
	}
	if got := rows[0]; got.ClientActionID != "legacy-action" || got.ProviderTurnID != "" || got.State != submissionAccepted {
		t.Fatalf("legacy record = %#v", got)
	}
}

func TestQueuedTurnByProviderTurnRequiresExactThreadIdentity(t *testing.T) {
	store, err := newTestQueuedTurnStore(t, filepath.Join(t.TempDir(), "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := QueuePutSubmission(store, prompts.QueuedTurn{
		ClientActionID: "bound", Provider: "codex", NativeThreadID: "native-thread",
		Text: "hello", TextHash: sendTextHash("hello"), State: submissionDelivering,
		ProviderTurnID: "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := QueueBindProviderTurn(store, "bound", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ByProviderTurn("codex", "native-thread", "", "turn-1"); !ok {
		t.Fatal("exact identity should match")
	}
	if _, ok := store.ByProviderTurn("codex", "other-thread", "", "turn-1"); ok {
		t.Fatal("specified other thread must not match")
	}
	if _, ok := store.ByProviderTurn("claude", "native-thread", "", "turn-1"); ok {
		t.Fatal("specified other provider must not match")
	}

	if _, _, err := QueuePutSubmission(store, prompts.QueuedTurn{
		ClientActionID: "unthreaded", Provider: "codex", NativeThreadID: "keep-me",
		Text: "later", TextHash: sendTextHash("later"), State: submissionDelivering,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database().Exec(`UPDATE queued_turns SET native_thread_id='', provider_turn_id='turn-empty-native' WHERE client_action_id='unthreaded'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ByProviderTurn("codex", "native-thread", "", "turn-empty-native"); ok {
		t.Fatal("empty record nativeThreadID must not wildcard-match a specified thread")
	}
}

func TestQueuedTurnBindProviderTurnFailureLeavesPreviousState(t *testing.T) {
	store, err := newTestQueuedTurnStore(t, filepath.Join(t.TempDir(), "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := QueuePutSubmission(store, prompts.QueuedTurn{
		ClientActionID: "action-fail", Provider: "codex", NativeThreadID: "native-thread",
		Text: "hello", TextHash: sendTextHash("hello"), State: submissionDelivering,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Database().Exec(`CREATE TRIGGER fail_legacy_bind BEFORE DELETE ON queued_turns BEGIN SELECT RAISE(FAIL,'synthetic bind failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := QueueBindProviderTurn(store, "action-fail", "turn-1"); err == nil {
		t.Fatal("expected bind persist failure")
	}
	got, ok := store.Get("action-fail")
	if !ok || got.State != submissionDelivering || got.ProviderTurnID != "" {
		t.Fatalf("failed bind mutated record = %#v found=%v", got, ok)
	}
}
