// Package db owns the shared state database connection policy.
package db

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

// MigrationStage identifies the point where table owners join the legacy
// store migration. The edge supplies the ordered owner work.
type MigrationStage uint8

const (
	MigrationIntegrity MigrationStage = iota
	MigrationSubscriptions
	MigrationTransactionStart
	MigrationPermissionEvents
	MigrationMail
	MigrationNativeRecovery
	MigrationCompletedUsage
	MigrationMemberRecovery
	MigrationParkedDiscard
)

// MigrationRunner composes state-table owners without making app depend on
// feature or diagnostics packages.
type MigrationRunner func(context.Context, MigrationStage, *sql.DB, *sql.Tx, int, string) error

// Open creates a private SQLite file before SQLite can create its WAL, then
// returns one serialized writer connection.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, errors.New("state db: database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(absolute + suffix); err == nil {
				return nil, errors.New("state db: absent database has a stranded journal")
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, errors.New("state db: database path is not a regular file")
	} else if err := os.Chmod(absolute, 0600); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(absolute + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("state db: database journal is not a regular file")
		}
		if err := os.Chmod(absolute+suffix, 0600); err != nil {
			return nil, err
		}
	}
	database, err := sql.Open("sqlite", URI(absolute, "rw", "foreign_keys(1)"))
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

// OnlineBackup copies a live SQLite connection into a standalone database.
// Keeping the driver-specific hook here lets state-table owners depend only on
// this package's database boundary.
func OnlineBackup(ctx context.Context, connection *sql.Conn, destination string) error {
	if connection == nil || destination == "" {
		return errors.New("state db: connection and backup destination are required")
	}
	return connection.Raw(func(driverConnection any) (copyErr error) {
		source, ok := driverConnection.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("state db: online backup requires the SQLite driver")
		}
		backup, err := source.NewBackup(URI(destination, "rw"))
		if err != nil {
			return err
		}
		defer func() { copyErr = errors.Join(copyErr, backup.Finish()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(128)
			if err != nil || !more {
				return err
			}
		}
	})
}

// URI returns a SQLite file URI with settings applied to every connection,
// including replacements after cancellation. The busy timeout is shared;
// callers supply any other connection-local settings their store requires.
func URI(path, mode string, pragmas ...string) string {
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("mode", mode)
	query.Add("_pragma", "busy_timeout(5000)")
	for _, pragma := range pragmas {
		query.Add("_pragma", pragma)
	}
	u.RawQuery = query.Encode()
	return u.String()
}
