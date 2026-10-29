package schema

import (
	"database/sql"
)

// MigratePermissionEventSchema owns provider permission event evidence.
func MigratePermissionEventSchema(tx *sql.Tx, version int) error {
	if version > 10 {
		return nil
	}
	_, err := tx.Exec(permissionEventsTableSQL)
	return err
}

// MigrateNativeRecoverySchema owns the hosted native-recovery journal.
func MigrateNativeRecoverySchema(tx *sql.Tx) error {
	_, err := tx.Exec(nativeRecoverySchemaSQL)
	return err
}

const nativeRecoverySchemaSQL = `CREATE TABLE IF NOT EXISTS hosted_native_recovery_grants (
	 grant_id TEXT PRIMARY KEY, hosted_thread_id TEXT NOT NULL REFERENCES hosted_threads(hosted_thread_id),
	 old_native_id TEXT NOT NULL, client_recovery_id TEXT NOT NULL, operator_session_id TEXT NOT NULL,
	 expires_at TEXT NOT NULL, consumed_at TEXT NOT NULL DEFAULT '',
	 UNIQUE(hosted_thread_id,client_recovery_id));
	 CREATE TABLE IF NOT EXISTS hosted_native_recoveries (
	 hosted_thread_id TEXT NOT NULL REFERENCES hosted_threads(hosted_thread_id), client_recovery_id TEXT NOT NULL,
	 old_native_id TEXT NOT NULL, new_native_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
	 record_json TEXT NOT NULL, PRIMARY KEY(hosted_thread_id,client_recovery_id));
	 CREATE UNIQUE INDEX IF NOT EXISTS hosted_native_recovery_active ON hosted_native_recoveries(hosted_thread_id)
	 WHERE state IN ('reserved','creating','created','bound_pending_activation','creation_unknown');
	 CREATE TABLE IF NOT EXISTS hosted_native_recovery_events (
	 event_id INTEGER PRIMARY KEY AUTOINCREMENT, hosted_thread_id TEXT NOT NULL,
	 client_recovery_id TEXT NOT NULL, state TEXT NOT NULL, record_json TEXT NOT NULL);`

const permissionEventsTableSQL = `CREATE TABLE IF NOT EXISTS hosted_permission_events (
	event_id INTEGER PRIMARY KEY AUTOINCREMENT,
	hosted_thread_id TEXT NOT NULL,
	provider TEXT NOT NULL,
	native_thread_id TEXT,
	nano_session_id TEXT NOT NULL,
	attachment_generation INTEGER NOT NULL,
	provider_request_id TEXT NOT NULL,
	turn_id TEXT,
	item_id TEXT,
	method TEXT NOT NULL,
	interaction_kind TEXT NOT NULL,
	tool_name TEXT,
	event_kind TEXT NOT NULL CHECK(event_kind IN ('requested','resolved')),
	permission_mode TEXT,
	decision TEXT,
	decision_source TEXT,
	outcome TEXT,
	request_summary TEXT NOT NULL,
	request_sha256 TEXT NOT NULL,
	observed_at TEXT NOT NULL,
	source_event_key TEXT NOT NULL UNIQUE,
	FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS hosted_permission_events_thread_cursor
	ON hosted_permission_events(hosted_thread_id,event_id);`
