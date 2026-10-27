package diagnostics

import (
	"database/sql"
	"fmt"
)

// MigrateStateSchema owns the browser incident table while it remains lodged
// in the shared state database. The unused alert columns and index remain
// for existing stores; removing them would require a schema migration.
func MigrateStateSchema(tx interface {
	Exec(string, ...any) (sql.Result, error)
}, version int) error {
	if version > 17 {
		return nil
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS browser_duplicate_incidents (
				incident_fingerprint TEXT PRIMARY KEY,
				provider TEXT NOT NULL DEFAULT '',
				thread_id TEXT NOT NULL DEFAULT '',
				hosted_thread_id TEXT NOT NULL DEFAULT '',
				raw_render_key_fingerprint TEXT NOT NULL DEFAULT '',
				semantic_id_fingerprint TEXT NOT NULL DEFAULT '',
				raw_render_key_duplicate_count INTEGER NOT NULL CHECK(raw_render_key_duplicate_count >= 0),
				semantic_id_duplicate_count INTEGER NOT NULL CHECK(semantic_id_duplicate_count >= 0),
				first_build TEXT NOT NULL DEFAULT '',
				last_build TEXT NOT NULL DEFAULT '',
				first_pane_id TEXT NOT NULL,
				last_pane_id TEXT NOT NULL,
				first_observed_at TEXT NOT NULL,
				last_observed_at TEXT NOT NULL,
				observation_count INTEGER NOT NULL CHECK(observation_count > 0),
				alert_state TEXT NOT NULL CHECK(alert_state IN ('pending','sent')),
				alert_recipient_hosted_id TEXT NOT NULL DEFAULT '',
				alert_message_id TEXT,
				alert_attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(alert_attempt_count >= 0),
				last_alert_attempt_at TEXT,
				last_alert_error TEXT)`,
		`CREATE INDEX IF NOT EXISTS browser_duplicate_incidents_pending ON browser_duplicate_incidents(alert_state,first_observed_at)`,
		`CREATE INDEX IF NOT EXISTS browser_duplicate_incidents_thread ON browser_duplicate_incidents(thread_id,last_observed_at)`,
		`CREATE INDEX IF NOT EXISTS browser_duplicate_incidents_hosted_thread ON browser_duplicate_incidents(hosted_thread_id,last_observed_at)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create browser duplicate incident schema: %w", err)
		}
	}
	return nil
}

func CheckStateIntegrity(db *sql.DB) error {
	var invalid int
	err := db.QueryRow(`SELECT count(*) FROM browser_duplicate_incidents
		WHERE (raw_render_key_fingerprint='' AND semantic_id_fingerprint='')`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("control metadata integrity failure: %d browser duplicate incidents have invalid fingerprint", invalid)
	}
	return nil
}
