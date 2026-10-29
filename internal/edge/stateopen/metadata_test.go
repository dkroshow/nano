package stateopen

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/edge/statewiring"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	state "github.com/dkroshow/nano/internal/state/store"
)

func TestControlMetadataReadPoolIsBoundedAndReadOnly(t *testing.T) {
	store := testMetadata(t)
	if store.ReadPool() == nil {
		t.Fatal("canonical metadata read pool is unavailable")
	}
	if got := store.ReadPool().Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("canonical metadata read connections = %d, want 4", got)
	}
	var foreignKeys, busyTimeout int
	if err := store.ReadPool().QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.ReadPool().QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 {
		t.Fatalf("read pool pragmas = foreign_keys:%d busy_timeout:%d", foreignKeys, busyTimeout)
	}
	if _, err := store.ReadPool().Exec(`CREATE TABLE must_stay_read_only(id INTEGER)`); err == nil {
		t.Fatal("canonical metadata read pool accepted a write")
	}
}

func TestControlMetadataMigratesInvokerNotificationFlagFromVersionSix(t *testing.T) {
	store := testMetadata(t)
	if _, err := store.CanonicalDatabase().Exec(`DROP INDEX hosted_threads_notify_invoker`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`DROP INDEX hosted_turn_operations_notification_lookup`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`ALTER TABLE hosted_threads RENAME TO hosted_threads_v6`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`CREATE TABLE hosted_threads (
		hosted_thread_id TEXT PRIMARY KEY, provider TEXT NOT NULL,
		current_native_id TEXT, presentation_mode TEXT NOT NULL,
		invoker_hosted_id TEXT, invoker_native_id TEXT, invoker_provider TEXT,
		attachment_generation INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`DROP TABLE hosted_threads_v6`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := state.MigrateLegacySchema(store.CanonicalDatabase(), statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.CanonicalDatabase().Query(`PRAGMA table_info(hosted_threads)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "notify_invoker_on_terminal" {
			found = notNull == 1 && defaultValue.Valid && defaultValue.String == "0"
		}
	}
	if !found {
		t.Fatal("migration did not add NOT NULL DEFAULT 0 notify_invoker_on_terminal")
	}
}

func TestControlMetadataMigrationAddsBrowserDuplicateIncidentsFromVersion17(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`DROP TABLE browser_duplicate_incidents`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`PRAGMA user_version = 17`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = openMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version, tables int
	if err := store.CanonicalDatabase().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := store.CanonicalDatabase().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='browser_duplicate_incidents'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != state.LegacySchemaVersion || tables != 1 {
		t.Fatalf("migration version=%d tables=%d, want %d and 1", version, tables, state.LegacySchemaVersion)
	}
}

func TestControlMetadataRefusesVersionThreeWithoutDroppingExecutionState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`CREATE TABLE agent_execution_state (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`INSERT INTO agent_execution_state(value) VALUES ('must survive')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if reopened, err := openMetadata(path); err == nil {
		reopened.Close()
		t.Fatal("version 3 migration succeeded without a lossless execution-state importer")
	} else if !strings.Contains(err.Error(), "schema 3") {
		t.Fatalf("version 3 migration error = %v, want schema 3 refusal", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM agent_execution_state`).Scan(&value); err != nil {
		t.Fatalf("legacy execution state was dropped: %v", err)
	}
	if value != "must survive" {
		t.Fatalf("legacy execution state = %q", value)
	}
}

func TestControlSchemaVersionSevenMigratesNotificationState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`DROP INDEX hosted_turn_operations_wake_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`ALTER TABLE hosted_turn_operations DROP COLUMN wake_batch_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`ALTER TABLE hosted_turn_operations DROP COLUMN invoker_acked_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`ALTER TABLE hosted_threads DROP COLUMN terminal_notification_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CanonicalDatabase().Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, check := range []struct{ table, column string }{{"hosted_threads", "terminal_notification_policy"}, {"hosted_turn_operations", "wake_batch_id"}, {"hosted_turn_operations", "invoker_acked_at"}} {
		rows, err := store.CanonicalDatabase().Query("PRAGMA table_info(" + check.table + ")")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var cid int
			var name, kind string
			var notnull, pk int
			var dflt any
			if err := rows.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
				t.Fatal(err)
			}
			found = found || name == check.column
		}
		rows.Close()
		if !found {
			t.Fatalf("migration missing %s.%s", check.table, check.column)
		}
	}
}

func testMetadata(t *testing.T) *hostedstore.MetadataStore {
	t.Helper()
	store, err := openMetadata(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
