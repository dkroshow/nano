// Package statewiring composes state-table owners at the program edge.
package statewiring

import (
	"context"
	"database/sql"
	"fmt"

	appstate "github.com/dkroshow/nano/internal/app/persistence"
	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

// Migrations returns the owner work in its existing migration order.
func Migrations() statedb.MigrationRunner {
	return func(ctx context.Context, stage statedb.MigrationStage, db *sql.DB, tx *sql.Tx, version int, backupPath string) error {
		switch stage {
		case statedb.MigrationIntegrity:
			if err := threads.CheckStateIntegrity(db); err != nil {
				return err
			}
			if err := mail.CheckLegacyStateIntegrity(db); err != nil {
				return err
			}
			return diagnostics.CheckStateIntegrity(db)
		case statedb.MigrationSubscriptions:
			return subscriptions.MigrateStateSchema(db)
		case statedb.MigrationTransactionStart:
			if err := subscriptions.MigrateStateSchema(tx); err != nil {
				return err
			}
			return diagnostics.MigrateStateSchema(tx, version)
		case statedb.MigrationPermissionEvents:
			return appstate.MigratePermissionEventSchema(tx, version)
		case statedb.MigrationMail:
			return mail.MigrateLegacyStateSchema(tx, version)
		case statedb.MigrationNativeRecovery:
			return appstate.MigrateNativeRecoverySchema(tx)
		case statedb.MigrationCompletedUsage:
			cursors, err := subscriptions.StateCursorCount(db)
			if err != nil {
				return err
			}
			_, err = coreturns.MigrateCompletedTurnUsageWithCursors(ctx, db, backupPath, cursors)
			return err
		case statedb.MigrationMemberRecovery:
			_, err := prompts.MigrateMemberRecovery(ctx, db, backupPath)
			return err
		case statedb.MigrationParkedDiscard:
			_, err := prompts.MigrateParkedDiscard(ctx, db, backupPath)
			return err
		default:
			return fmt.Errorf("statewiring: unknown migration stage %d", stage)
		}
	}
}
