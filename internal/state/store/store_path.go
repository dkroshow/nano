package statestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)


// StorePaths keeps the canonical destination separate from retained legacy
// sources. Writers must be stopped for the whole PrepareStore call.
type StorePaths struct {
	Destination, Control, BackupRoot string
	Turns                            turns.LegacyTurnPaths
}

// PrepareStore publishes nano_threads.sqlite only after a private copy has
// migrated and reconciled all three record families. A schema-20 destination
// is backed up, copied, migrated, validated, and atomically replaced while all
// writers are stopped. Legacy files and the pre-migration store stay retained.
// A prepared store that still holds transcript_boundaries is backed up, then
// loses that table.
func PrepareStore(ctx context.Context, paths StorePaths, migrations statedb.MigrationRunner) (turns.TurnMigrationReport, error) {
	report, err := prepareStore(ctx, paths, verifySourceFingerprints, migrations)
	if err != nil {
		return report, err
	}
	return report, retireTranscriptBoundaries(ctx, paths.Destination, paths.BackupRoot)
}

func prepareStore(ctx context.Context, paths StorePaths, verifyLegacyCopy sourceFingerprintVerifier, migrations statedb.MigrationRunner) (turns.TurnMigrationReport, error) {
	var report turns.TurnMigrationReport
	if paths.Destination == "" || paths.BackupRoot == "" {
		return report, errors.New("threads: destination and backup root are required")
	}
	destination, err := filepath.Abs(paths.Destination)
	if err != nil {
		return report, err
	}
	for _, source := range []string{paths.Control, paths.Turns.Queue, paths.Turns.Attempts, paths.Turns.Completed} {
		if source == "" {
			continue
		}
		absolute, err := filepath.Abs(source)
		if err != nil {
			return report, err
		}
		if absolute == destination {
			return report, errors.New("threads: destination must differ from legacy sources")
		}
	}
	info, err := os.Lstat(destination)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return report, errors.New("threads: existing destination is not a complete regular store")
		}
		return prepareExistingStore(ctx, destination, paths, migrations)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(destination + suffix); err == nil {
			return report, errors.New("threads: destination has a stranded SQLite journal")
		} else if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	sources, err := fingerprintSources(ctx, paths)
	if err != nil {
		return report, err
	}
	if err := os.MkdirAll(paths.BackupRoot, 0700); err != nil {
		return report, err
	}
	run, err := os.MkdirTemp(paths.BackupRoot, "threads-migration-")
	if err != nil {
		return report, err
	}
	candidate := filepath.Join(run, "candidate.sqlite")
	// Only this invocation's disposable candidate is removed. Backups and every
	// original source remain available even when migration or publication fails.
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(candidate + suffix)
		}
	}()
	if paths.Control != "" {
		info, err := os.Stat(paths.Control)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return report, errors.New("threads: legacy control store is empty or not regular")
			}
			source, err := sql.Open("sqlite", statedb.URI(paths.Control, "ro"))
			if err != nil {
				return report, err
			}
			original := filepath.Join(run, "control-original.sqlite")
			backupErr := threads.Backup(ctx, source, original)
			if err := errors.Join(backupErr, source.Close()); err != nil {
				return report, err
			}
			if err := threads.Restore(ctx, original, candidate); err != nil {
				return report, err
			}
		} else {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Lstat(paths.Control + suffix); err == nil {
					return report, errors.New("threads: legacy control store has a stranded journal")
				} else if !errors.Is(err, os.ErrNotExist) {
					return report, err
				}
			}
		}
	}
	db, err := statedb.Open(ctx, candidate)
	if err != nil {
		return report, err
	}
	defer db.Close()
	if err := MigrateLegacySchema(db, migrations); err != nil {
		return report, err
	}
	var preCopyVersion int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&preCopyVersion); err != nil {
		return report, err
	}
	report, err = turns.MigrateTurnRecordsWithSources(ctx, db, paths.Turns, filepath.Join(run, "turn-records"), sources)
	if err != nil {
		return report, err
	}
	if err := verifyCopiedLegacySources(ctx, db, paths, preCopyVersion, verifyLegacyCopy); err != nil {
		return report, err
	}
	if _, err := turns.MigrateTurnTransactions(ctx, db, filepath.Join(run, "turn-transactions-v20.sqlite")); err != nil {
		return report, err
	}
	if err := migrateCompletedTurnUsage(ctx, db, filepath.Join(run, "completed-turn-usage-v21.sqlite"), migrations); err != nil {
		return report, err
	}
	if err := migrateMemberRecovery(ctx, db, filepath.Join(run, "member-recovery-v22.sqlite"), migrations); err != nil {
		return report, err
	}
	if err := migrateParkedDiscard(ctx, db, filepath.Join(run, "parked-discard-v23.sqlite"), migrations); err != nil {
		return report, err
	}
	if err := validatePreparedStore(ctx, db, migrations); err != nil {
		return report, err
	}
	// Backup's checked, synced no-replace publication is the path cutover. There
	// is no interval where a partial schema can masquerade as the canonical file.
	if err := threads.Backup(ctx, db, destination); err != nil {
		return report, err
	}
	return report, nil
}

func prepareExistingStore(ctx context.Context, destination string, paths StorePaths, migrations statedb.MigrationRunner) (report turns.TurnMigrationReport, err error) {
	if err := os.MkdirAll(paths.BackupRoot, 0700); err != nil {
		return report, err
	}
	db, err := statedb.Open(ctx, destination)
	if err != nil {
		return report, err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, db.Close())
		}
	}()
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return report, err
	}
	switch version {
	case turns.TurnSchemaVersion:
		if _, err := turns.MigrateTurnTransactions(ctx, db, ""); err != nil {
			return report, err
		}
		if err := migrateCompletedTurnUsage(ctx, db, "", migrations); err != nil {
			return report, err
		}
		if err := migrateMemberRecovery(ctx, db, "", migrations); err != nil {
			return report, err
		}
		if err := migrateParkedDiscard(ctx, db, "", migrations); err != nil {
			return report, err
		}
		if err := validatePreparedStore(ctx, db, migrations); err != nil {
			return report, err
		}
		return readTurnMigrationReport(ctx, db)
	case turns.MemberRecoveryTurnSchemaVersion, turns.UsageTurnSchemaVersion, turns.TransactionalTurnSchemaVersion:
		report, err = readTurnMigrationReport(ctx, db)
		if err != nil {
			return report, err
		}
	case turns.LegacyTurnSchemaVersion:
		report, err = turns.MigrateTurnRecords(ctx, db, turns.LegacyTurnPaths{}, "")
		if err != nil {
			return report, err
		}
	case LegacySchemaVersion:
		return report, fmt.Errorf("threads: destination schema %d is not a completed turn-record migration", version)
	default:
		return report, fmt.Errorf("threads: destination schema %d is not supported", version)
	}

	if err := validatePreparedStore(ctx, db, migrations); err != nil {
		return report, err
	}
	run, err := os.MkdirTemp(paths.BackupRoot, "threads-schema-")
	if err != nil {
		return report, err
	}
	original := filepath.Join(run, fmt.Sprintf("nano-threads-v%d.sqlite", version))
	if err := threads.Backup(ctx, db, original); err != nil {
		return report, err
	}
	candidate := filepath.Join(run, "candidate.sqlite")
	if err := threads.Restore(ctx, original, candidate); err != nil {
		return report, err
	}
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(candidate + suffix)
		}
	}()
	candidateDB, err := statedb.Open(ctx, candidate)
	if err != nil {
		return report, err
	}
	candidateClosed := false
	defer func() {
		if !candidateClosed {
			err = errors.Join(err, candidateDB.Close())
		}
	}()
	if version == turns.LegacyTurnSchemaVersion {
		if _, err := turns.MigrateTurnTransactions(ctx, candidateDB, filepath.Join(run, "migration-input-v20.sqlite")); err != nil {
			return report, err
		}
	}
	if err := migrateCompletedTurnUsage(ctx, candidateDB, filepath.Join(run, "migration-input-v21.sqlite"), migrations); err != nil {
		return report, err
	}
	if err := migrateMemberRecovery(ctx, candidateDB, filepath.Join(run, "migration-input-v22.sqlite"), migrations); err != nil {
		return report, err
	}
	if err := migrateParkedDiscard(ctx, candidateDB, filepath.Join(run, "migration-input-v23.sqlite"), migrations); err != nil {
		return report, err
	}
	if err := validatePreparedStore(ctx, candidateDB, migrations); err != nil {
		return report, err
	}
	staged, err := reservePublicationPath(filepath.Dir(destination))
	if err != nil {
		return report, err
	}
	defer os.Remove(staged)
	if err := threads.Backup(ctx, candidateDB, staged); err != nil {
		return report, err
	}
	if err := candidateDB.Close(); err != nil {
		return report, err
	}
	candidateClosed = true
	if err := checkpointStandalone(ctx, db); err != nil {
		return report, err
	}
	if err := db.Close(); err != nil {
		return report, err
	}
	closed = true
	if err := retainSQLiteSidecars(destination, run); err != nil {
		return report, err
	}
	if err := os.Rename(staged, destination); err != nil {
		return report, fmt.Errorf("threads: publish transactional store: %w", err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return report, err
	}
	verified, err := OpenReadOnly(ctx, destination, migrations)
	if err != nil {
		return report, fmt.Errorf("threads: verify published transactional store: %w", err)
	}
	err = errors.Join(validatePreparedStore(ctx, verified, migrations), verified.Close())
	return report, err
}

func validatePreparedStore(ctx context.Context, db *sql.DB, migrations statedb.MigrationRunner) error {
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("threads: destination integrity: %s", integrity)
	}
	return CheckMetadataIntegrity(db, migrations)
}

func readTurnMigrationReport(ctx context.Context, db *sql.DB) (turns.TurnMigrationReport, error) {
	var report turns.TurnMigrationReport
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT report_json FROM threads_store_migrations WHERE name=?`, turns.MigrationName).Scan(&raw); err != nil {
		return report, fmt.Errorf("threads: missing turn migration receipt: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return report, err
	}
	return report, nil
}

func reservePublicationPath(directory string) (string, error) {
	file, err := os.CreateTemp(directory, ".threads-publish-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func checkpointStandalone(ctx context.Context, db *sql.DB) error {
	var busy, pages, checkpointed int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &pages, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("threads: canonical store stayed busy during publication checkpoint")
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return err
	}
	if mode != "delete" {
		return fmt.Errorf("threads: canonical store journal mode remained %q", mode)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func retainSQLiteSidecars(path, directory string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.Rename(path+suffix, filepath.Join(directory, "nano-threads-v20.sqlite"+suffix)); err != nil {
			return err
		}
	}
	return nil
}
