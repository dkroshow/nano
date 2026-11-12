package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	statedb "github.com/dkroshow/nano/internal/state/db"
	state "github.com/dkroshow/nano/internal/state/store"
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
		dir, err := os.MkdirTemp("", "nano-schema-")
		if err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		defer os.RemoveAll(dir)
		// The startup path itself builds the current schema from nothing.
		path := filepath.Join(dir, "nano_threads.sqlite")
		if _, err := state.PrepareStore(context.Background(), state.StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "backups")}, statewiring.Migrations()); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		transactionalTurnFixtureData, transactionalTurnFixtureErr = os.ReadFile(path)
	})
	return transactionalTurnFixtureData, transactionalTurnFixtureErr
}

// AttachmentStore opens an attachment store over a fresh current-schema
// threads database. Reopening the returned database with
// hostedstore.NewAttachmentStore stands in for a Nano restart.
func AttachmentStore(t testing.TB) (*hostedstore.AttachmentStore, *sql.DB) {
	t.Helper()
	db, _, err := TurnDatabase(t, filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := hostedstore.NewAttachmentStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}
