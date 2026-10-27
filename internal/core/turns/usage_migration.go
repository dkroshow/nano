package turns

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dkroshow/nano/internal/threads"
)

const completedUsageMigrationName = "completed-turn-usage-v22"

type CompletedUsageMigrationReport struct {
	BackupPath          string `json:"backupPath"`
	Completed           int    `json:"completed"`
	SubscriptionCursors int    `json:"subscriptionCursors"`
}

// MigrateCompletedTurnUsage adds the three provider counters exposed by the
// completed-turn feed. Its caller migrates a private candidate, never the live
// file, and supplies that candidate's pre-migration backup path.
func MigrateCompletedTurnUsage(ctx context.Context, db *sql.DB, backupPath string) (CompletedUsageMigrationReport, error) {
	return MigrateCompletedTurnUsageWithCursors(ctx, db, backupPath, 0)
}

// MigrateCompletedTurnUsageWithCursors records the feature-owned cursor count
// without reading or creating the subscription table itself.
func MigrateCompletedTurnUsageWithCursors(ctx context.Context, db *sql.DB, backupPath string, subscriptionCursors int) (CompletedUsageMigrationReport, error) {
	var report CompletedUsageMigrationReport
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	// Usage arrived with schema 22; every later supported schema keeps its
	// receipt.
	if version >= UsageTurnSchemaVersion && version <= TurnSchemaVersion {
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, completedUsageMigrationName).Scan(&raw); err != nil {
			return report, fmt.Errorf("threads: missing completed usage migration receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return report, err
		}
		return report, verifyCompletedUsageSchema(ctx, db)
	}
	if version != TransactionalTurnSchemaVersion {
		return report, fmt.Errorf("threads: completed usage migration requires schema %d, found %d", TransactionalTurnSchemaVersion, version)
	}
	if backupPath == "" {
		return report, errors.New("threads: completed usage migration requires a new backup path")
	}
	if err := threads.Backup(ctx, db, backupPath); err != nil {
		return report, fmt.Errorf("threads: back up completed usage source: %w", err)
	}
	report.BackupPath = backupPath
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completed_turns`).Scan(&report.Completed); err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`ALTER TABLE completed_turns ADD COLUMN usage_input_tokens INTEGER CHECK(usage_input_tokens >= 0)`,
		`ALTER TABLE completed_turns ADD COLUMN usage_cached_input_tokens INTEGER CHECK(usage_cached_input_tokens >= 0)`,
		`ALTER TABLE completed_turns ADD COLUMN usage_output_tokens INTEGER CHECK(usage_output_tokens >= 0)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM completed_turns`).Scan(&count); err != nil {
		return report, err
	}
	if count != report.Completed {
		return report, fmt.Errorf("threads: completed row count changed during usage migration: got %d want %d", count, report.Completed)
	}
	report.SubscriptionCursors = subscriptionCursors
	raw, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,?)`, completedUsageMigrationName, string(raw), `{}`); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=22`); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, verifyCompletedUsageSchema(ctx, db)
}

func verifyCompletedUsageSchema(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	for _, column := range []string{"usage_input_tokens", "usage_cached_input_tokens", "usage_output_tokens"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('completed_turns') WHERE name=?`, column).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("threads: missing completed_turns.%s", column)
		}
	}
	return nil
}
