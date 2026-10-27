package prompts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Schema 23 adds the owed and resolved facts with nothing filled in: an old
// Confirmed prompt never gains an obligation, rows survive, the schema-22
// source is backed up first, and a second run only verifies its receipt.
func TestMemberRecoveryMigrationAddsEmptyFactsOnlyFromSchema22(t *testing.T) {
	ctx := context.Background()
	db, path := schema20TurnFixture(t)
	dir := filepath.Dir(path)
	if _, err := MigrateTurnTransactions(ctx, db, filepath.Join(dir, "v20.sqlite")); err != nil {
		t.Fatal(err)
	}
	// The control tables share this database; the migration extends two.
	if _, err := db.Exec(`CREATE TABLE hosted_thread_native_ids(hosted_thread_id TEXT NOT NULL, provider TEXT NOT NULL, native_thread_id TEXT NOT NULL, PRIMARY KEY(hosted_thread_id, provider, native_thread_id));
		INSERT INTO hosted_thread_native_ids VALUES('hosted','claude','cs_handle');
		CREATE TABLE hosted_turn_operations(operation_id TEXT PRIMARY KEY, hosted_thread_id TEXT NOT NULL, transport_action_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateMemberRecovery(ctx, db, filepath.Join(dir, "too-early.sqlite")); err == nil {
		t.Fatal("schema 21 migrated past the usage schema")
	}
	// The usage stage (core/turns, which this package must not import) takes
	// the store to 22; its columns stand in for it here.
	if _, err := db.Exec(`ALTER TABLE completed_turns ADD COLUMN usage_input_tokens INTEGER; ALTER TABLE completed_turns ADD COLUMN usage_cached_input_tokens INTEGER; ALTER TABLE completed_turns ADD COLUMN usage_output_tokens INTEGER; PRAGMA user_version=22`); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateMemberRecovery(ctx, db, ""); err == nil {
		t.Fatal("migrated without a backup")
	}
	backup := filepath.Join(dir, "v22.sqlite")
	report, err := MigrateMemberRecovery(ctx, db, backup)
	if err != nil {
		t.Fatal(err)
	}
	if report.Queued != 2 || report.NativeIDs != 1 || report.BackupPath != backup {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("schema-22 backup: %v", err)
	}
	var version, owed, resolved int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != MemberRecoveryTurnSchemaVersion {
		t.Fatalf("version = %d, %v", version, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM queued_turns WHERE member_owed<>0`).Scan(&owed); err != nil || owed != 0 {
		t.Fatalf("owed rows = %d, %v", owed, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM hosted_thread_native_ids WHERE resolved_native_id IS NOT NULL`).Scan(&resolved); err != nil || resolved != 0 {
		t.Fatalf("resolved rows = %d, %v", resolved, err)
	}
	again, err := MigrateMemberRecovery(ctx, db, "")
	if err != nil || again != report {
		t.Fatalf("verification = %+v, %v; want the receipt %+v", again, err, report)
	}
}
