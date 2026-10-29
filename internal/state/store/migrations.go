package statestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

// LegacySchemaVersion is the last control.sqlite schema before the three turn
// families joined the threads store. Its existing migrations remain lossless
// under the same supported-version contract; schema 3 still requires its retired
// execution importer and is refused.
const LegacySchemaVersion = 19

// MigrateLegacySchema runs only after the caller has completed a backup. It
// preserves the existing metadata, mail, execution and recovery transactions.
// Feature tables stay in this database until their owning stages move them.
func MigrateLegacySchema(db *sql.DB, migrations statedb.MigrationRunner) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == prompts.TurnSchemaVersion {
		if err := CheckMetadataIntegrity(db, migrations); err != nil {
			return err
		}
		if _, err := prompts.MigrateTurnTransactions(context.Background(), db, ""); err != nil {
			return err
		}
		if err := migrations(context.Background(), statedb.MigrationCompletedUsage, db, nil, version, ""); err != nil {
			return err
		}
		if err := migrations(context.Background(), statedb.MigrationMemberRecovery, db, nil, version, ""); err != nil {
			return err
		}
		return migrations(context.Background(), statedb.MigrationParkedDiscard, db, nil, version, "")
	}
	if version == prompts.LegacyTurnSchemaVersion {
		if err := CheckMetadataIntegrity(db, migrations); err != nil {
			return err
		}
		_, err := prompts.MigrateTurnRecords(context.Background(), db, prompts.LegacyTurnPaths{}, "")
		return err
	}
	if version > LegacySchemaVersion {
		return fmt.Errorf("control metadata schema %d is newer than this Nano", version)
	}
	if version == LegacySchemaVersion {
		if err := migrations(context.Background(), statedb.MigrationSubscriptions, db, nil, version, ""); err != nil {
			return err
		}
		return CheckMetadataIntegrity(db, migrations)
	}
	if version == 3 {
		return errors.New("control metadata schema 3 cannot migrate without a lossless agent execution-state importer")
	}
	if version == 16 {
		// The v17 mail-table rebuild drops and recreates tables other tables
		// reference. SQLite cannot toggle foreign_keys inside a transaction, and
		// this store runs on one connection, so the switch is connection-wide
		// and restored on every exit path.
		if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer func() { _, _ = db.Exec(`PRAGMA foreign_keys = ON`) }()
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := migrations(context.Background(), statedb.MigrationTransactionStart, db, tx, version, ""); err != nil {
		return err
	}
	if version == 0 {
		statements := []string{
			`CREATE TABLE hosted_threads (
			hosted_thread_id TEXT PRIMARY KEY, provider TEXT NOT NULL,
			current_native_id TEXT, presentation_mode TEXT NOT NULL,
			sidebar_placement TEXT NOT NULL DEFAULT 'invoker',
			invoker_hosted_id TEXT, invoker_native_id TEXT, invoker_provider TEXT,
			notify_invoker_on_terminal INTEGER NOT NULL DEFAULT 0,
			terminal_notification_policy TEXT NOT NULL DEFAULT 'none',
			attachment_generation INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
			`CREATE TABLE hosted_thread_native_ids (
			hosted_thread_id TEXT NOT NULL, provider TEXT NOT NULL, native_thread_id TEXT NOT NULL,
			provisional INTEGER NOT NULL DEFAULT 0, retired_at TEXT, first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL,
			PRIMARY KEY(hosted_thread_id, provider, native_thread_id),
			FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE)`,
			`CREATE UNIQUE INDEX hosted_thread_active_native_id ON hosted_thread_native_ids(provider, native_thread_id) WHERE retired_at IS NULL`,
			`CREATE INDEX hosted_threads_notify_invoker ON hosted_threads(notify_invoker_on_terminal,invoker_hosted_id)`,
			`CREATE TABLE thread_metadata_events (
			event_id INTEGER PRIMARY KEY AUTOINCREMENT, hosted_thread_id TEXT NOT NULL, field TEXT NOT NULL,
			value_json TEXT, disposition TEXT NOT NULL, source TEXT NOT NULL, evidence TEXT,
			native_thread_id TEXT, nano_session_id TEXT, attachment_generation INTEGER,
			evidence_freshness TEXT NOT NULL, causation_event_id INTEGER, source_event_key TEXT,
			observed_at TEXT NOT NULL, committed_at TEXT NOT NULL,
			FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE,
			FOREIGN KEY(causation_event_id) REFERENCES thread_metadata_events(event_id))`,
			`CREATE UNIQUE INDEX metadata_source_event_key ON thread_metadata_events(source_event_key) WHERE source_event_key IS NOT NULL`,
			`CREATE TABLE thread_metadata_current (
			hosted_thread_id TEXT NOT NULL, field TEXT NOT NULL,
			desired_value_json TEXT, desired_disposition TEXT NOT NULL, desired_source TEXT, desired_event_id INTEGER,
			desired_settlement_event_id INTEGER,
			effective_value_json TEXT, effective_disposition TEXT NOT NULL, effective_source TEXT, effective_event_id INTEGER,
			thread_revision INTEGER NOT NULL, updated_at TEXT NOT NULL,
			PRIMARY KEY(hosted_thread_id, field),
			FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE,
			FOREIGN KEY(desired_event_id) REFERENCES thread_metadata_events(event_id) DEFERRABLE INITIALLY DEFERRED,
			FOREIGN KEY(desired_settlement_event_id) REFERENCES thread_metadata_events(event_id) DEFERRABLE INITIALLY DEFERRED,
			FOREIGN KEY(effective_event_id) REFERENCES thread_metadata_events(event_id) DEFERRABLE INITIALLY DEFERRED)`,
			`CREATE TABLE control_migrations (
			name TEXT PRIMARY KEY, source_fingerprint TEXT NOT NULL, completed_at TEXT NOT NULL)`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create control metadata schema: %w", err)
			}
		}
	}
	if version == 1 {
		if _, err := tx.Exec(`ALTER TABLE thread_metadata_current ADD COLUMN desired_settlement_event_id INTEGER`); err != nil {
			return fmt.Errorf("add desired settlement reference: %w", err)
		}
		if _, err := tx.Exec(`CREATE TABLE control_migrations (name TEXT PRIMARY KEY, source_fingerprint TEXT NOT NULL, completed_at TEXT NOT NULL)`); err != nil {
			return fmt.Errorf("create migration checkpoint table: %w", err)
		}
	}
	if version <= 3 {
		for _, statement := range []string{
			`CREATE TABLE hosted_execution_sessions (
				hosted_thread_id TEXT PRIMARY KEY, client_session_id TEXT UNIQUE, request_hash TEXT NOT NULL,
				nano_session_id TEXT, status TEXT NOT NULL, error TEXT, run_id TEXT NOT NULL,
				created_at TEXT NOT NULL, updated_at TEXT NOT NULL, revision INTEGER NOT NULL,
				FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE)`,
			`CREATE TABLE hosted_turn_operations (
				operation_id TEXT PRIMARY KEY, hosted_thread_id TEXT NOT NULL, client_action_id TEXT NOT NULL,
				transport_action_id TEXT NOT NULL, prompt_hash TEXT NOT NULL, provider_turn_id TEXT, status TEXT NOT NULL,
				error_source TEXT, error_code TEXT, error_message TEXT, error_details_json TEXT,
				final_item_id TEXT, final_response TEXT, final_response_truncated INTEGER NOT NULL DEFAULT 0, final_response_bytes INTEGER NOT NULL DEFAULT 0,
				candidate_item_id TEXT, candidate_phase TEXT, candidate_response TEXT, candidate_bytes INTEGER NOT NULL DEFAULT 0, candidate_truncated INTEGER NOT NULL DEFAULT 0,
				wake_batch_id TEXT, invoker_acked_at TEXT,
				assignment_event_id INTEGER, run_id TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, revision INTEGER NOT NULL,
				UNIQUE(hosted_thread_id, client_action_id),
				FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE,
				FOREIGN KEY(assignment_event_id) REFERENCES thread_metadata_events(event_id))`,
			`CREATE INDEX hosted_turn_operations_notification_lookup ON hosted_turn_operations(hosted_thread_id,client_action_id,status)`,
			`CREATE INDEX hosted_turn_operations_wake_pending ON hosted_turn_operations(status,wake_batch_id,invoker_acked_at,created_at)`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create normalized execution schema: %w", err)
			}
		}
	}
	if version <= 4 {
		if _, err := tx.Exec(`CREATE TABLE hosted_attachment_generations (hosted_thread_id TEXT NOT NULL, generation INTEGER NOT NULL, bound_at TEXT NOT NULL, PRIMARY KEY(hosted_thread_id,generation), FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE)`); err != nil {
			return fmt.Errorf("create attachment generation history: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO hosted_attachment_generations(hosted_thread_id,generation,bound_at) SELECT hosted_thread_id,attachment_generation,updated_at FROM hosted_threads WHERE attachment_generation > 0`); err != nil {
			return err
		}
	}
	if version >= 4 && version <= 5 {
		if _, err := tx.Exec(`ALTER TABLE hosted_turn_operations ADD COLUMN assignment_event_id INTEGER`); err != nil {
			return fmt.Errorf("add operation assignment event reference: %w", err)
		}
	}
	if version == 6 {
		if _, err := tx.Exec(`ALTER TABLE hosted_threads ADD COLUMN notify_invoker_on_terminal INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add invoker terminal notification flag: %w", err)
		}
		for _, statement := range []string{
			`CREATE INDEX hosted_threads_notify_invoker ON hosted_threads(notify_invoker_on_terminal,invoker_hosted_id)`,
			`CREATE INDEX hosted_turn_operations_notification_lookup ON hosted_turn_operations(hosted_thread_id,client_action_id,status)`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("create notification lookup index: %w", err)
			}
		}
	}
	if version == 7 {
		if _, err := tx.Exec(`ALTER TABLE hosted_threads ADD COLUMN terminal_notification_policy TEXT NOT NULL DEFAULT 'none'`); err != nil {
			return fmt.Errorf("add terminal notification policy: %w", err)
		}
		if _, err := tx.Exec(`UPDATE hosted_threads SET terminal_notification_policy=CASE WHEN notify_invoker_on_terminal=1 THEN 'individual' ELSE 'none' END`); err != nil {
			return fmt.Errorf("migrate terminal notification policy: %w", err)
		}
		if _, err := tx.Exec(`ALTER TABLE hosted_turn_operations ADD COLUMN wake_batch_id TEXT`); err != nil {
			return fmt.Errorf("add notification wake batch: %w", err)
		}
		if _, err := tx.Exec(`ALTER TABLE hosted_turn_operations ADD COLUMN invoker_acked_at TEXT`); err != nil {
			return fmt.Errorf("add notification acknowledgement: %w", err)
		}
		if _, err := tx.Exec(`CREATE INDEX hosted_turn_operations_wake_pending ON hosted_turn_operations(status,wake_batch_id,invoker_acked_at,created_at)`); err != nil {
			return fmt.Errorf("create coalesced notification index: %w", err)
		}
	}
	if err := migrations(context.Background(), statedb.MigrationPermissionEvents, db, tx, version, ""); err != nil {
		return err
	}
	if version <= 11 {
		for _, col := range []struct{ table, column string }{
			{"hosted_threads", "last_human_prompt_at"},
			{"hosted_turn_operations", "origin"},
			{"hosted_turn_operations", "sender_hosted_id"},
			{"hosted_turn_operations", "sender_native_id"},
			{"hosted_turn_operations", "sender_provider"},
			{"hosted_turn_operations", "sender_name"},
		} {
			if err := addTextColumnIfMissing(tx, col.table, col.column); err != nil {
				return fmt.Errorf("add prompt origin/sender attribution: %w", err)
			}
		}
	}
	if version <= 14 {
		if err := addTextColumnIfMissing(tx, "hosted_turn_operations", "sender_submitted_at"); err != nil {
			return fmt.Errorf("add agent sender submission timestamp: %w", err)
		}
	}
	if err := migrations(context.Background(), statedb.MigrationMail, db, tx, version, ""); err != nil {
		return err
	}
	if version == 12 {
		if _, err := tx.Exec(`DELETE FROM thread_metadata_current WHERE field='instructionFileBoundary'`); err != nil {
			return fmt.Errorf("remove retired instruction boundary projection: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM thread_metadata_events WHERE field='instructionFileBoundary'`); err != nil {
			return fmt.Errorf("remove retired instruction boundary events: %w", err)
		}
	}
	if version <= 13 {
		if err := addColumnIfMissing(tx, "hosted_threads", "sidebar_placement", "TEXT NOT NULL DEFAULT 'invoker'"); err != nil {
			return fmt.Errorf("add sidebar placement: %w", err)
		}
		if _, err := tx.Exec(`UPDATE hosted_threads SET sidebar_placement='invoker' WHERE sidebar_placement IS NULL OR sidebar_placement=''`); err != nil {
			return fmt.Errorf("initialize sidebar placement: %w", err)
		}
	}
	if err := migrations(context.Background(), statedb.MigrationNativeRecovery, db, tx, version, ""); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", LegacySchemaVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return CheckMetadataIntegrity(db, migrations)
}

func migrateCompletedTurnUsage(ctx context.Context, db *sql.DB, backupPath string, migrations statedb.MigrationRunner) error {
	return migrations(ctx, statedb.MigrationCompletedUsage, db, nil, prompts.UsageTurnSchemaVersion, backupPath)
}

// migrateMemberRecovery takes a schema-22 private candidate to schema 23, or
// with an empty backup path only verifies a store at schema 23 or later.
func migrateMemberRecovery(ctx context.Context, db *sql.DB, backupPath string, migrations statedb.MigrationRunner) error {
	return migrations(ctx, statedb.MigrationMemberRecovery, db, nil, prompts.MemberRecoveryTurnSchemaVersion, backupPath)
}

// migrateParkedDiscard takes a schema-23 private candidate to schema 24, or
// with an empty backup path only verifies a schema-24 store.
func migrateParkedDiscard(ctx context.Context, db *sql.DB, backupPath string, migrations statedb.MigrationRunner) error {
	return migrations(ctx, statedb.MigrationParkedDiscard, db, nil, prompts.TurnSchemaVersion, backupPath)
}

func CheckMetadataIntegrity(db *sql.DB, migrations statedb.MigrationRunner) error {
	return migrations(context.Background(), statedb.MigrationIntegrity, db, nil, 0, "")
}

func addTextColumnIfMissing(tx *sql.Tx, table, column string) error {
	return addColumnIfMissing(tx, table, column, "TEXT")
}

func addColumnIfMissing(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}
