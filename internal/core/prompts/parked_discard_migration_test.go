package prompts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Schema 24 lets a parked prompt expire, so Discard can end it, and changes
// nothing else: every parked prompt stays parked until someone discards one,
// a parked prompt can do nothing but expire, confirmed and refused prompts stay
// final, the schema-23 source is backed up first, and a second run only
// verifies its receipt.
func TestParkedDiscardMigrationLetsOnlyAParkedPromptExpire(t *testing.T) {
	ctx := context.Background()
	db, path := schema20TurnFixture(t)
	dir := filepath.Dir(path)
	if _, err := MigrateTurnTransactions(ctx, db, filepath.Join(dir, "v20.sqlite")); err != nil {
		t.Fatal(err)
	}
	// The control tables share this database, and the usage stage (core/turns,
	// which this package must not import) takes the store to 22; its columns
	// stand in for it here.
	if _, err := db.Exec(`CREATE TABLE hosted_thread_native_ids(hosted_thread_id TEXT NOT NULL, provider TEXT NOT NULL, native_thread_id TEXT NOT NULL, PRIMARY KEY(hosted_thread_id, provider, native_thread_id));
		CREATE TABLE hosted_turn_operations(operation_id TEXT PRIMARY KEY, hosted_thread_id TEXT NOT NULL, transport_action_id TEXT NOT NULL);
		ALTER TABLE completed_turns ADD COLUMN usage_input_tokens INTEGER; ALTER TABLE completed_turns ADD COLUMN usage_cached_input_tokens INTEGER; ALTER TABLE completed_turns ADD COLUMN usage_output_tokens INTEGER; PRAGMA user_version=22`); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateMemberRecovery(ctx, db, filepath.Join(dir, "v22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE queued_turns SET state='parked' WHERE client_action_id='agent';
		UPDATE queued_turns SET state='refused' WHERE client_action_id='legacy-orphan'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE queued_turns SET state='expired' WHERE client_action_id='agent'`); err == nil {
		t.Fatal("schema 23 let a parked prompt change")
	}

	if _, err := MigrateParkedDiscard(ctx, db, ""); err == nil {
		t.Fatal("migrated without a backup")
	}
	backup := filepath.Join(dir, "v23.sqlite")
	report, err := MigrateParkedDiscard(ctx, db, backup)
	if err != nil {
		t.Fatal(err)
	}
	if report.Queued != 2 || report.Parked != 1 || report.BackupPath != backup {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("schema-23 backup: %v", err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM queued_turns WHERE client_action_id='agent'`).Scan(&state); err != nil || state != "parked" {
		t.Fatalf("parked prompt after migration = %q, %v; want still parked", state, err)
	}
	for _, change := range []string{
		`UPDATE queued_turns SET state='accepted' WHERE client_action_id='agent'`,
		`UPDATE queued_turns SET state='expired' WHERE client_action_id='legacy-orphan'`,
	} {
		if _, err := db.Exec(change); err == nil {
			t.Fatalf("schema 24 allowed %s", change)
		}
	}
	store, err := OpenStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if record, expired, err := store.Expire(ctx, "agent", time.Now()); err != nil || !expired || record.Outbox.State != Expired || record.ProviderText != "retain agent text" {
		t.Fatalf("expire parked = %s %q, %v, %v; want expired with its text kept", record.Outbox.State, record.ProviderText, expired, err)
	}

	again, err := MigrateParkedDiscard(ctx, db, "")
	if err != nil || again != report {
		t.Fatalf("verification = %+v, %v; want the receipt %+v", again, err, report)
	}
}
