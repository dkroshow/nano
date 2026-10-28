package hosted

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	state "github.com/dkroshow/nano/internal/app/statestore"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

func openTestMetadataStore(path string) (*MetadataStore, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve control metadata path: %w", err)
	}
	db, err := statedb.Open(context.Background(), abs)
	if err != nil {
		return nil, err
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 0 && version < state.LegacySchemaVersion {
		backupRoot := filepath.Join(filepath.Dir(abs), "threads-schema-backups")
		if err := os.MkdirAll(backupRoot, 0700); err != nil {
			db.Close()
			return nil, err
		}
		backupDir, err := os.MkdirTemp(backupRoot, "before-schema-")
		if err != nil {
			db.Close()
			return nil, err
		}
		if err := threads.Backup(context.Background(), db, filepath.Join(backupDir, "threads.sqlite")); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
		db.Close()
		return nil, err
	}

	// Browser registry reads can traverse every hosted session. Give those
	// read-only projections a bounded WAL pool so they never monopolize the one
	// serialized control-plane connection used by durable writes such as Codex
	// /fast.
	u := url.URL{Scheme: "file", Path: abs}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = query.Encode()
	readDB, err := sql.Open("sqlite", u.String())
	if err != nil {
		db.Close()
		return nil, err
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)
	if err := readDB.Ping(); err != nil {
		readDB.Close()
		db.Close()
		return nil, fmt.Errorf("open canonical metadata read pool: %w", err)
	}
	store, err := NewMetadataStore(db, readDB)
	if err != nil {
		readDB.Close()
		db.Close()
		return nil, err
	}
	return store, nil
}
