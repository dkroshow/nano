package history

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreConfiguresWALAndBusyTimeout(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var journalMode string
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
	var busyTimeout int
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy timeout = %dms, want 5000ms", busyTimeout)
	}
}

func TestHistoryStoreSetsJournalSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	writable, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var writableLimit int64
	if err := writable.db.QueryRow(`PRAGMA journal_size_limit`).Scan(&writableLimit); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	var readOnlyLimit int64
	if err := readOnly.db.QueryRow(`PRAGMA journal_size_limit`).Scan(&readOnlyLimit); err != nil {
		t.Fatal(err)
	}
	const wantJournalSizeLimit = 256 << 20
	if writableLimit != wantJournalSizeLimit || readOnlyLimit != wantJournalSizeLimit {
		t.Fatalf("journal_size_limit writable=%d read-only=%d, want %d", writableLimit, readOnlyLimit, wantJournalSizeLimit)
	}
}

func TestOpenReadOnlyDoesNotCreateOrWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	writable, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	var queryOnly int
	if err := readOnly.db.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatalf("query_only = %d, %v", queryOnly, err)
	}
	if _, err := readOnly.db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (999, 'never')`); err == nil {
		t.Fatal("read-only store accepted a write")
	}

	missing := filepath.Join(t.TempDir(), "missing", "history.sqlite")
	if _, err := OpenReadOnly(missing); err == nil {
		t.Fatal("read-only open created a missing database")
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created parent directory: %v", err)
	}
}

func TestOpenMaintenanceRequiresExistingCurrentStore(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := OpenMaintenance(missing); err == nil {
		t.Fatal("maintenance open created a missing store")
	}
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	maintenance, err := OpenMaintenance(path)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close()
	var journalMode string
	var journalLimit int64
	if err := maintenance.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := maintenance.db.QueryRow(`PRAGMA journal_size_limit`).Scan(&journalLimit); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" || journalLimit != historyJournalSizeLimit {
		t.Fatalf("maintenance journal mode=%q limit=%d", journalMode, journalLimit)
	}
}

func TestStoreValidatesProjectedThreadKinds(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	validGuardian := testSource(root, "guardian.jsonl", "guardian-native", ThreadMain, nil)
	validGuardian.Result.Projection.Kind = ThreadGuardian
	if _, err := store.ApplySource(ctx, validGuardian); err != nil {
		t.Fatalf("guardian projection rejected: %v", err)
	}
	invalid := testSource(root, "invalid.jsonl", "invalid-native", ThreadMain, nil)
	invalid.Result.Projection.Kind = ThreadKind("not-a-thread-kind")
	if _, err := store.ApplySource(ctx, invalid); err == nil || !strings.Contains(err.Error(), "invalid projected thread kind") {
		t.Fatalf("invalid projected kind error = %v", err)
	}
}

func TestStorePersistsStableIDsAndPagesWithoutGaps(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nano", "history.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	root, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	first := testSource(rootSpec, "one.jsonl", "native-one", ThreadMain, &updated)
	second := testSource(rootSpec, "two.jsonl", "native-two", ThreadMain, &updated)
	third := testSource(rootSpec, "three.jsonl", "native-three", ThreadMain, nil)
	worker := testSource(rootSpec, "worker.jsonl", "native-worker", ThreadSubagent, &updated)
	worker.Result.Projection.ParentNativeID = "native-one"
	guardian := testSource(rootSpec, "guardian.jsonl", "native-guardian", ThreadMain, &updated)
	guardian.Result.Projection.Kind = ThreadGuardian
	guardian.Result.Projection.ParentNativeID = "native-one"
	firstResult, err := store.ApplySource(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []SourceProjection{second, third, worker, guardian} {
		if _, err := store.ApplySource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}

	var ids []string
	var cursor *ThreadCursor
	for {
		page, err := store.ListThreads(ctx, ListThreadsParams{Limit: 1, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			ids = append(ids, item.ID)
			if item.Kind == ThreadSubagent {
				t.Fatal("default list included subagent")
			}
		}
		cursor = page.NextCursor
		if cursor == nil {
			break
		}
	}
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("paged IDs = %#v, want three unique main threads", ids)
	}
	withWorkers, err := store.ListThreads(ctx, ListThreadsParams{IncludeSubagents: true})
	if err != nil || len(withWorkers.Items) != 4 {
		t.Fatalf("subagent list = %d, %v", len(withWorkers.Items), err)
	}
	withGuardians, err := store.ListThreads(ctx, ListThreadsParams{IncludeGuardians: true})
	if err != nil || len(withGuardians.Items) != 4 {
		t.Fatalf("guardian diagnostic list = %d, %v", len(withGuardians.Items), err)
	}
	withBoth, err := store.ListThreads(ctx, ListThreadsParams{IncludeSubagents: true, IncludeGuardians: true})
	if err != nil || len(withBoth.Items) != 5 {
		t.Fatalf("combined diagnostic list = %d, %v", len(withBoth.Items), err)
	}
	children, err := store.ListThreads(ctx, ListThreadsParams{
		IncludeSubagents: true, ParentNativeID: "native-one",
	})
	if err != nil || len(children.Items) != 1 || children.Items[0].NativeID != "native-worker" {
		t.Fatalf("parent worker lookup = %#v, %v", children, err)
	}
	for _, item := range withWorkers.Items {
		if item.NativeID == "native-worker" && item.ParentNativeID != "native-one" {
			t.Fatalf("subagent parent = %q, want native-one", item.ParentNativeID)
		}
	}
	byNativeID, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-one"})
	if err != nil || len(byNativeID.Items) != 1 || byNativeID.Items[0].ID != firstResult.ThreadID || byNativeID.NextCursor != nil {
		t.Fatalf("native ID lookup = %#v, %v", byNativeID, err)
	}
	byNanoID, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: firstResult.ThreadID})
	if err != nil || len(byNanoID.Items) != 1 || byNanoID.Items[0].NativeID != "native-one" {
		t.Fatalf("Nano ID lookup = %#v, %v", byNanoID, err)
	}
	missing, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "missing"})
	if err != nil || len(missing.Items) != 0 {
		t.Fatalf("missing ID lookup = %#v, %v", missing, err)
	}
	hiddenWorker, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-worker"})
	if err != nil || len(hiddenWorker.Items) != 0 {
		t.Fatalf("filtered subagent ID lookup = %#v, %v", hiddenWorker, err)
	}
	visibleWorker, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-worker", IncludeSubagents: true})
	if err != nil || len(visibleWorker.Items) != 1 {
		t.Fatalf("included subagent ID lookup = %#v, %v", visibleWorker, err)
	}
	directGuardian, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-guardian"})
	if err != nil || len(directGuardian.Items) != 1 || directGuardian.Items[0].Kind != ThreadGuardian {
		t.Fatalf("direct guardian lookup = %#v, %v", directGuardian, err)
	}
	directGuardianWithWorkers, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-guardian", IncludeSubagents: true})
	if err != nil || len(directGuardianWithWorkers.Items) != 1 || directGuardianWithWorkers.Items[0].Kind != ThreadGuardian {
		t.Fatalf("direct guardian lookup with include-subagents = %#v, %v", directGuardianWithWorkers, err)
	}
	visibleGuardian, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-guardian", IncludeGuardians: true})
	if err != nil || len(visibleGuardian.Items) != 1 || visibleGuardian.Items[0].ParentNativeID != "native-one" {
		t.Fatalf("diagnostic guardian lookup = %#v, %v", visibleGuardian, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	detail, err := store.GetThread(ctx, firstResult.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ID != firstResult.ThreadID || detail.NativeID != "native-one" || len(detail.Sources) != 1 {
		t.Fatalf("reopened detail = %#v", detail)
	}
	rootAgain, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil || rootAgain.ID != root.ID {
		t.Fatalf("stable root = %#v, %v; want ID %d", rootAgain, err, root.ID)
	}
	resultAgain, err := store.ApplySource(ctx, first)
	if err != nil || resultAgain.ThreadID != firstResult.ThreadID {
		t.Fatalf("stable thread = %#v, %v; want %s", resultAgain, err, firstResult.ThreadID)
	}
	if resultAgain.Changed || resultAgain.CatalogVersion != withWorkers.CatalogVersion {
		t.Fatalf("unchanged projection mutated catalog: %#v, prior version %d", resultAgain, withWorkers.CatalogVersion)
	}
	stored, ok, err := store.LoadSource(ctx, root.ID, first.Source.Path)
	if err != nil || !ok || stored.CompleteOffset != 123 || stored.CompleteLines != 3 || stored.Projection == nil ||
		stored.Projection.NativeThreadID != "native-one" {
		t.Fatalf("stored append checkpoint = %#v, %v, %v", stored, ok, err)
	}
}

func TestStoreReturnsWriteLockFailureAndSucceedsAfterRelease(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	root, err := store.EnsureRoot(ctx, SourceRoot{
		Provider: ProviderCodex, Kind: RootCodexSessions,
		Path: filepath.Join(t.TempDir(), "sessions"),
	})
	if err != nil {
		t.Fatalf("ensure root: %v", err)
	}

	store.db.SetMaxOpenConns(1)
	if _, err := store.db.ExecContext(ctx, `PRAGMA busy_timeout=10`); err != nil {
		t.Fatalf("shorten store busy timeout: %v", err)
	}
	locker, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	defer locker.Close()
	lockTx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer lockTx.Rollback()
	if _, err := lockTx.ExecContext(ctx, `UPDATE catalog_state SET catalog_version = catalog_version`); err != nil {
		t.Fatalf("acquire write lock: %v", err)
	}

	if err := store.SetIndexState(ctx, root.ID, IndexScanning, nil); err == nil {
		t.Fatal("write reported success while another connection held the SQLite write lock")
	}
	if err := lockTx.Rollback(); err != nil {
		t.Fatalf("release write lock: %v", err)
	}
	if err := store.SetIndexState(ctx, root.ID, IndexScanning, nil); err != nil {
		t.Fatalf("write after lock release: %v", err)
	}
}

func TestStoreReconciliationRetainsMissingAndFailedProjection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	root, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC)
	source := testSource(rootSpec, "session.jsonl", "claude-session", ThreadMain, &updated)
	applied, err := store.ApplySource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	failure := source
	failure.Result = nil
	failure.Status = SourceError
	errorText := "provider history parser failed"
	failure.ErrorSummary = &errorText
	if _, err := store.ApplySource(ctx, failure); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetThread(ctx, applied.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Source.Status != SourceError || detail.NativeID != "claude-session" {
		t.Fatalf("failed projection did not preserve last good thread: %#v", detail)
	}
	if _, err := store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, State: IndexDegraded, CompletedAt: updated.Add(30 * time.Minute),
		Error: &errorText,
	}); err != nil {
		t.Fatal(err)
	}
	detail, err = store.GetThread(ctx, applied.ThreadID)
	if err != nil || detail.Source.Status != SourceError {
		t.Fatalf("degraded reconciliation marked evidence missing: %#v, %v", detail, err)
	}

	reconciled, err := store.ReconcileRoot(ctx, RootReconciliation{
		RootID: root.ID, State: IndexReady, CompletedAt: updated.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reconciled.Changed || reconciled.SourceCount != 0 {
		t.Fatalf("reconciliation = %#v", reconciled)
	}
	detail, err = store.GetThread(ctx, applied.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Source.Status != SourceMissing || detail.Sources[0].Status != SourceMissing {
		t.Fatalf("missing source detail = %#v", detail)
	}
	status, err := store.IndexStatus(ctx)
	if err != nil || status.State != IndexReady || len(status.Roots) != 1 || status.Roots[0].SourceCount != 0 {
		t.Fatalf("index status = %#v, %v", status, err)
	}
}

func TestStoreMigratesGrokSummarySourceToUpdates(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	summary := testSource(root, filepath.Join("encoded", "session-1", "summary.json"), "grok-native", ThreadMain, nil)
	summary.Source.Device, summary.Source.Inode, summary.Source.Size = 1, 10, 100
	first, err := store.ApplySource(ctx, summary)
	if err != nil {
		t.Fatal(err)
	}

	updates := testSource(root, filepath.Join("encoded", "session-1", "updates.jsonl"), "grok-native", ThreadMain, nil)
	updates.Source.Device, updates.Source.Inode, updates.Source.Size = 1, 11, 200
	updates.Result.CompleteOffset = 200
	second, err := store.ApplySource(ctx, updates)
	if err != nil {
		t.Fatal(err)
	}
	if second.ThreadID != first.ThreadID {
		t.Fatalf("Grok source transition changed thread %q to %q", first.ThreadID, second.ThreadID)
	}
	detail, err := store.GetThread(ctx, first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Source.Conflict || len(detail.Sources) != 1 || filepath.Base(detail.Sources[0].Path) != "updates.jsonl" {
		t.Fatalf("Grok source transition = %#v", detail)
	}
	rootRecord, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadSource(ctx, rootRecord.ID, summary.Source.Path); err != nil || exists {
		t.Fatalf("legacy summary source remains: exists=%v err=%v", exists, err)
	}
	migrated, exists, err := store.LoadSource(ctx, rootRecord.ID, updates.Source.Path)
	if err != nil || !exists || migrated.Generation != 2 {
		t.Fatalf("migrated update source = %#v, exists=%v err=%v", migrated, exists, err)
	}
}

func TestStoreDuplicateNativeIdentityIsExplicitConflict(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	firstInput := testSource(root, "a.jsonl", "same-native", ThreadMain, nil)
	firstTitle := "first authority"
	firstInput.Result.Projection.Title = &firstTitle
	first, err := store.ApplySource(ctx, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	secondInput := testSource(root, "b.jsonl", "same-native", ThreadMain, nil)
	secondTitle := "conflicting authority"
	secondInput.Result.Projection.Title = &secondTitle
	second, err := store.ApplySource(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if first.ThreadID != second.ThreadID {
		t.Fatalf("duplicate native identities minted separate rows: %q, %q", first.ThreadID, second.ThreadID)
	}
	detail, err := store.GetThread(ctx, first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Source.Conflict || len(detail.Sources) != 2 {
		t.Fatalf("duplicate identity not exposed as conflict: %#v", detail)
	}
	if detail.Title == nil || *detail.Title != firstTitle {
		t.Fatalf("conflicting source replaced canonical metadata: %#v", detail.Title)
	}
	_, err = store.GetThread(ctx, "nth_missing")
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("missing thread error = %v", err)
	}
}

func TestStoreOrdersFixedWidthTimestampsChronologically(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	exact := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	fractional := exact.Add(100 * time.Millisecond)
	if _, err := store.ApplySource(ctx, testSource(root, "exact.jsonl", "exact", ThreadMain, &exact)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplySource(ctx, testSource(root, "fractional.jsonl", "fractional", ThreadMain, &fractional)); err != nil {
		t.Fatal(err)
	}
	first, err := store.ListThreads(ctx, ListThreadsParams{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.Items[0].NativeID != "fractional" || first.NextCursor == nil {
		t.Fatalf("first chronological page = %#v", first)
	}
	second, err := store.ListThreads(ctx, ListThreadsParams{Limit: 1, After: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].NativeID != "exact" {
		t.Fatalf("second chronological page = %#v", second)
	}
}

func TestStoreMigratesVariableWidthTimestamps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	updated := time.Date(2026, 7, 9, 12, 0, 0, 100_000_000, time.UTC)
	applied, err := store.ApplySource(ctx, testSource(root, "legacy.jsonl", "legacy", ThreadMain, &updated))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, "2026-07-09T12:00:00.1Z", applied.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 2`); err != nil {
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
	var raw string
	if err := store.db.QueryRowContext(ctx, `SELECT updated_at FROM threads WHERE id = ?`, applied.ThreadID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "2026-07-09T12:00:00.100000000Z" {
		t.Fatalf("migrated timestamp = %q", raw)
	}
}

func TestTranscriptEvidenceHashMigrationIsAdditiveAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 4`); err != nil {
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
	var migrations int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 4`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	hasColumn, err := sqliteTableHasColumn(ctx, tx, "conversation_items", "source_line_sha256")
	if err != nil || !hasColumn || migrations != 1 {
		t.Fatalf("v4 migration: column=%v migrations=%d err=%v", hasColumn, migrations, err)
	}
}

func TestThreadNamesMigrationIsAdditiveAndIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 6`); err != nil {
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
	var migrations int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	hasTable, err := sqliteTableHasColumn(ctx, tx, "thread_names", "provenance")
	if err != nil || !hasTable || migrations != 1 {
		t.Fatalf("v6 migration: hasColumn=%v migrations=%d err=%v", hasTable, migrations, err)
	}
}

func TestThreadNamesDerivedCopyOverlayAndChangeDetection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	updated := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	applied, err := store.ApplySource(ctx, testSource(rootSpec, "session.jsonl", "claude-session", ThreadMain, &updated))
	if err != nil {
		t.Fatal(err)
	}

	overlayFor := func(id string) *ThreadNameRef {
		t.Helper()
		detail, err := store.GetThread(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return detail.Name
	}
	listVersion := func() uint64 {
		t.Helper()
		page, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "claude-session"})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("list = %#v, %v", page, err)
		}
		return page.CatalogVersion
	}

	if overlayFor(applied.ThreadID) != nil {
		t.Fatal("thread carried a name overlay before any names file was loaded")
	}
	baseVersion := listVersion()

	recordTime := time.Now().UTC()
	record := ThreadNameRecord{Provider: ProviderClaude, NativeThreadID: "claude-session", Name: "influence graph classifier", Provenance: ThreadNameManual, UpdatedAt: &recordTime}
	changed, err := store.SyncThreadNames(ctx, []ThreadNameRecord{record})
	if err != nil || !changed {
		t.Fatalf("first sync = %v, %v; want changed", changed, err)
	}
	afterInsert := listVersion()
	if afterInsert <= baseVersion {
		t.Fatalf("insert did not bump catalog version: %d -> %d", baseVersion, afterInsert)
	}
	if ref := overlayFor(applied.ThreadID); ref == nil || ref.Value != "influence graph classifier" || ref.Provenance != ThreadNameManual {
		t.Fatalf("detail overlay = %#v", ref)
	}
	page, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "claude-session"})
	if err != nil || page.Items[0].Name == nil || page.Items[0].Name.Value != "influence graph classifier" {
		t.Fatalf("list overlay = %#v, %v", page.Items, err)
	}
	named, err := store.ThreadNamesNamed(ctx, "influence graph classifier")
	if err != nil || len(named) != 1 || named[0].NativeThreadID != "claude-session" {
		t.Fatalf("ThreadNamesNamed = %#v, %v", named, err)
	}
	if missing, err := store.ThreadNamesNamed(ctx, "absent name"); err != nil || len(missing) != 0 {
		t.Fatalf("ThreadNamesNamed absent = %#v, %v", missing, err)
	}

	// A no-op sync must not churn the catalog version.
	changed, err = store.SyncThreadNames(ctx, []ThreadNameRecord{record})
	if err != nil || changed {
		t.Fatalf("no-op sync = %v, %v; want unchanged", changed, err)
	}
	if listVersion() != afterInsert {
		t.Fatalf("no-op sync advanced catalog version from %d", afterInsert)
	}

	updatedTimestamp := record
	updatedAt := record.UpdatedAt.Add(time.Minute)
	updatedTimestamp.UpdatedAt = &updatedAt
	changed, err = store.SyncThreadNames(ctx, []ThreadNameRecord{updatedTimestamp})
	if err != nil || changed {
		t.Fatalf("timestamp-only sync = %v, %v; want unchanged", changed, err)
	}
	if listVersion() != afterInsert {
		t.Fatalf("timestamp-only sync advanced catalog version from %d", afterInsert)
	}

	// A real rename is a change and updates the overlay.
	renamed := record
	renamed.Name = "renamed classifier"
	changed, err = store.SyncThreadNames(ctx, []ThreadNameRecord{renamed})
	if err != nil || !changed {
		t.Fatalf("rename sync = %v, %v; want changed", changed, err)
	}
	afterRename := listVersion()
	if afterRename <= afterInsert {
		t.Fatalf("rename did not bump catalog version: %d -> %d", afterInsert, afterRename)
	}
	if ref := overlayFor(applied.ThreadID); ref == nil || ref.Value != "renamed classifier" {
		t.Fatalf("renamed overlay = %#v", ref)
	}

	// Removing the record from the set deletes the derived row.
	changed, err = store.SyncThreadNames(ctx, nil)
	if err != nil || !changed {
		t.Fatalf("delete sync = %v, %v; want changed", changed, err)
	}
	if overlayFor(applied.ThreadID) != nil {
		t.Fatal("overlay survived deletion from the truth set")
	}
	if listVersion() <= afterRename {
		t.Fatalf("delete did not bump catalog version from %d", afterRename)
	}
}

func withOrigin(source SourceProjection, origin string) SourceProjection {
	evidence := EvidenceNativeDurable
	source.Result.Projection.Metadata.Origin = MetadataField{State: MetadataKnown, Value: &origin, Evidence: &evidence}
	return source
}

func TestStoreProgrammaticFilterAndInteractionCounts(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	if _, err := store.EnsureRoot(ctx, rootSpec); err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	guardian := withOrigin(testSource(rootSpec, "guardian.jsonl", "native-guardian-sdk", ThreadMain, &updated), "sdk-cli")
	guardian.Result.Projection.Kind = ThreadGuardian
	sources := []SourceProjection{
		withOrigin(testSource(rootSpec, "interactive.jsonl", "native-cli", ThreadMain, &updated), "cli"),
		withOrigin(testSource(rootSpec, "programmatic.jsonl", "native-sdk", ThreadMain, &updated), "sdk-cli"),
		testSource(rootSpec, "unclassified.jsonl", "native-unknown", ThreadMain, &updated),
		withOrigin(testSource(rootSpec, "worker.jsonl", "native-worker-sdk", ThreadSubagent, &updated), "sdk-cli"),
		guardian,
	}
	for index, source := range sources {
		if _, err := store.ApplySource(ctx, source); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			// Counts are cached by catalog version; a status read here must not
			// leave the final tally below stale.
			if _, err := store.IndexStatus(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Default hides programmatic; subagents are also hidden by default.
	byDefault, err := store.ListThreads(ctx, ListThreadsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(byDefault.Items) != 2 {
		t.Fatalf("default list = %d items, want 2 (interactive + unclassified)", len(byDefault.Items))
	}
	for _, item := range byDefault.Items {
		if item.Interaction == InteractionProgrammatic {
			t.Fatalf("default list leaked programmatic thread %q", item.NativeID)
		}
	}

	withProgrammatic, err := store.ListThreads(ctx, ListThreadsParams{IncludeProgrammatic: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withProgrammatic.Items) != 3 {
		t.Fatalf("programmatic list = %d items, want 3 non-subagent", len(withProgrammatic.Items))
	}

	// Exact threadId lookup composes with the programmatic filter.
	hidden, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-sdk"})
	if err != nil || len(hidden.Items) != 0 {
		t.Fatalf("programmatic threadId lookup with filter off = %#v, %v", hidden.Items, err)
	}
	shown, err := store.ListThreads(ctx, ListThreadsParams{ThreadID: "native-sdk", IncludeProgrammatic: true})
	if err != nil || len(shown.Items) != 1 || shown.Items[0].Interaction != InteractionProgrammatic {
		t.Fatalf("programmatic threadId lookup with filter on = %#v, %v", shown.Items, err)
	}

	// Counts exclude both delegated workers and Guardian permission reviewers.
	status, err := store.IndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := InteractionCounts{Interactive: 1, Programmatic: 1, Unclassified: 1}
	if status.InteractionCounts != want {
		t.Fatalf("interaction counts = %#v, want %#v", status.InteractionCounts, want)
	}
}

func withCreatedAt(source SourceProjection, createdAt time.Time) SourceProjection {
	source.Result.Projection.CreatedAt = &createdAt
	return source
}

func TestStoreTimeBounds(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	if _, err := store.EnsureRoot(ctx, rootSpec); err != nil {
		t.Fatal(err)
	}
	day := func(d int) time.Time { return time.Date(2026, 7, d, 12, 0, 0, 0, time.UTC) }
	up1, up2, up3 := day(15), day(16), day(17)
	sources := []SourceProjection{
		withCreatedAt(testSource(rootSpec, "a.jsonl", "native-a", ThreadMain, &up3), day(1)),
		withCreatedAt(testSource(rootSpec, "b.jsonl", "native-b", ThreadMain, &up1), day(2)),
		withCreatedAt(testSource(rootSpec, "c.jsonl", "native-c", ThreadMain, &up2), day(3)),
	}
	for _, source := range sources {
		if _, err := store.ApplySource(ctx, source); err != nil {
			t.Fatal(err)
		}
	}
	order := func(page ThreadPage) []string {
		ids := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			ids = append(ids, item.NativeID)
		}
		return ids
	}

	// Fixed order: updated desc.
	byDefault, err := store.ListThreads(ctx, ListThreadsParams{})
	if err != nil || !reflect.DeepEqual(order(byDefault), []string{"native-a", "native-c", "native-b"}) {
		t.Fatalf("default order = %v, %v", order(byDefault), err)
	}

	// Inclusive created bounds compose with everything else.
	from, to := day(2), day(3)
	bounded, err := store.ListThreads(ctx, ListThreadsParams{CreatedFrom: &from, CreatedTo: &to})
	if err != nil || !reflect.DeepEqual(order(bounded), []string{"native-c", "native-b"}) {
		t.Fatalf("created-bounded order = %v, %v", order(bounded), err)
	}
	upFrom := day(17)
	updatedBounded, err := store.ListThreads(ctx, ListThreadsParams{UpdatedFrom: &upFrom})
	if err != nil || !reflect.DeepEqual(order(updatedBounded), []string{"native-a"}) {
		t.Fatalf("updated-bounded order = %v, %v", order(updatedBounded), err)
	}
}

func testSource(root SourceRoot, name, nativeID string, kind ThreadKind, updatedAt *time.Time) SourceProjection {
	created := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	indexed := time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)
	model, evidence := "test-model", EvidenceNativeDurable
	metadata := Metadata{
		CWD: UnknownField(), Model: MetadataField{State: MetadataKnown, Value: &model, Evidence: &evidence},
		Effort: UnknownField(), Priority: UnknownField(), Permission: UnknownField(),
		HistoricalBranch: UnknownField(),
	}
	projection := ProjectionResult{
		Projection: ThreadProjection{Provider: root.Provider, NativeThreadID: nativeID,
			IdentityEvidence: EvidenceNativeDurable, Kind: kind, CreatedAt: &created,
			UpdatedAt: updatedAt, Metadata: metadata},
		CompleteOffset: 123, CompleteLines: 3,
	}
	return SourceProjection{
		Source: SourceCandidate{Root: root, Path: filepath.Join(root.Path, name),
			RelativePath: name, Kind: kind, Archived: root.Archived, Size: 123, ModTime: created,
			Device: 1, Inode: uint64(len(name) + int(name[0]))},
		Result: &projection, ParserVersion: "test-v1", Status: SourcePresent, IndexedAt: indexed,
	}
}

// TestTurnItemIndexMigratesWithRowsIntact catches the per-turn scans that made
// a transcript rebuild cost turns times items: writing a parsed turn looks up
// its items by (projection_id, turn_key) twice, and with only
// projection-leading indexes each lookup scanned the whole projection.
// Boundary: opening a v25 store that already holds transcript rows. The v26
// migration adds the index on both keys, records itself once, and changes no
// row.
func TestTurnItemIndexMigratesWithRowsIntact(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	projectionID, _ := addSearchSchemaTranscriptItem(t, ctx, store)
	if _, err := store.db.Exec(`DROP INDEX IF EXISTS conversation_items_turn; DELETE FROM schema_migrations WHERE version=26`); err != nil {
		t.Fatal(err)
	}
	before := transcriptItemRows(t, store, projectionID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var migration, index int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=26`).Scan(&migration); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='conversation_items_turn'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if migration != 1 || index != 1 {
		t.Fatalf("migration rows=%d index rows=%d, want 1 each", migration, index)
	}
	var columns []string
	rows, err := store.db.Query(`SELECT name FROM pragma_index_info('conversation_items_turn') ORDER BY seqno`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	rows.Close()
	if want := []string{"projection_id", "turn_key"}; !reflect.DeepEqual(columns, want) {
		t.Fatalf("conversation_items_turn columns = %v, want %v", columns, want)
	}
	if after := transcriptItemRows(t, store, projectionID); len(before) == 0 || !reflect.DeepEqual(after, before) {
		t.Fatalf("item rows changed across the migration:\nbefore %v\nafter  %v", before, after)
	}
	for _, statement := range []string{
		`SELECT COALESCE(MIN(projected_seq),0),COALESCE(MAX(projected_seq),0) FROM conversation_items WHERE projection_id=1 AND turn_key='turn'`,
		`UPDATE conversation_items SET is_final_answer = CASE WHEN item_id = 'item' THEN 1 ELSE 0 END WHERE projection_id = 1 AND turn_key = 'turn'`,
	} {
		if plan := explainQueryPlan(t, store, statement); !strings.Contains(plan, "conversation_items_turn") {
			t.Fatalf("per-turn lookup does not use conversation_items_turn: %s\n%s", plan, statement)
		}
	}
}

func transcriptItemRows(t *testing.T, store *Store, projectionID int64) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT item_id, turn_key, projected_seq, item_type, is_final_answer FROM conversation_items WHERE projection_id = ? ORDER BY projected_seq`, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var itemID, turnKey, itemType string
		var seq, final int64
		if err := rows.Scan(&itemID, &turnKey, &seq, &itemType, &final); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%d|%s|%d", itemID, turnKey, seq, itemType, final))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMessagePageIndexMigratesAndServesEnhancedPages catches an Enhanced page
// that walks every hidden row again: SQLite uses a partial index only for a
// query that repeats its predicate. Boundary: opening a v26 store that holds
// transcript rows, then reading an Enhanced page that starts mid-turn. The v27
// migration adds the index, records itself once and changes no row, and each
// of the page's four item reads (the page, the turn extension's count and
// rows, and the older-rows check) reads through the index.
func TestMessagePageIndexMigratesAndServesEnhancedPages(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(root, "thread.jsonl", "message-page-index", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	// One turn: a prompt, hidden reasoning, the answer.
	projection := searchEligibilityProjection("message-page-index", []TranscriptItem{
		searchEligibilityItem("prompt", 1, ItemUserMessage, RoleUser, "question"),
		searchEligibilityItem("thinking", 2, ItemReasoning, RoleAssistant, "hidden"),
		searchEligibilityItem("answer", 3, ItemAgentMessage, RoleAssistant, "answer"),
	}, nil)
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderCodex, ParserVersion: "message-index-test", CompletionRuleVersion: "message-index-test",
		Result: projection, IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var projectionID int64
	if err := store.db.QueryRow(`SELECT id FROM conversation_projections WHERE projection_key = ?`, projection.ProjectionKey).Scan(&projectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX conversation_items_messages_page; DELETE FROM schema_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	before := transcriptItemRows(t, store, projectionID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := registerRecordingDriver(t)
	store, err = openStore(recordingDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var migration, index int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=27`).Scan(&migration); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='conversation_items_messages_page'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if migration != 1 || index != 1 {
		t.Fatalf("migration rows=%d index rows=%d, want 1 each", migration, index)
	}
	if after := transcriptItemRows(t, store, projectionID); len(before) == 0 || !reflect.DeepEqual(after, before) {
		t.Fatalf("item rows changed across the migration:\nbefore %v\nafter  %v", before, after)
	}
	recorder.reset()
	// One row cuts the turn, so the page extends back to the prompt.
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, Limit: 1, MessagesOnly: true})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "prompt" || page.Items[1].ID != "answer" {
		t.Fatalf("Enhanced page = %#v, %v", page, err)
	}
	// The four item reads, told apart by their shape, not by the predicate.
	const rangeWhere = "FROM conversation_items\nWHERE projection_id = ? AND projected_seq >= ? AND projected_seq < ?"
	reads := map[string]func(string) bool{
		"page": func(query string) bool {
			return strings.HasSuffix(strings.TrimSpace(query), "ORDER BY projected_seq DESC LIMIT ?")
		},
		"extension count": func(query string) bool { return strings.Contains(query, "SELECT COUNT(*) "+rangeWhere) },
		"extension rows": func(query string) bool {
			return strings.Contains(query, rangeWhere) && strings.HasSuffix(strings.TrimSpace(query), "ORDER BY projected_seq")
		},
		"older check": func(query string) bool {
			return strings.Contains(query, "SELECT EXISTS(\n  SELECT 1 FROM conversation_items WHERE projection_id = ? AND projected_seq < ?")
		},
	}
	seen := map[string]bool{}
	for _, statement := range recorder.statements() {
		read := ""
		for name, matches := range reads {
			if matches(statement.query) {
				read = name
			}
		}
		if read == "" {
			continue
		}
		seen[read] = true
		var details []string
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+statement.query, statement.args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		rows.Close()
		if plan := strings.Join(details, "; "); !strings.Contains(plan, "conversation_items_messages_page") {
			t.Fatalf("Enhanced %s does not use the message index: %s\n%s", read, plan, statement.query)
		}
	}
	for name := range reads {
		if !seen[name] {
			t.Errorf("Enhanced page issued no %s read", name)
		}
	}
}

const recordingDriverName = "sqlite-recording"

type recordedStatement struct {
	query string
	args  []any
}

// statementRecorder keeps every query a recording connection runs, with its
// arguments, so a test can explain the exact statements the store issued.
type statementRecorder struct {
	mu   sync.Mutex
	list []recordedStatement
}

func (r *statementRecorder) reset() {
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

func (r *statementRecorder) statements() []recordedStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedStatement(nil), r.list...)
}

var (
	recordingRegisterOnce sync.Once
	queryRecorder         = &statementRecorder{}
)

func registerRecordingDriver(t *testing.T) *statementRecorder {
	t.Helper()
	recordingRegisterOnce.Do(func() {
		probe, err := sql.Open("sqlite", "file:recording-probe?mode=memory")
		if err != nil {
			t.Fatalf("probe sqlite driver: %v", err)
		}
		base := probe.Driver()
		if err := probe.Close(); err != nil {
			t.Fatalf("close probe: %v", err)
		}
		sql.Register(recordingDriverName, recordingDriver{Driver: base})
	})
	return queryRecorder
}

type recordingDriver struct{ driver.Driver }

func (d recordingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return recordingConn{Conn: conn}, nil
}

// modernc's conn has no Queryer fast path, so every statement is prepared.
// BeginTx passes through for the store's read-only transactions.
type recordingConn struct{ driver.Conn }

func (c recordingConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return recordingStmt{Stmt: stmt, query: query}, nil
}

func (c recordingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

type recordingStmt struct {
	driver.Stmt
	query string
}

func (s recordingStmt) Query(args []driver.Value) (driver.Rows, error) {
	values := make([]any, len(args))
	for index, arg := range args {
		values[index] = arg
	}
	queryRecorder.mu.Lock()
	queryRecorder.list = append(queryRecorder.list, recordedStatement{query: s.query, args: values})
	queryRecorder.mu.Unlock()
	return s.Stmt.Query(args)
}

// Failure caught: every slow write hold logged its own line, 13% of a five-day
// server log. Boundary: the write mutex's diagnostic. The first slow hold still
// logs at once, later ones within the minute are counted, and the next line
// carries that count.
func TestSlowWriteHoldsLogOncePerMinuteWithACount(t *testing.T) {
	var logs bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prior) })
	var m timedWriteMutex
	hold := func() {
		m.Lock()
		time.Sleep(slowWriteHold + 10*time.Millisecond)
		m.Unlock()
	}
	hold()
	hold()
	if lines := strings.Count(logs.String(), "slow write hold"); lines != 1 {
		t.Fatalf("lines within one minute = %d, want 1:\n%s", lines, logs.String())
	}
	m.slowLogged = m.slowLogged.Add(-slowWriteLogInterval)
	hold()
	if lines := strings.Count(logs.String(), "slow write hold"); lines != 2 || !strings.Contains(logs.String(), "; 1 more since the last line") {
		t.Fatalf("log after the minute =\n%s", logs.String())
	}
}

// statedb.Open is authoritative; this test-only opener exists to count statements.
func openStore(driverName, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("history database path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve history database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create history data directory: %w", err)
	}

	u := url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Set("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", historyJournalSizeLimit))
	u.RawQuery = query.Encode()
	db, err := sql.Open(driverName, u.String())
	if err != nil {
		return nil, fmt.Errorf("open history database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(abs, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect history database: %w", err)
	}
	return store, nil
}
