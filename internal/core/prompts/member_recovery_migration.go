package prompts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dkroshow/nano/internal/threads"
)

const memberRecoveryMigrationName = "claude-member-recovery-v23"

type MemberRecoveryMigrationReport struct {
	BackupPath string `json:"backupPath"`
	Queued     int    `json:"queued"`
	NativeIDs  int    `json:"nativeIds"`
}

// memberRecoveryStatements add, with no backfill, the two facts Claude member
// recovery retains: a prompt's owed member completion, written only by a new
// settlement, and a provisional handle's proven native, written once by the
// native claim. The indexes keep each read to current owed work, to one
// prompt's own event, and to one prompt's own retained operation rows. That
// last index is not unique: two operations for one action stay visible as an
// error.
var memberRecoveryStatements = []string{
	`ALTER TABLE queued_turns ADD COLUMN member_owed INTEGER NOT NULL DEFAULT 0 CHECK(member_owed IN (0,1))`,
	`CREATE INDEX queued_turns_member_owed ON queued_turns(hosted_thread_id, position) WHERE member_owed = 1`,
	`CREATE INDEX queued_turns_claude_event ON queued_turns(hosted_thread_id, json_extract(binding_json, '$.eventId')) WHERE provider = 'claude' AND binding_json <> ''`,
	`ALTER TABLE hosted_thread_native_ids ADD COLUMN resolved_native_id TEXT`,
	`CREATE INDEX hosted_turn_operations_transport ON hosted_turn_operations(hosted_thread_id, transport_action_id)`,
}

// MigrateMemberRecovery extends a backed-up schema-22 candidate to schema 23.
// Its caller migrates a private candidate, never the live file, and publishes
// it only after this succeeds. At schema 23 it only verifies the receipt and
// the schema.
func MigrateMemberRecovery(ctx context.Context, db *sql.DB, backupPath string) (MemberRecoveryMigrationReport, error) {
	var report MemberRecoveryMigrationReport
	if db == nil {
		return report, errors.New("threads: member recovery migration database is required")
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	if version >= MemberRecoveryTurnSchemaVersion && version <= TurnSchemaVersion {
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, memberRecoveryMigrationName).Scan(&raw); err != nil {
			return report, fmt.Errorf("threads: missing member recovery migration receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return report, err
		}
		return report, verifyMemberRecoverySchema(ctx, db)
	}
	if version != UsageTurnSchemaVersion {
		return report, fmt.Errorf("threads: member recovery migration requires schema %d, found %d", UsageTurnSchemaVersion, version)
	}
	if backupPath == "" {
		return report, errors.New("threads: member recovery migration requires a new backup path")
	}
	if err := threads.Backup(ctx, db, backupPath); err != nil {
		return report, fmt.Errorf("threads: back up member recovery source: %w", err)
	}
	report.BackupPath = backupPath
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM queued_turns`).Scan(&report.Queued); err != nil {
		return report, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM hosted_thread_native_ids`).Scan(&report.NativeIDs); err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range memberRecoveryStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, fmt.Errorf("threads: migrate member recovery: %w", err)
		}
	}
	for _, expected := range []struct {
		table string
		count int
	}{{"queued_turns", report.Queued}, {"hosted_thread_native_ids", report.NativeIDs}} {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+expected.table).Scan(&count); err != nil {
			return report, err
		}
		if count != expected.count {
			return report, fmt.Errorf("threads: %s count changed during member recovery migration", expected.table)
		}
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return report, fmt.Errorf("threads: member recovery integrity check: %s: %w", integrity, err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,?)`, memberRecoveryMigrationName, string(raw), `{}`); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=23`); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, verifyMemberRecoverySchema(ctx, db)
}

// verifyMemberRecoverySchema checks the schema-23 objects by definition: the
// owed column's type, NOT NULL and zero default; the resolution column's type
// and nullability; and each index exactly as the migration created it.
func verifyMemberRecoverySchema(ctx context.Context, db *sql.DB) error {
	for _, column := range []struct {
		table, name, kind, defaultValue string
		notNull                         bool
	}{{"queued_turns", "member_owed", "INTEGER", "0", true}, {"hosted_thread_native_ids", "resolved_native_id", "TEXT", "", false}} {
		var kind string
		var notNull bool
		var defaultValue sql.NullString
		err := db.QueryRowContext(ctx, `SELECT type,"notnull",dflt_value FROM pragma_table_info(?) WHERE name=?`, column.table, column.name).Scan(&kind, &notNull, &defaultValue)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("threads: missing %s.%s", column.table, column.name)
		}
		if err != nil {
			return err
		}
		if kind != column.kind || notNull != column.notNull || defaultValue.String != column.defaultValue {
			return fmt.Errorf("threads: %s.%s is %s notnull=%v default=%q", column.table, column.name, kind, notNull, defaultValue.String)
		}
	}
	for _, index := range []struct{ name, statement string }{
		{"queued_turns_member_owed", memberRecoveryStatements[1]},
		{"queued_turns_claude_event", memberRecoveryStatements[2]},
		{"hosted_turn_operations_transport", memberRecoveryStatements[4]},
	} {
		var statement string
		err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, index.name).Scan(&statement)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("threads: missing index %s", index.name)
		}
		if err != nil {
			return err
		}
		if statement != index.statement {
			return fmt.Errorf("threads: index %s is %q", index.name, statement)
		}
	}
	return nil
}
