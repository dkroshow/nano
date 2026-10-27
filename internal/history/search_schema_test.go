package history

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConversationSearchSchemaMigrationIsEmptyAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "thread.jsonl", "search-schema-thread", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %t, %v", stored, ok, err)
	}
	projection := searchEligibilityProjection("search-schema-projection", []TranscriptItem{
		searchEligibilityItem("historical-message", 1, ItemUserMessage, RoleUser, "historical message"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: "search-schema-test", CompletionRuleVersion: "search-schema-rules",
		Result: projection, IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Recreate the pre-v10 state around an already-populated conversation item.
	// Opening it must add only empty derived search storage, never backfill it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE conversation_search_literal_fts`,
		`DROP TABLE conversation_search_terms_fts`,
		`DROP TABLE conversation_search_state`,
		`DROP TABLE conversation_search_documents`,
		`DELETE FROM schema_migrations WHERE version = 10`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			t.Fatalf("prepare pre-v10 database with %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_items`, 1)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_literal_fts`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_terms_fts`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM schema_migrations WHERE version = 10`, 1)
	assertSearchStates(t, mustConversationSearchStatuses(t, store), map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchNeedsBuild,
		ConversationSearchTerms:   ConversationSearchNeedsBuild,
	})

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, 0)
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_state`, 2)
	assertCount(t, store.db, `SELECT COUNT(*) FROM schema_migrations WHERE version = 10`, 1)
}

func TestConversationSearchDocumentsCascadeFromConversationItems(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projectionID, itemID := addSearchSchemaTranscriptItem(t, ctx, store)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO conversation_search_documents(document_id, projection_id, item_id)
VALUES (?, ?, ?)`, 71, projectionID, itemID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM conversation_projections WHERE id = ?`, projectionID); err != nil {
		t.Fatal(err)
	}
	assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, 0)
}

func TestContentlessConversationSearchFTSDeleteAndReinsert(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, table := range []string{"conversation_search_literal_fts", "conversation_search_terms_fts"} {
		t.Run(table, func(t *testing.T) {
			if _, err := store.db.ExecContext(ctx, `INSERT INTO `+table+`(rowid, text) VALUES (?, ?)`, 41, "obsolete needle"); err != nil {
				t.Fatal(err)
			}
			assertFTSMatchCount(t, ctx, store.db, table, "needle", 1)
			// contentless_delete=1 deliberately supports ordinary DELETE; the
			// legacy special delete command is neither required nor used.
			if _, err := store.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE rowid = ?`, 41); err != nil {
				t.Fatalf("contentless delete: %v", err)
			}
			assertFTSMatchCount(t, ctx, store.db, table, "needle", 0)
			if _, err := store.db.ExecContext(ctx, `INSERT INTO `+table+`(rowid, text) VALUES (?, ?)`, 41, "replacement needle"); err != nil {
				t.Fatalf("reinsert after contentless delete: %v", err)
			}
			assertFTSMatchCount(t, ctx, store.db, table, "replacement", 1)
		})
	}
}

func TestConversationSearchStateVersionInvalidationAndLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	initial := mustConversationSearchStatuses(t, store)
	assertSearchStates(t, initial, map[ConversationSearchMode]ConversationSearchBuildState{
		ConversationSearchLiteral: ConversationSearchNeedsBuild,
		ConversationSearchTerms:   ConversationSearchNeedsBuild,
	})

	checkpointProjection, checkpointDocument := int64(8), int64(19)
	checkpointItem := "source-key"
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		if _, err := store.SetConversationSearchState(ctx, mode, ConversationSearchStateUpdate{
			State: ConversationSearchBuilding, CheckpointProjectionID: &checkpointProjection,
			CheckpointItemID: &checkpointItem, CheckpointDocumentID: &checkpointDocument,
			DocumentCount: 3, IndexedCount: 2,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetConversationSearchState(ctx, mode, ConversationSearchStateUpdate{
			State: ConversationSearchReady, CheckpointProjectionID: &checkpointProjection,
			CheckpointItemID: &checkpointItem, CheckpointDocumentID: &checkpointDocument,
			DocumentCount: 3, IndexedCount: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SetConversationSearchState(ctx, ConversationSearchLiteral, ConversationSearchStateUpdate{
		State: ConversationSearchReady, DocumentCount: 3, IndexedCount: 2,
	}); err == nil {
		t.Fatal("partial search index unexpectedly became ready")
	}
	versions := defaultConversationSearchVersions()
	versions.Literal = "trigram-v2"
	statuses, err := store.EnsureConversationSearchVersions(ctx, versions)
	if err != nil {
		t.Fatal(err)
	}
	byMode := searchStatusByMode(statuses)
	if got := byMode[ConversationSearchLiteral]; got.State != ConversationSearchNeedsBuild || got.Revision != 1 || got.DocumentCount != 0 || got.CheckpointItemID != nil {
		t.Fatalf("literal tokenizer invalidation = %#v", got)
	}
	if got := byMode[ConversationSearchTerms]; got.State != ConversationSearchReady || got.Revision != 0 || got.DocumentCount != 3 {
		t.Fatalf("terms changed on literal-only invalidation = %#v", got)
	}

	versions.CorpusRule = "conversation-corpus-v2"
	statuses, err = store.EnsureConversationSearchVersions(ctx, versions)
	if err != nil {
		t.Fatal(err)
	}
	byMode = searchStatusByMode(statuses)
	if got := byMode[ConversationSearchLiteral]; got.State != ConversationSearchNeedsBuild || got.Revision != 2 {
		t.Fatalf("literal corpus invalidation = %#v", got)
	}
	if got := byMode[ConversationSearchTerms]; got.State != ConversationSearchNeedsBuild || got.Revision != 1 {
		t.Fatalf("terms corpus invalidation = %#v", got)
	}
	if _, err := store.SetConversationSearchState(ctx, ConversationSearchTerms, ConversationSearchStateUpdate{State: ConversationSearchReady}); err == nil {
		t.Fatal("needs_build to ready transition unexpectedly succeeded")
	}
	tooLong := strings.Repeat("x", maxSearchErrorSummaryBytes+1)
	if _, err := store.SetConversationSearchState(ctx, ConversationSearchTerms, ConversationSearchStateUpdate{State: ConversationSearchBuilding, ErrorSummary: &tooLong}); err == nil {
		t.Fatal("oversized error summary unexpectedly succeeded")
	}
}

func TestConversationSearchVersionsInvalidateOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	markSearchModesReady(t, ctx, store)
	if _, err := store.db.ExecContext(ctx, `
UPDATE conversation_search_state
SET tokenizer_version = 'old-trigram-tokenizer'
WHERE mode = ?`, ConversationSearchLiteral); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	statuses := searchStatusByMode(mustConversationSearchStatuses(t, store))
	assertInvalidatedSearchStatus(t, statuses[ConversationSearchLiteral], 1)
	if got := statuses[ConversationSearchTerms]; got.State != ConversationSearchReady || got.Revision != 0 || got.DocumentCount != 3 || got.CheckpointItemID == nil {
		t.Fatalf("terms changed on reopen tokenizer-only invalidation = %#v", got)
	}

	if _, err := store.SetConversationSearchState(ctx, ConversationSearchLiteral, ConversationSearchStateUpdate{
		State: ConversationSearchBuilding, DocumentCount: 3, IndexedCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetConversationSearchState(ctx, ConversationSearchLiteral, ConversationSearchStateUpdate{
		State: ConversationSearchReady, DocumentCount: 3, IndexedCount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE conversation_search_state
SET corpus_rule_version = 'old-corpus-rule',
    tokenizer_version = 'old-tokenizer'
WHERE mode IN (?, ?)`, ConversationSearchLiteral, ConversationSearchTerms); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	statuses = searchStatusByMode(mustConversationSearchStatuses(t, store))
	assertInvalidatedSearchStatus(t, statuses[ConversationSearchLiteral], 2)
	assertInvalidatedSearchStatus(t, statuses[ConversationSearchTerms], 1)
}

func markSearchModesReady(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	checkpointProjection, checkpointDocument := int64(8), int64(19)
	checkpointItem := "source-key"
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		if _, err := store.SetConversationSearchState(ctx, mode, ConversationSearchStateUpdate{
			State: ConversationSearchBuilding, CheckpointProjectionID: &checkpointProjection,
			CheckpointItemID: &checkpointItem, CheckpointDocumentID: &checkpointDocument,
			DocumentCount: 3, IndexedCount: 2,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SetConversationSearchState(ctx, mode, ConversationSearchStateUpdate{
			State: ConversationSearchReady, CheckpointProjectionID: &checkpointProjection,
			CheckpointItemID: &checkpointItem, CheckpointDocumentID: &checkpointDocument,
			DocumentCount: 3, IndexedCount: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func assertInvalidatedSearchStatus(t *testing.T, status ConversationSearchStatus, wantRevision int64) {
	t.Helper()
	if status.State != ConversationSearchNeedsBuild || status.Revision != wantRevision ||
		status.DocumentCount != 0 || status.IndexedCount != 0 ||
		status.CheckpointProjectionID != nil || status.CheckpointItemID != nil || status.CheckpointDocumentID != nil ||
		status.StartedAt != nil || status.CompletedAt != nil || status.ErrorSummary != nil {
		t.Fatalf("invalidated search status = %#v", status)
	}
}

func addSearchSchemaTranscriptItem(t *testing.T, ctx context.Context, store *Store) (int64, string) {
	t.Helper()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "thread.jsonl", "search-schema-cascade", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %t, %v", stored, ok, err)
	}
	projection := searchEligibilityProjection("search-schema-cascade-projection", []TranscriptItem{
		searchEligibilityItem("cascade-item", 1, ItemUserMessage, RoleUser, "cascade text"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: "search-schema-test", CompletionRuleVersion: "search-schema-rules",
		Result: projection, IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var projectionID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM conversation_projections WHERE projection_key = ?`, projection.ProjectionKey).Scan(&projectionID); err != nil {
		t.Fatal(err)
	}
	return projectionID, "cascade-item"
}

func mustConversationSearchStatuses(t *testing.T, store *Store) []ConversationSearchStatus {
	t.Helper()
	statuses, err := store.ConversationSearchStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return statuses
}

func assertSearchStates(t *testing.T, statuses []ConversationSearchStatus, want map[ConversationSearchMode]ConversationSearchBuildState) {
	t.Helper()
	if len(statuses) != len(want) {
		t.Fatalf("status count = %d, want %d: %#v", len(statuses), len(want), statuses)
	}
	for _, status := range statuses {
		if status.State != want[status.Mode] {
			t.Fatalf("%s state = %q, want %q", status.Mode, status.State, want[status.Mode])
		}
	}
}

func searchStatusByMode(statuses []ConversationSearchStatus) map[ConversationSearchMode]ConversationSearchStatus {
	byMode := make(map[ConversationSearchMode]ConversationSearchStatus, len(statuses))
	for _, status := range statuses {
		byMode[status.Mode] = status
	}
	return byMode
}

func assertCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count for %q = %d, want %d", query, got, want)
	}
}

func assertFTSMatchCount(t *testing.T, ctx context.Context, db *sql.DB, table, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+table+` MATCH ?`, query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s match count for %q = %d, want %d", table, query, got, want)
	}
}
