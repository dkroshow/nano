package statestore

import (
	"context"
	"database/sql"
	"errors"
	"os"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

// OpenReadOnly validates the canonical schema without creating or migrating a
// store. Offline diagnosis never falls back to legacy files.
func OpenReadOnly(ctx context.Context, path string, migrations statedb.MigrationRunner) (*sql.DB, error) {
	return openReadOnly(ctx, path, false, migrations)
}

// OpenBackupSource also accepts the supported pre-cutover schema. This lets an
// operator preserve the legacy store before an approved change without running
// a migration or selecting a different runtime store.
func OpenBackupSource(ctx context.Context, path string, migrations statedb.MigrationRunner) (*sql.DB, error) {
	return openReadOnly(ctx, path, true, migrations)
}

func openReadOnly(ctx context.Context, path string, allowLegacy bool, migrations statedb.MigrationRunner) (*sql.DB, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("threads: source is not a complete regular store")
	}
	db, err := sql.Open("sqlite", statedb.URI(path, "ro"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err == nil {
		switch {
		case version == turns.TurnSchemaVersion:
			if _, err = turns.MigrateTurnTransactions(ctx, db, ""); err == nil {
				if err = migrateCompletedTurnUsage(ctx, db, "", migrations); err == nil {
					if err = migrateMemberRecovery(ctx, db, "", migrations); err == nil {
						err = migrateParkedDiscard(ctx, db, "", migrations)
					}
				}
			}
		case allowLegacy && version == turns.MemberRecoveryTurnSchemaVersion:
			if _, err = turns.MigrateTurnTransactions(ctx, db, ""); err == nil {
				if err = migrateCompletedTurnUsage(ctx, db, "", migrations); err == nil {
					err = migrateMemberRecovery(ctx, db, "", migrations)
				}
			}
		case allowLegacy && version == turns.UsageTurnSchemaVersion:
			if _, err = turns.MigrateTurnTransactions(ctx, db, ""); err == nil {
				err = migrateCompletedTurnUsage(ctx, db, "", migrations)
			}
		case allowLegacy && version == turns.TransactionalTurnSchemaVersion:
			_, err = turns.MigrateTurnTransactions(ctx, db, "")
		case allowLegacy && version == turns.LegacyTurnSchemaVersion:
			_, err = turns.MigrateTurnRecords(ctx, db, turns.LegacyTurnPaths{}, "")
		case allowLegacy && version == 19:
			err = CheckMetadataIntegrity(db, migrations)
		default:
			err = errors.New("threads: source is not a supported store schema")
		}
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
