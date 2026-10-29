package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

func turnMigrationFixture(t *testing.T) (*hostedstore.MetadataStore, prompts.LegacyTurnPaths, []prompts.QueuedTurn, []prompts.SendAttempt) {
	t.Helper()
	dir := t.TempDir()
	control, err := openControlMetadataStore(filepath.Join(dir, "control-copy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	for _, id := range []string{"current", "retired", "provisional"} {
		if err := control.EnsureHostedThread(id, "claude", presentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := control.ClaimNativeID("retired", "claude", "native", false); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CanonicalDatabase().Exec(`UPDATE hosted_thread_native_ids SET retired_at='before' WHERE hosted_thread_id='retired'`); err != nil {
		t.Fatal(err)
	}
	if err := control.ClaimNativeID("current", "claude", "native", false); err != nil {
		t.Fatal(err)
	}
	if err := control.ClaimNativeID("provisional", "claude", "cs_provisional", true); err != nil {
		t.Fatal(err)
	}
	paths := prompts.LegacyTurnPaths{Queue: filepath.Join(dir, "queued-prompts.json"), Attempts: filepath.Join(dir, "send-attempts.json"), Completed: filepath.Join(dir, "turn-completed.sqlite")}
	queue := []prompts.QueuedTurn{
		{ClientActionID: "q2", Provider: "claude", HostedThreadID: "current", NativeThreadID: "native", Text: "same text", PresentationText: "shown text", TextHash: "hash", RequestHash: "request", State: "uncertain", Origin: "agent", CreatedAt: "later", ProviderTurnID: "turn-2", ProviderTerminalStatus: "interrupted", ProviderTerminalAt: "terminal"},
		{ClientActionID: "q1", Provider: "claude", HostedThreadID: "provisional", NativeThreadID: "cs_provisional", Text: "same text", TextHash: "hash", State: "parked", CreatedAt: "earlier"},
	}
	attempts := []prompts.SendAttempt{
		{ClientActionID: "q2", Provider: "claude", NativeThreadID: "native", TextHash: "hash", Status: "timeout", RunID: "old", Reason: "uncertain", CreatedAt: "created", UpdatedAt: "updated", ProviderTurnID: "turn-2", ProviderTerminalStatus: "interrupted", ProviderTerminalAt: "terminal", IdentityConflict: "action_bound_to_multiple_provider_turns"},
		{ClientActionID: "confirmed", Provider: "codex", NativeThreadID: "codex-native", TextHash: "hash", Status: "accepted", RunID: "old", UpdatedAt: "updated", Confirmed: true, ConfirmedAt: "confirmed", CanonicalMessageID: "canonical", ProviderTurnID: "turn-1"},
	}
	writeMigrationJSON(t, paths.Queue, queue)
	writeMigrationJSON(t, paths.Attempts, attempts)
	completed, err := openCompletedTurnStore(paths.Completed)
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Close()
	if _, err := completed.db.Exec(`INSERT INTO completed_turns(id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at) VALUES(7,'current','claude','turn-0','final-0','at','projection','created'),(42,'retired','claude','turn-old','final-old','at','projection','created'); UPDATE sqlite_sequence SET seq=99 WHERE name='completed_turns'; CREATE TABLE observer_fixture(id TEXT PRIMARY KEY, bookmark TEXT); INSERT INTO observer_fixture VALUES('observer','turn-0')`); err != nil {
		t.Fatal(err)
	}
	return control, paths, queue, attempts
}

func writeMigrationJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestThreadsTurnMigrationPreservesExactFactsAndRerunsWithoutImport(t *testing.T) {
	control, paths, queue, attempts := turnMigrationFixture(t)
	before := threadsDatabaseSnapshot(t, control.CanonicalDatabase())
	completed, err := sql.Open("sqlite", paths.Completed)
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Close()
	oldCompleted := threadsDatabaseSnapshot(t, completed)
	queueBytes, _ := os.ReadFile(paths.Queue)
	attemptBytes, _ := os.ReadFile(paths.Attempts)
	backup := filepath.Join(filepath.Dir(paths.Queue), "backup")
	report, err := prompts.MigrateTurnRecords(context.Background(), control.CanonicalDatabase(), paths, backup)
	if err != nil {
		t.Fatal(err)
	}
	if report.Queued != 2 || report.Attempts != 2 || report.Completed != 2 || report.CompletedSequence != 99 || report.BackupDir != backup {
		t.Fatalf("wrong reconciliation: %+v", report)
	}
	gotQueue, err := prompts.LoadQueuedTurns(context.Background(), control.CanonicalDatabase())
	if err != nil || !reflect.DeepEqual(gotQueue, queue) {
		t.Fatalf("queue order/fields changed: %+v, %v", gotQueue, err)
	}
	gotAttempts, err := prompts.LoadSendAttempts(context.Background(), control.CanonicalDatabase())
	if err != nil || !reflect.DeepEqual(gotAttempts, attempts) {
		t.Fatalf("attempt fields changed: %+v, %v", gotAttempts, err)
	}
	after := threadsDatabaseSnapshot(t, control.CanonicalDatabase())
	for table, rows := range before {
		if table == "schema" || table == "version" || table == "sqlite_sequence" {
			continue
		}
		if !reflect.DeepEqual(after[table], rows) {
			t.Fatalf("existing table changed: %s", table)
		}
	}
	if !reflect.DeepEqual(after["completed_turns"], oldCompleted["completed_turns"]) {
		t.Fatal("completion rows or ids changed")
	}
	if _, exists := after["observer_fixture"]; exists {
		t.Fatal("observer state crossed the migration boundary")
	}
	if !reflect.DeepEqual(threadsDatabaseSnapshot(t, completed), oldCompleted) {
		t.Fatal("completion source changed")
	}
	for path, expected := range map[string][]byte{paths.Queue: queueBytes, paths.Attempts: attemptBytes} {
		actual, err := os.ReadFile(path)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("source changed: %s", path)
		}
		actual, err = os.ReadFile(filepath.Join(backup, filepath.Base(path)))
		if err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("backup changed source bytes: %s", path)
		}
	}
	backupDB, err := sql.Open("sqlite", filepath.Join(backup, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(threadsDatabaseSnapshot(t, backupDB), before) {
		t.Fatal("backup is not the pre-migration database")
	}
	backupDB.Close()
	for _, name := range []string{"control.sqlite", "queued-prompts.json", "send-attempts.json", "turn-completed.sqlite", "manifest.json"} {
		info, err := os.Stat(filepath.Join(backup, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("non-private/missing backup %s: %v", name, err)
		}
	}
	// Stale legacy files must never resurrect confirmed/removed records after cutover.
	if err := os.WriteFile(paths.Queue, []byte("corrupt after successful cutover"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := prompts.MigrateTurnRecords(context.Background(), control.CanonicalDatabase(), paths, filepath.Join(backup, "must-not-be-created"))
	if err != nil || second != report {
		t.Fatalf("rerun: %+v %v", second, err)
	}
	if !reflect.DeepEqual(threadsDatabaseSnapshot(t, control.CanonicalDatabase()), after) {
		t.Fatal("rerun mutated canonical data")
	}
	var nextID int64
	if err := control.CanonicalDatabase().QueryRow(`INSERT INTO completed_turns(target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at) VALUES('current','claude','next','final-next','at','test','at') RETURNING id`).Scan(&nextID); err != nil || nextID != 100 {
		t.Fatalf("completion cursor reused: %d %v", nextID, err)
	}
}

func TestThreadsTurnMigrationFailsWithoutPartialMutation(t *testing.T) {
	for _, failure := range []string{"corrupt-json", "duplicate-action", "unknown-field", "empty-json", "backup-failure", "future-schema", "cancelled", "sql-conflict", "unmarked-current-schema", "corrupt-completions", "duplicate-completions", "future-completion-field", "future-completion-version"} {
		t.Run(failure, func(t *testing.T) {
			control, paths, queue, _ := turnMigrationFixture(t)
			backup := filepath.Join(filepath.Dir(paths.Queue), "backup")
			ctx := context.Background()
			switch failure {
			case "corrupt-json":
				os.WriteFile(paths.Queue, []byte("["), 0600)
			case "duplicate-action":
				writeMigrationJSON(t, paths.Queue, append(queue, queue[0]))
			case "unknown-field":
				os.WriteFile(paths.Attempts, []byte(`[{"clientActionId":"a","futureEvidence":"must not disappear"}]`), 0600)
			case "empty-json":
				os.WriteFile(paths.Queue, nil, 0600)
			case "backup-failure":
				os.WriteFile(backup, []byte("occupied"), 0600)
			case "future-schema":
				control.CanonicalDatabase().Exec(`PRAGMA user_version=999`)
			case "unmarked-current-schema":
				control.CanonicalDatabase().Exec(`PRAGMA user_version=20`)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "sql-conflict":
				control.CanonicalDatabase().Exec(`CREATE TABLE completed_turns(id INTEGER); INSERT INTO completed_turns VALUES(123)`)
			case "duplicate-completions", "future-completion-field", "future-completion-version":
				completed, err := sql.Open("sqlite", paths.Completed)
				if err != nil {
					t.Fatal(err)
				}
				statement := `PRAGMA user_version=1`
				if failure == "future-completion-field" {
					statement = `ALTER TABLE completed_turns ADD COLUMN future_evidence TEXT`
				}
				if failure == "duplicate-completions" {
					statement = `ALTER TABLE completed_turns RENAME TO old_completed; CREATE TABLE completed_turns AS SELECT * FROM old_completed; INSERT INTO completed_turns SELECT * FROM old_completed; UPDATE sqlite_sequence SET name='completed_turns' WHERE name='old_completed'`
				}
				if _, err := completed.Exec(statement); err != nil {
					t.Fatal(err)
				}
				completed.Close()
			case "corrupt-completions":
				os.WriteFile(paths.Completed, []byte("corrupt"), 0600)
			}
			before := threadsDatabaseSnapshot(t, control.CanonicalDatabase())
			if _, err := prompts.MigrateTurnRecords(ctx, control.CanonicalDatabase(), paths, backup); err == nil {
				t.Fatal("unsafe migration succeeded")
			}
			if !reflect.DeepEqual(threadsDatabaseSnapshot(t, control.CanonicalDatabase()), before) {
				t.Fatal("failed migration changed schema or records")
			}
		})
	}
}

func TestThreadsStorePublicationKeepsOriginalUntilAllRecordsReconcile(t *testing.T) {
	control, legacy, queue, attempts := turnMigrationFixture(t)
	ctx := context.Background()
	dir := filepath.Dir(legacy.Queue)
	paths := state.StorePaths{Destination: filepath.Join(dir, "nano_threads.sqlite"), Control: filepath.Join(dir, "control-copy.sqlite"), BackupRoot: filepath.Join(dir, "migration-backups"), Turns: legacy}
	before := threadsDatabaseSnapshot(t, control.CanonicalDatabase())
	if err := os.WriteFile(legacy.Queue, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.PrepareStore(ctx, paths, statewiring.Migrations()); err == nil {
		t.Fatal("partial migration published")
	}
	if _, err := os.Lstat(paths.Destination); !os.IsNotExist(err) {
		t.Fatalf("failed candidate became canonical: %v", err)
	}
	if !reflect.DeepEqual(threadsDatabaseSnapshot(t, control.CanonicalDatabase()), before) {
		t.Fatal("failed candidate changed source")
	}
	writeMigrationJSON(t, legacy.Queue, queue)
	report, err := state.PrepareStore(ctx, paths, statewiring.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if report.Queued != len(queue) || report.Attempts != len(attempts) || report.Completed != 2 {
		t.Fatalf("wrong report: %+v", report)
	}
	if !reflect.DeepEqual(threadsDatabaseSnapshot(t, control.CanonicalDatabase()), before) {
		t.Fatal("publication rewrote old control store")
	}
	canonical, err := statedb.Open(ctx, paths.Destination)
	if err != nil {
		t.Fatal(err)
	}
	defer canonical.Close()
	if got, err := prompts.LoadQueuedTurns(ctx, canonical); err != nil || !reflect.DeepEqual(got, queue) {
		t.Fatalf("published queue: %+v %v", got, err)
	}
	if got, err := prompts.LoadSendAttempts(ctx, canonical); err != nil || !reflect.DeepEqual(got, attempts) {
		t.Fatalf("published journal: %+v %v", got, err)
	}
	// Consumption after cutover cannot be undone by a later startup.
	if err := prompts.SaveQueuedTurns(ctx, canonical, queue[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := state.PrepareStore(ctx, paths, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	if got, err := prompts.LoadQueuedTurns(ctx, canonical); err != nil || !reflect.DeepEqual(got, queue[:1]) {
		t.Fatalf("old text resurrected: %+v %v", got, err)
	}
	info, err := os.Stat(paths.Destination)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("canonical mode: %v %v", info, err)
	}
	// Metadata's application wrapper must accept the migrated schema unchanged.
	view, err := openControlMetadataStore(paths.Destination)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if got, err := view.View("current"); err != nil || got.NativeThreadID != "native" {
		t.Fatalf("migrated identity unavailable: %+v %v", got, err)
	}
}

func TestThreadsStorePublicationHandlesFreshAndRejectsAmbiguousDestinations(t *testing.T) {
	for _, mode := range []string{"fresh", "future", "empty-control", "stranded-journal", "existing-empty", "existing-unmigrated"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			paths := state.StorePaths{Destination: filepath.Join(dir, "nano_threads.sqlite"), Control: filepath.Join(dir, "control.sqlite"), BackupRoot: filepath.Join(dir, "backups")}
			switch mode {
			case "empty-control":
				os.WriteFile(paths.Control, nil, 0600)
			case "stranded-journal":
				os.WriteFile(paths.Destination+"-wal", []byte("stranded"), 0600)
			case "existing-empty":
				os.WriteFile(paths.Destination, nil, 0600)
			case "future", "existing-unmigrated":
				db, err := sql.Open("sqlite", paths.Destination)
				if err != nil {
					t.Fatal(err)
				}
				version := 19
				if mode == "future" {
					version = 999
				}
				if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			report, err := state.PrepareStore(context.Background(), paths, statewiring.Migrations())
			if mode != "fresh" {
				if err == nil {
					t.Fatal("ambiguous destination accepted")
				}
				return
			}
			if err != nil || report.Queued != 0 || report.Attempts != 0 || report.Completed != 0 {
				t.Fatalf("fresh store: %+v %v", report, err)
			}
		})
	}
}
