package subscriptions

import (
	"database/sql"
	"fmt"
)

// MigrateStateSchema keeps the legacy subscription cursor in the shared state
// database until a later migration gives the feature its own store.
func MigrateStateSchema(tx interface {
	Exec(string, ...any) (sql.Result, error)
}) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS subscription_completed_cursors (
		hosted_thread_id TEXT PRIMARY KEY,
		cursor TEXT NOT NULL DEFAULT '',
		initialized_at TEXT NOT NULL,
		updated_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create subscription cursor schema: %w", err)
	}
	return nil
}

// StateCursorCount returns the retained legacy cursor count for the shared
// migration receipt.
func StateCursorCount(db interface {
	QueryRow(string, ...any) *sql.Row
}) (int, error) {
	var count int
	err := db.QueryRow(`SELECT count(*) FROM subscription_completed_cursors`).Scan(&count)
	return count, err
}
