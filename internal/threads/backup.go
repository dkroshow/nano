package threads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	statedb "github.com/dkroshow/nano/internal/state/db"
)

// Backup writes a consistent SQLite copy to a new destination. It includes
// committed WAL pages and preserves rowids, schema version, and sequence values.
func Backup(ctx context.Context, db *sql.DB, destination string) (err error) {
	if db == nil {
		return errors.New("threads: database is required")
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, connection.Close()) }()
	return BackupConnection(ctx, connection, destination)
}

// BackupConnection uses the same backup command while its caller reserves the
// store writer connection through backup and a following maintenance transaction.
// The connection must not have an open transaction.
func BackupConnection(ctx context.Context, connection *sql.Conn, destination string) error {
	if connection == nil || destination == "" {
		return errors.New("threads: database and backup destination are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(destination + suffix); err == nil {
			return fmt.Errorf("threads: backup destination %q: %w", destination+suffix, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".threads-copy-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Close(); err != nil {
		return err
	}
	err = statedb.OnlineBackup(ctx, connection, temporary)
	if err != nil {
		return fmt.Errorf("threads: online backup: %w", err)
	}
	copyDB, err := OpenCheckedCopy(ctx, temporary)
	if err != nil {
		return err
	}
	if err := copyDB.Close(); err != nil {
		return err
	}
	file, err = os.OpenFile(temporary, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = errors.Join(file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A hard link publishes the completed file atomically without replacing a
	// destination created by another caller during the copy. Both paths share
	// a directory, so publication cannot cross a filesystem boundary.
	if err := os.Link(temporary, destination); err != nil {
		return fmt.Errorf("threads: publish backup: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// Restore checks a backup and copies it to a new destination. Selecting the
// restored path for service startup is an explicit, separate operator action.
func Restore(ctx context.Context, source, destination string) error {
	db, err := OpenCheckedCopy(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	return Backup(ctx, db, destination)
}

// OpenCheckedCopy opens a complete standalone SQLite backup read-only.
func OpenCheckedCopy(ctx context.Context, path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	// SQLite treats an empty file as a new database. A restore must instead
	// reject a truncated or absent backup before it can become a valid store.
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("threads: backup must be a nonempty regular database")
	}
	// Restore accepts a closed, standalone backup. Immutable reads avoid
	// creating WAL/SHM files beside that backup, including on read-only media.
	// Reject journals rather than silently ignoring potentially newer data.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return nil, fmt.Errorf("threads: backup has SQLite sidecar %q; use an online backup first", path+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", statedb.URI(path, "ro")+"&immutable=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		db.Close()
		return nil, fmt.Errorf("threads: check backup: %w", err)
	}
	if result != "ok" {
		db.Close()
		return nil, fmt.Errorf("threads: backup integrity check: %s", result)
	}
	return db, nil
}
