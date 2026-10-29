package statestore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	statedb "github.com/dkroshow/nano/internal/state/db"
)


// testMigrations keeps statestore tests inside their package boundary. Tests
// exercise statestore's own schema; feature-owner migrations are covered by
// their packages and by statewiring tests.
func testMigrations() statedb.MigrationRunner {
	return func(ctx context.Context, stage statedb.MigrationStage, db *sql.DB, _ *sql.Tx, _ int, backupPath string) error {
		switch stage {
		case statedb.MigrationIntegrity, statedb.MigrationSubscriptions, statedb.MigrationTransactionStart,
			statedb.MigrationPermissionEvents, statedb.MigrationMail, statedb.MigrationNativeRecovery:
			return nil
		case statedb.MigrationCompletedUsage:
			_, err := coreturns.MigrateCompletedTurnUsage(ctx, db, backupPath)
			return err
		case statedb.MigrationMemberRecovery:
			_, err := prompts.MigrateMemberRecovery(ctx, db, backupPath)
			return err
		case statedb.MigrationParkedDiscard:
			_, err := prompts.MigrateParkedDiscard(ctx, db, backupPath)
			return err
		default:
			return fmt.Errorf("statestore test: unknown migration stage %d", stage)
		}
	}
}
