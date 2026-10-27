package threads

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupAndRestorePreserveCommittedWALAndRowIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sqlite")
	db := testDatabase(t, path)
	execSQL(t, db, `CREATE TABLE facts (id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT NOT NULL);
		INSERT INTO facts VALUES(41,'retired alias');
		INSERT INTO facts VALUES(52,'uncertain prompt');
		PRAGMA user_version=19;`)
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture needs committed uncheckpointed WAL: %v", err)
	}
	// The open source stays in WAL mode. A raw copy of its main file loses
	// these records; the online backup must see the committed database.
	backup := filepath.Join(dir, "private backups", "before.sqlite")
	if err := Backup(context.Background(), db, backup); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db, `UPDATE facts SET value='after backup' WHERE id=52`)
	restored := filepath.Join(dir, "restored.sqlite")
	if err := Restore(context.Background(), backup, restored); err != nil {
		t.Fatal(err)
	}
	for _, copyPath := range []string{backup, restored} {
		info, err := os.Stat(copyPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private copy %s: %v, %v", copyPath, info, err)
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if _, err := os.Lstat(copyPath + suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("standalone backup has sidecar %s: %v", suffix, err)
			}
		}
		copyDB := testDatabase(t, copyPath)
		var version, sequence int
		var value string
		if err := copyDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 19 {
			t.Fatalf("schema version %d: %v", version, err)
		}
		if err := copyDB.QueryRow(`SELECT value FROM facts WHERE id=52`).Scan(&value); err != nil || value != "uncertain prompt" {
			t.Fatalf("backup WAL record %q: %v", value, err)
		}
		if err := copyDB.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='facts'`).Scan(&sequence); err != nil || sequence != 52 {
			t.Fatalf("sequence %d: %v", sequence, err)
		}
		var count int
		if err := copyDB.QueryRow(`SELECT count(*) FROM facts`).Scan(&count); err != nil || count != 2 {
			t.Fatalf("record count %d: %v", count, err)
		}
	}
}

func TestCopyRefusesSQLiteSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			db := testDatabase(t, filepath.Join(dir, "source.sqlite"))
			execSQL(t, db, `CREATE TABLE facts(value TEXT); INSERT INTO facts VALUES('source')`)
			backup := filepath.Join(dir, "backup.sqlite")
			if err := Backup(context.Background(), db, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup+suffix, []byte("preserve journal"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Restore(context.Background(), backup, filepath.Join(dir, "restored.sqlite")); err == nil {
				t.Fatal("restore ignored a journal")
			}
			destination := filepath.Join(dir, "other.sqlite")
			if err := os.WriteFile(destination+suffix, []byte("preserve journal"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Backup(context.Background(), db, destination); !errors.Is(err, os.ErrExist) {
				t.Fatalf("backup ignored destination journal: %v", err)
			}
			for _, path := range []string{backup + suffix, destination + suffix} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "preserve journal" {
					t.Fatalf("journal changed: %q, %v", data, err)
				}
			}
		})
	}
}

func TestBackupNeverReplacesAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	db := testDatabase(t, filepath.Join(dir, "source.sqlite"))
	execSQL(t, db, `CREATE TABLE facts(value TEXT); INSERT INTO facts VALUES('source')`)
	for _, kind := range []string{"file", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "existing")
			switch kind {
			case "file":
				if err := os.WriteFile(destination, []byte("keep me"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("missing-target", destination); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(destination, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := Backup(context.Background(), db, destination); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing %s: %v", kind, err)
			}
			if kind == "file" {
				data, err := os.ReadFile(destination)
				if err != nil || string(data) != "keep me" {
					t.Fatalf("destination replaced: %q, %v", data, err)
				}
			}
			if kind == "symlink" {
				target, err := os.Readlink(destination)
				if err != nil || target != "missing-target" {
					t.Fatalf("link replaced: %q, %v", target, err)
				}
			}
		})
	}
}

func TestFailedCopyDoesNotPublishOrChangeSource(t *testing.T) {
	for _, failure := range []string{"cancelled", "closed source", "invalid destination parent", "corrupt source", "missing source", "empty source"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.sqlite")
			destination := filepath.Join(dir, "copy.sqlite")
			var err error
			switch failure {
			case "corrupt source", "empty source", "missing source":
				if failure != "missing source" {
					data := []byte("not a database")
					if failure == "empty source" {
						data = nil
					}
					if err := os.WriteFile(source, data, 0600); err != nil {
						t.Fatal(err)
					}
				}
				err = Restore(context.Background(), source, destination)
			default:
				db := testDatabase(t, source)
				execSQL(t, db, `CREATE TABLE facts(value TEXT); INSERT INTO facts VALUES('before')`)
				ctx := context.Background()
				switch failure {
				case "cancelled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				case "closed source":
					db.Close()
				case "invalid destination parent":
					parent := filepath.Join(dir, "file")
					if err := os.WriteFile(parent, []byte("keep parent"), 0600); err != nil {
						t.Fatal(err)
					}
					destination = filepath.Join(parent, "copy.sqlite")
				}
				err = Backup(ctx, db, destination)
				reopened := testDatabase(t, source)
				var value string
				if err := reopened.QueryRow(`SELECT value FROM facts`).Scan(&value); err != nil || value != "before" {
					t.Fatalf("source changed: %q, %v", value, err)
				}
			}
			if err == nil {
				t.Fatal("failed copy reported success")
			}
			if _, err := os.Lstat(destination); err == nil {
				t.Fatal("failed copy published destination")
			}
			partials, err := filepath.Glob(filepath.Join(dir, ".threads-copy-*"))
			if err != nil || len(partials) != 0 {
				t.Fatalf("copy left scratch files: %v, %v", partials, err)
			}
		})
	}
}

func TestBackupIsASeparateCommittedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sqlite")
	db := testDatabase(t, path)
	execSQL(t, db, `CREATE TABLE facts(value TEXT); INSERT INTO facts VALUES('committed')`)
	writer := testDatabase(t, path)
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE facts SET value='uncommitted'`); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "backup.sqlite")
	if err := Backup(context.Background(), db, destination); err != nil {
		t.Fatal(err)
	}
	copyDB := testDatabase(t, destination)
	var value string
	if err := copyDB.QueryRow(`SELECT value FROM facts`).Scan(&value); err != nil || value != "committed" {
		t.Fatalf("backup included an uncommitted write: %q, %v", value, err)
	}
}

func TestBackupRejectsNilDatabase(t *testing.T) {
	var db *sql.DB
	if err := Backup(context.Background(), db, filepath.Join(t.TempDir(), "backup.sqlite")); err == nil {
		t.Fatal("nil source accepted")
	}
}
