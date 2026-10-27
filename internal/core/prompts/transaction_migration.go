package prompts

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dkroshow/nano/internal/threads"
)

// TransactionalTurnSchemaVersion is the first schema with transactional prompt
// and completion record families. C1 splits their runtime owners without a
// version bump. UsageTurnSchemaVersion also keeps provider-reported usage on
// each terminal feed row. MemberRecoveryTurnSchemaVersion also keeps each
// Claude prompt's owed member completion and each provisional handle's proven
// native (member_recovery_migration.go). TurnSchemaVersion, the current
// schema, lets a parked prompt expire (parked_discard_migration.go).
const TransactionalTurnSchemaVersion = 21
const UsageTurnSchemaVersion = 22
const MemberRecoveryTurnSchemaVersion = 23
const TurnSchemaVersion = 24

const turnTransactionMigrationName = "turn-transactions-v21"

type TurnTransactionMigrationReport struct {
	BackupPath        string `json:"backupPath"`
	Queued            int    `json:"queued"`
	Attempts          int    `json:"attempts"`
	Completed         int    `json:"completed"`
	CompletedSequence int64  `json:"completedSequence"`
}

// MigrateTurnTransactions extends a backed-up schema-20 candidate. The caller
// publishes that candidate only after this function succeeds. Existing sources
// and the backup are never removed or overwritten.
func MigrateTurnTransactions(ctx context.Context, db *sql.DB, backupPath string) (TurnTransactionMigrationReport, error) {
	return migrateTurnTransactions(ctx, db, backupPath, nil)
}

func migrateTurnTransactions(ctx context.Context, db *sql.DB, backupPath string, afterSchema func(*sql.Tx) error) (TurnTransactionMigrationReport, error) {
	var report TurnTransactionMigrationReport
	if db == nil {
		return report, errors.New("threads: turn transaction migration database is required")
	}
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	if version >= TransactionalTurnSchemaVersion && version <= TurnSchemaVersion {
		var raw string
		if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, turnTransactionMigrationName).Scan(&raw); err != nil {
			return report, fmt.Errorf("threads: missing turn transaction migration receipt: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			return report, err
		}
		if err := verifyTurnTransactionSchema(ctx, db); err != nil {
			return TurnTransactionMigrationReport{}, err
		}
		return report, nil
	}
	if version != LegacyTurnSchemaVersion {
		return report, fmt.Errorf("threads: turn transaction migration requires schema %d, found %d", LegacyTurnSchemaVersion, version)
	}
	if backupPath == "" {
		return report, errors.New("threads: turn transaction migration requires a new backup path")
	}
	if err := threads.Backup(ctx, db, backupPath); err != nil {
		return report, fmt.Errorf("threads: back up turn transaction source: %w", err)
	}
	report.BackupPath = backupPath
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM queued_turns`).Scan(&report.Queued); err != nil {
		return report, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM send_attempts`).Scan(&report.Attempts); err != nil {
		return report, err
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM completed_turns`).Scan(&report.Completed); err != nil {
		return report, err
	}
	err := db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name='completed_turns'`).Scan(&report.CompletedSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return report, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range turnTransactionMigrationStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, fmt.Errorf("threads: migrate turn transactions: %w", err)
		}
	}
	for _, statement := range turnTransactionGuardStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, fmt.Errorf("threads: guard turn transactions: %w", err)
		}
	}
	if afterSchema != nil {
		if err := afterSchema(tx); err != nil {
			return report, err
		}
	}
	for _, expected := range []struct {
		table string
		count int
	}{{"queued_turns", report.Queued}, {"send_attempts", report.Attempts}, {"completed_turns", report.Completed}} {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+expected.table).Scan(&count); err != nil {
			return report, err
		}
		if count != expected.count {
			return report, fmt.Errorf("threads: %s count changed during turn transaction migration", expected.table)
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		return report, fmt.Errorf("threads: turn transaction foreign-key check: violations=%d: %w", violations, err)
	}
	var integrity string
	if err := tx.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return report, fmt.Errorf("threads: turn transaction integrity check: %s: %w", integrity, err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO threads_store_migrations(name,report_json,manifest_json) VALUES(?,?,?)`, turnTransactionMigrationName, string(raw), `{}`); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=21`); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, verifyTurnTransactionSchema(ctx, db)
}

var turnTransactionMigrationStatements = []string{
	`ALTER TABLE queued_turns ADD COLUMN revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0)`,
	`ALTER TABLE queued_turns ADD COLUMN delivery_rank INTEGER NOT NULL DEFAULT 0 CHECK(delivery_rank IN (0,1,2))`,
	`ALTER TABLE queued_turns ADD COLUMN binding_json TEXT NOT NULL DEFAULT '' CHECK(binding_json = '' OR json_valid(binding_json))`,
	`ALTER TABLE queued_turns ADD COLUMN binding_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE queued_turns ADD COLUMN claim_token TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE queued_turns ADD COLUMN claim_epoch INTEGER NOT NULL DEFAULT 0 CHECK(claim_epoch >= 0)`,
	`UPDATE queued_turns SET delivery_rank=CASE origin WHEN 'agent' THEN 1 WHEN 'system' THEN 2 ELSE 0 END`,
	`ALTER TABLE send_attempts ADD COLUMN hosted_thread_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE send_attempts ADD COLUMN revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0)`,
	`ALTER TABLE send_attempts ADD COLUMN binding_json TEXT NOT NULL DEFAULT '' CHECK(binding_json = '' OR json_valid(binding_json))`,
	`ALTER TABLE send_attempts ADD COLUMN claim_token TEXT NOT NULL DEFAULT ''`,
	`UPDATE send_attempts SET hosted_thread_id=COALESCE((SELECT q.hosted_thread_id FROM queued_turns q WHERE q.client_action_id=send_attempts.client_action_id),'')`,
	`ALTER TABLE completed_turns ADD COLUMN client_action_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE completed_turns ADD COLUMN native_thread_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE completed_turns ADD COLUMN binding_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE completed_turns ADD COLUMN outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','completed','failed','interrupted','truncated','declined','cancelled','unknown'))`,
}

var turnTransactionGuardStatements = []string{
	`CREATE UNIQUE INDEX queued_turns_binding_claim ON queued_turns(binding_key) WHERE binding_key <> ''`,
	`CREATE UNIQUE INDEX queued_turns_dispatch_claim ON queued_turns(hosted_thread_id) WHERE claim_token <> ''`,
	`CREATE INDEX queued_turns_drain_order ON queued_turns(hosted_thread_id,state,delivery_rank,position)`,
	`CREATE INDEX send_attempts_host_action ON send_attempts(hosted_thread_id,client_action_id)`,
	`CREATE UNIQUE INDEX completed_turns_binding ON completed_turns(binding_key) WHERE binding_key <> ''`,
	`CREATE UNIQUE INDEX completed_turns_legacy_turn ON completed_turns(target_hosted_thread_id,turn_id) WHERE binding_key = ''`,
	`CREATE TRIGGER queued_turns_managed_insert BEFORE INSERT ON queued_turns
		WHEN NEW.revision > 0 AND (NEW.hosted_thread_id = '' OR NEW.provider = '' OR NEW.client_action_id = '' OR
			NEW.state NOT IN ('queued','delivering','accepted','uncertain','confirmed','refused','parked') OR
			(NEW.claim_token <> '' AND NEW.claim_epoch <= 0))
		BEGIN SELECT RAISE(ABORT,'invalid managed queued turn'); END`,
	`CREATE TRIGGER queued_turns_managed_update BEFORE UPDATE ON queued_turns
		WHEN NEW.client_action_id <> OLD.client_action_id OR NEW.provider <> OLD.provider OR
			NEW.position <> OLD.position OR
			(NEW.claim_token <> '' AND NEW.claim_epoch <= 0) OR
			(OLD.state IN ('confirmed','refused','parked') AND NEW.state <> OLD.state)
		BEGIN SELECT RAISE(ABORT,'invalid managed queued turn update'); END`,
}

func verifyTurnTransactionSchema(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	for _, name := range []string{
		"queued_turns_binding_claim", "queued_turns_dispatch_claim", "queued_turns_drain_order",
		"send_attempts_host_action", "completed_turns_binding", "completed_turns_legacy_turn", "completed_turns_target_order",
		"queued_turns_managed_insert", "queued_turns_managed_update",
	} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema WHERE name=?`, name).Scan(&count); err != nil || count != 1 {
			return fmt.Errorf("threads: missing turn transaction schema object %s: %w", name, err)
		}
	}
	for table, columns := range map[string][]string{
		"queued_turns":    {"position", "revision", "delivery_rank", "binding_json", "binding_key", "claim_token", "claim_epoch"},
		"send_attempts":   {"hosted_thread_id", "revision", "binding_json", "claim_token"},
		"completed_turns": {"client_action_id", "native_thread_id", "binding_key", "outcome"},
	} {
		for _, column := range columns {
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count); err != nil || count != 1 {
				return fmt.Errorf("threads: missing %s.%s: %w", table, column, err)
			}
		}
	}
	return nil
}
