package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	state "github.com/dkroshow/nano/internal/app/statestore"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

// Existing delivery fixtures name a queue/journal path. Both now resolve to
// one disposable SQLite file in that fixture directory, just as startup does.
var (
	transactionalTurnFixtureOnce sync.Once
	transactionalTurnFixtureData []byte
	transactionalTurnFixtureErr  error
)

func TurnDatabase(t testing.TB, legacyPath string) (*sql.DB, string, error) {
	t.Helper()
	path := filepath.Join(filepath.Dir(legacyPath), "nano_threads.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		data, err := transactionalTurnFixture()
		if err != nil {
			return nil, path, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, path, err
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, path, err
		}
	} else if err != nil {
		return nil, path, err
	}
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		return nil, path, err
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return nil, path, err
	}
	if version != turns.TurnSchemaVersion {
		return nil, path, fmt.Errorf("schema %d is not current schema %d", version, turns.TurnSchemaVersion)
	}
	return db, path, nil
}

func transactionalTurnFixture() ([]byte, error) {
	transactionalTurnFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nano-schema21-")
		if err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "source.sqlite")
		db, err := statedb.Open(context.Background(), path)
		if err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		defer db.Close()
		if err := state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateTurnRecords(context.Background(), db, turns.LegacyTurnPaths{}, filepath.Join(dir, "v19-backup")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateTurnTransactions(context.Background(), db, filepath.Join(dir, "v20.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := coreturns.MigrateCompletedTurnUsage(context.Background(), db, filepath.Join(dir, "v21.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateMemberRecovery(context.Background(), db, filepath.Join(dir, "v22.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateParkedDiscard(context.Background(), db, filepath.Join(dir, "v23.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		backup := filepath.Join(dir, "schema24.sqlite")
		if err := threads.Backup(context.Background(), db, backup); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		transactionalTurnFixtureData, transactionalTurnFixtureErr = os.ReadFile(backup)
	})
	return transactionalTurnFixtureData, transactionalTurnFixtureErr
}
