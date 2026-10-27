package history

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func identityStoreProjection(t *testing.T, store *Store, key string) int64 {
	t.Helper()
	ctx := context.Background()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	metadata := testSource(root, "thread.jsonl", "native-thread-"+key, ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: ProviderClaude, ParserVersion: "identity-test", CompletionRuleVersion: "identity-test", Result: TranscriptProjectionResult{ProjectionKey: key, ParserState: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := store.db.QueryRow(`SELECT id FROM conversation_projections WHERE projection_key=?`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTranscriptIdentityReceiptsSurviveReopenAndStayInProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	projection := identityStoreProjection(t, store, "first")
	other := identityStoreProjection(t, store, "other")
	item := identityTestItem()
	item.NativeItemID = ""
	item.CallID = ""
	item.SourceLine = 1
	first := identityStoreApply(t, store, projection, item)
	if first.ItemID == "" {
		t.Fatal("new native evidence did not get a canonical identity")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	late := item
	late.ID = "second-local"
	late.SourceLine = 20
	late.Scope.ItemID = late.ID
	late.Observations = append([]model.Observation(nil), item.Observations...)
	late.Observations[0].ID = "second-observation"
	got := identityStoreApply(t, store, projection, late)
	if got.ItemID != first.ItemID || len(got.Replay) != 1 || got.Replay[0].ToObservation != item.Observations[0].ID {
		t.Fatalf("reopen lost replay identity: %+v / %+v", first, got)
	}
	var id string
	if err := store.db.QueryRow(`SELECT a.item_id FROM conversation_parse_identities a JOIN conversation_parse_values v ON v.id=a.identity_value_id WHERE a.projection_id=? AND a.identity_kind='local' AND v.value_key=?`, projection, testTranscriptValueKey(t, late.ID)).Scan(&id); err != nil || id != first.ItemID {
		t.Fatalf("second local key missing: %q %v", id, err)
	}
	isolated := identityStoreApply(t, store, other, late)
	if len(isolated.Replay) != 0 {
		t.Fatal("another projection borrowed old native receipts")
	}
	conflict := late
	conflict.ID = "conflict-local"
	conflict.SourceLine = 21
	conflict.Observations = append([]model.Observation(nil), late.Observations...)
	conflict.Observations[0].NativeEventDigest = "different"
	if got := identityStoreApply(t, store, projection, conflict); got.ItemID != "" || got.Diagnostic != "conflicting_native_event" {
		t.Fatalf("conflicting UUID reuse joined: %+v", got)
	}
}

func identityStoreApply(t *testing.T, store *Store, projection int64, item model.Item) transcriptIdentityResolution {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	newID := canonicalTranscriptItemID(Provider(item.Scope.Provider), "native-thread", item.ID)
	got, err := resolveTranscriptItemIdentityTx(ctx, tx, projection, newID, "", item)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestTranscriptIdentityReceiptWritesRollback(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "rollback")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	item := identityTestItem()
	if _, err := resolveTranscriptItemIdentityTx(ctx, tx, projection, "native-thread", "", item); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_parse_identities WHERE projection_id=?`, projection).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back identity survived: %d %v", count, err)
	}
	// Removing only this synthetic projection must cascade its receipts.
	identityStoreApply(t, store, projection, item)
	if _, err := store.db.Exec(`DELETE FROM conversation_projections WHERE id=?`, projection); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT item_id FROM conversation_parse_identities WHERE projection_id=?`, projection).Scan(new(string)); err != sql.ErrNoRows {
		t.Fatalf("projection collection left receipts: %v", err)
	}
}

func TestTranscriptReplayAliasDoesNotClaimLaterTurnCall(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "replay-call")
	original := identityTestItem()
	original.NativeItemID = ""
	original.Scope.TurnKey = "first-turn"
	original.Observations[0].Scope = original.Scope
	first := identityStoreApply(t, store, projection, original)
	late := original
	late.ID = "replay-local"
	late.Scope.ItemID, late.Scope.TurnKey = late.ID, "later-turn"
	late.Observations = append([]model.Observation(nil), original.Observations...)
	late.Observations[0].ID, late.Observations[0].Scope = "replay-observation", late.Scope
	for i := 0; i < 2; i++ {
		got := identityStoreApply(t, store, projection, late)
		if got.ItemID != first.ItemID || len(got.Replay) != 1 {
			t.Fatalf("replay revision %d lost its original item", i)
		}
	}
	next := late
	next.ID, next.Scope.ItemID = "new-call-local", "new-call-local"
	next.Observations = nil
	got := identityStoreApply(t, store, projection, next)
	if got.ItemID == first.ItemID || got.Diagnostic != "" {
		t.Fatal("replay alias made a new call in the later turn claim an old row")
	}
}
