package prompts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dkroshow/nano/internal/threads"
)

const parkedDiscardMigrationName = "parked-discard-v24"

type ParkedDiscardMigrationReport struct {
	BackupPath string `json:"backupPath"`
	Queued     int    `json:"queued"`
	Parked     int    `json:"parked"`
}

// parkedDiscardTrigger replaces schema 21's update guard. Confirmed and
// refused prompts stay final; a parked prompt may now only expire, which is
// how the person's Discard ends it for good. No row changes: every parked
// prompt stays parked until someone discards it.
var parkedDiscardStatements = []string{
	`DROP TRIGGER queued_turns_managed_update`,
	`CREATE TRIGGER queued_turns_managed_update BEFORE UPDATE ON queued_turns
		WHEN NEW.client_action_id <> OLD.client_action_id OR NEW.provider <> OLD.provider OR
			NEW.position <> OLD.position OR
			(NEW.claim_token <> '' AND NEW.claim_epoch <= 0) OR
			(OLD.state IN ('confirmed','refused') AND NEW.state <> OLD.state) OR
			(OLD.state = 'parked' AND NEW.state NOT IN ('parked','expired'))
		BEGIN SELECT RAISE(ABORT,'invalid managed queued turn update'); END`,
}

// MigrateParkedDiscard extends a backed-up schema-23 candidate to schema 24.
// Its caller migrates a private candidate, never the live file, and publishes
// it only after this succeeds. At schema 24 it only verifies the receipt and
// the guard.
func MigrateParkedDiscard(ctx context.Context, db *sql.DB, backupPath string) (ParkedDiscardMigrationReport, error) {
	var report ParkedDiscardMigrationReport
	if db == nil {
		return report, errors.New("threads: parked discard migration database is required")
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	if version == TurnSchemaVersion {
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, parkedDiscardMigrationName).Scan(&raw); err != nil {
			return report, fmt.Errorf("threads: missing parked discard migration receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return report, err
		}
		return report, verifyParkedDiscardSchema(ctx, db)
	}
	if version != MemberRecoveryTurnSchemaVersion {
		return report, fmt.Errorf("threads: parked discard migration requires schema %d, found %d", MemberRecoveryTurnSchemaVersion, version)
	}
	if backupPath == "" {
		return report, errors.New("threads: parked discard migration requires a new backup path")
	}
	if err := threads.Backup(ctx, db, backupPath); err != nil {
		return report, fmt.Errorf("threads: back up parked discard source: %w", err)
	}
	report.BackupPath = backupPath
	counts := func(q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}) (queued, parked int, err error) {
		err = q.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(state='parked'),0) FROM queued_turns`).Scan(&queued, &parked)
		return queued, parked, err
	}
	var err error
	if report.Queued, report.Parked, err = counts(db); err != nil {
		return report, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range parkedDiscardStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, fmt.Errorf("threads: migrate parked discard: %w", err)
		}
	}
	queued, parked, err := counts(tx)
	if err != nil {
		return report, err
	}
	if queued != report.Queued || parked != report.Parked {
		return report, errors.New("threads: queued_turns changed during parked discard migration")
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return report, fmt.Errorf("threads: parked discard integrity check: %s: %w", integrity, err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,?)`, parkedDiscardMigrationName, string(raw), `{}`); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, TurnSchemaVersion)); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, verifyParkedDiscardSchema(ctx, db)
}

// verifyParkedDiscardSchema checks the schema-24 update guard exactly as the
// migration created it.
func verifyParkedDiscardSchema(ctx context.Context, db *sql.DB) error {
	var statement string
	err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='trigger' AND name='queued_turns_managed_update'`).Scan(&statement)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("threads: missing trigger queued_turns_managed_update")
	}
	if err != nil {
		return err
	}
	if statement != strings.TrimSpace(parkedDiscardStatements[1]) {
		return fmt.Errorf("threads: trigger queued_turns_managed_update is %q", statement)
	}
	return nil
}
