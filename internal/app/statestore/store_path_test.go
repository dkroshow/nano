package statestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

func TestOpenDatabaseSecuresMainAndJournalsBeforeUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.sqlite")
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE private_fact(value TEXT); INSERT INTO private_fact VALUES('private')`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe file %s: %v %v", suffix, info, err)
		}
	}
}

func TestOpenDatabaseRejectsCancelledOrStrandedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if db, err := statedb.Open(ctx, path); err == nil {
		db.Close()
		t.Fatal("cancelled open succeeded")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled open created a database")
	}
	if err := os.WriteFile(path+"-wal", []byte("stranded"), 0600); err != nil {
		t.Fatal(err)
	}
	if db, err := statedb.Open(context.Background(), path); err == nil {
		db.Close()
		t.Fatal("stranded journal accepted")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("stranded journal created an empty database")
	}
}

func TestPrepareStorePublishesSchema21FromPrivateSchema20Candidate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	want := []turns.QueuedTurn{{
		ClientActionID: "retained", Provider: "codex", HostedThreadID: "hosted",
		NativeThreadID: "native", Text: "private prompt", PresentationText: "private prompt",
		TextHash: "text-hash", RequestHash: "request-hash", State: "accepted", Origin: "human",
		CreatedAt: "2026-09-11T00:00:00Z", ProviderTurnID: "turn",
	}}
	createSchema20Store(t, path, want)
	backupRoot := filepath.Join(dir, "backups")
	if _, err := PrepareStore(ctx, StorePaths{Destination: path, BackupRoot: backupRoot}, testMigrations()); err != nil {
		t.Fatal(err)
	}
	db, err := OpenReadOnly(ctx, path, testMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != turns.TurnSchemaVersion {
		t.Fatalf("published version = %d, %v", version, err)
	}
	if got, err := turns.LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("published queue = %+v, %v", got, err)
	}
	originals, err := filepath.Glob(filepath.Join(backupRoot, "threads-schema-*", "nano-threads-v20.sqlite"))
	if err != nil || len(originals) != 1 {
		t.Fatalf("retained schema-20 originals = %v, %v", originals, err)
	}
}

// A schema-22 store reaches schema 23 only through a private candidate: the
// published file carries the new columns with no owed or resolved rows, keeps
// every prompt, and the untouched schema-22 original stays in the backups.
func TestPrepareStorePublishesSchema23FromPrivateSchema22Candidate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	want := []turns.QueuedTurn{{ClientActionID: "retained", Provider: "claude", HostedThreadID: "hosted", Text: "keep", State: "uncertain"}}
	createSchema20Store(t, path, want)
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnTransactions(ctx, db, filepath.Join(dir, "v20.sqlite")); err != nil {
		t.Fatal(err)
	}
	if err := migrateCompletedTurnUsage(ctx, db, filepath.Join(dir, "v21.sqlite"), testMigrations()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backupRoot := filepath.Join(dir, "backups")
	if _, err := PrepareStore(ctx, StorePaths{Destination: path, BackupRoot: backupRoot}, testMigrations()); err != nil {
		t.Fatal(err)
	}
	db, err = OpenReadOnly(ctx, path, testMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, owed int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != turns.TurnSchemaVersion {
		t.Fatalf("published version = %d, %v", version, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM queued_turns WHERE member_owed<>0`).Scan(&owed); err != nil || owed != 0 {
		t.Fatalf("migration backfilled owed rows: %d, %v", owed, err)
	}
	if got, err := turns.LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("published queue = %+v, %v", got, err)
	}
	originals, err := filepath.Glob(filepath.Join(backupRoot, "threads-schema-*", "nano-threads-v22.sqlite"))
	if err != nil || len(originals) != 1 {
		t.Fatalf("retained schema-22 originals = %v, %v", originals, err)
	}
}

func TestPrepareStoreFailedSchema21CandidateLeavesCanonicalSchema20(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	want := []turns.QueuedTurn{{ClientActionID: "retained", Provider: "claude", HostedThreadID: "hosted", Text: "keep", State: "uncertain"}}
	createSchema20Store(t, path, want)
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// The candidate's v21 index creation collides with this synthetic future
	// object. The source still passes the v20 receipt and integrity checks.
	if _, err := db.Exec(`CREATE TABLE queued_turns_binding_claim(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareStore(ctx, StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "backups")}, testMigrations()); err == nil {
		t.Fatal("failed candidate replaced the canonical store")
	}
	db, err = statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, transactionalColumn int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('queued_turns') WHERE name='revision'`).Scan(&transactionalColumn); err != nil {
		t.Fatal(err)
	}
	if version != turns.LegacyTurnSchemaVersion || transactionalColumn != 0 {
		t.Fatalf("failed publication changed canonical schema: version=%d column=%d", version, transactionalColumn)
	}
	if got, err := turns.LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("failed publication changed canonical rows: %+v, %v", got, err)
	}
}

func TestPrepareStoreSchema20UpgradeIgnoresChangedLegacySource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	completedPath := filepath.Join(dir, "turn-completed.sqlite")
	completed, err := statedb.Open(ctx, completedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completed.Exec(turns.CompletedTurnSchema + `;
		INSERT INTO completed_turns(id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at)
		VALUES(7,'hosted','codex','turn-7','final-7','2026-09-14T07:00:00Z','provider','2026-09-14T07:00:00Z')`); err != nil {
		completed.Close()
		t.Fatal(err)
	}
	if err := completed.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(dir, "nano_threads.sqlite")
	paths := StorePaths{
		Destination: destination,
		BackupRoot:  filepath.Join(dir, "backups"),
		Turns:       turns.LegacyTurnPaths{Completed: completedPath},
	}
	createSchema20StoreWithSources(t, destination, nil, paths)

	completed, err = statedb.Open(ctx, completedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completed.Exec(`
		INSERT INTO completed_turns(target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at)
		VALUES('hosted','codex','turn-8','final-8','2026-09-14T07:17:00Z','provider','2026-09-14T07:17:00Z')`); err != nil {
		completed.Close()
		t.Fatal(err)
	}
	if err := completed.Close(); err != nil {
		t.Fatal(err)
	}

	legacyChecks := 0
	if _, err := prepareStore(ctx, paths, func(ctx context.Context, db *sql.DB, paths StorePaths) error {
		legacyChecks++
		return verifySourceFingerprints(ctx, db, paths)
	}, testMigrations()); err != nil {
		t.Fatalf("schema-20 upgrade reread a legacy source: %v", err)
	}
	if legacyChecks != 0 {
		t.Fatalf("schema-20 upgrade checked legacy sources %d times", legacyChecks)
	}
	db, err := sql.Open("sqlite", statedb.URI(destination, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != turns.TurnSchemaVersion {
		t.Fatalf("published version = %d, %v", version, err)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("published integrity = %q, %v", integrity, err)
	}
	var completedRows int
	if err := db.QueryRow(`SELECT count(*) FROM completed_turns`).Scan(&completedRows); err != nil || completedRows != 1 {
		t.Fatalf("canonical completed rows = %d, %v", completedRows, err)
	}
}

func createSchema20Store(t *testing.T, path string, queue []turns.QueuedTurn) {
	t.Helper()
	createSchema20StoreWithSources(t, path, queue, StorePaths{Destination: path, BackupRoot: filepath.Join(filepath.Dir(path), "unused")})
}

// Failure caught: retiring the unused transcript_boundaries table loses its
// rows with no copy, or takes hosted threads or prompts with it (the table
// cascades from hosted_threads). Boundary: PrepareStore on disposable stores,
// one created fresh and one an older Nano left holding the table.
func TestPrepareStoreBacksUpThenDropsTranscriptBoundaries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	paths := StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "backups")}
	backups := func() []string {
		t.Helper()
		found, err := filepath.Glob(filepath.Join(paths.BackupRoot, "transcript-boundaries-*", "nano-threads-v*.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	if _, err := PrepareStore(ctx, paths, testMigrations()); err != nil {
		t.Fatal(err)
	}
	if found := backups(); len(found) != 0 {
		t.Fatalf("a fresh store was backed up for a table it never had: %v", found)
	}

	// An older Nano created the table on every open and left a row in it.
	want := []turns.QueuedTurn{{ClientActionID: "kept-prompt", Provider: "codex", HostedThreadID: "kept", NativeThreadID: "native", Text: "keep", State: "accepted"}}
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,revision,created_at,updated_at) VALUES('kept','codex','native','shared',1,'before','before')`,
		`CREATE TABLE transcript_boundaries (
			hosted_thread_id TEXT NOT NULL PRIMARY KEY REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE,
			record_json TEXT NOT NULL,
			revision INTEGER NOT NULL,
			updated_at TEXT NOT NULL)`,
		`INSERT INTO transcript_boundaries VALUES('kept','{"segments":[]}',1,'before')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := turns.SaveQueuedTurns(ctx, db, want); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for open := 1; open <= 2; open++ {
		if _, err := PrepareStore(ctx, paths, testMigrations()); err != nil {
			t.Fatalf("prepare %d: %v", open, err)
		}
		db, err := OpenReadOnly(ctx, path, testMigrations())
		if err != nil {
			t.Fatal(err)
		}
		var tables, hosted int
		var raw string
		var report transcriptBoundariesReport
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='transcript_boundaries'`).Scan(&tables); err != nil || tables != 0 {
			t.Fatalf("prepare %d: transcript_boundaries still present: %d, %v", open, tables, err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id='kept'`).Scan(&hosted); err != nil || hosted != 1 {
			t.Fatalf("prepare %d: hosted thread rows = %d, %v", open, hosted, err)
		}
		if got, err := turns.LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("prepare %d: prompts = %+v, %v", open, got, err)
		}
		if err := db.QueryRow(`SELECT report_json FROM threads_store_migrations WHERE name=?`, transcriptBoundariesRetirement).Scan(&raw); err != nil {
			t.Fatalf("prepare %d: no retirement receipt: %v", open, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		found := backups()
		if err := json.Unmarshal([]byte(raw), &report); err != nil || report.Rows != 1 || len(found) != 1 || report.BackupPath != found[0] {
			t.Fatalf("prepare %d: receipt %+v (%v), backups %v; want one backup holding the one row", open, report, err, found)
		}
	}
	backup, err := sql.Open("sqlite", statedb.URI(backups()[0], "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var boundary string
	if err := backup.QueryRow(`SELECT b.hosted_thread_id FROM transcript_boundaries b JOIN hosted_threads h USING(hosted_thread_id) JOIN queued_turns q USING(hosted_thread_id)`).Scan(&boundary); err != nil || boundary != "kept" {
		t.Fatalf("backup does not hold the boundary row with its thread and prompt: %q, %v", boundary, err)
	}
}

func createSchema20StoreWithSources(t *testing.T, path string, queue []turns.QueuedTurn, paths StorePaths) {
	t.Helper()
	ctx := context.Background()
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacySchema(db, testMigrations()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	sources, err := fingerprintSources(ctx, paths)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnRecordsWithSources(ctx, db, paths.Turns, filepath.Join(filepath.Dir(path), "schema20-import-backup"), sources); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := turns.SaveQueuedTurns(ctx, db, queue); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
