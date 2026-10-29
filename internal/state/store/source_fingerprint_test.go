package statestore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

func TestSourceFingerprintsIgnoreObserverWritesAndSQLiteCheckpoints(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	paths := StorePaths{Destination: filepath.Join(dir, "nano_threads.sqlite"), Control: filepath.Join(dir, "control.sqlite"), BackupRoot: filepath.Join(dir, "backups"), Turns: turns.LegacyTurnPaths{Completed: filepath.Join(dir, "turn-completed.sqlite")}}
	control, err := statedb.Open(ctx, paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if err := MigrateLegacySchema(control, testMigrations()); err != nil {
		t.Fatal(err)
	}
	completed, err := statedb.Open(ctx, paths.Turns.Completed)
	if err != nil {
		t.Fatal(err)
	}
	defer completed.Close()
	if _, err := completed.Exec(turns.CompletedTurnSchema + `; CREATE TABLE observer_fixture (id TEXT PRIMARY KEY); INSERT INTO completed_turns VALUES(7,'owner','codex','turn','final','time','provider','time')`); err != nil {
		t.Fatal(err)
	}
	before, err := fingerprintSources(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completed.Exec(`INSERT INTO observer_fixture VALUES ('changed observer')`); err != nil {
		t.Fatal(err)
	}
	for _, db := range []*sql.DB{control, completed} {
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
	}
	after, err := fingerprintSources(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("observer write or checkpoint changed source fingerprints: before=%+v after=%+v", before, after)
	}
	if _, err := completed.Exec(`UPDATE sqlite_sequence SET seq=8 WHERE name='completed_turns'`); err != nil {
		t.Fatal(err)
	}
	changed, err := fingerprintSources(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(changed, before) {
		t.Fatal("changed completion cursor kept the same source fingerprint")
	}
}

func TestFreshLegacyCopyReverifiesChangedSourcesBeforeLaterSchemas(t *testing.T) {
	for _, mode := range []string{"new-journal", "malformed-journal", "new-control", "new-completion", "missing-fingerprints", "observer-only"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			paths := StorePaths{Destination: filepath.Join(dir, "nano_threads.sqlite"), Control: filepath.Join(dir, "control.sqlite"), BackupRoot: filepath.Join(dir, "backups"), Turns: turns.LegacyTurnPaths{Queue: filepath.Join(dir, "queued-turns.json"), Completed: filepath.Join(dir, "turn-completed.sqlite")}}
			checks := 0
			_, err := prepareStore(ctx, paths, func(ctx context.Context, candidate *sql.DB, paths StorePaths) error {
				checks++
				switch mode {
				case "new-journal":
					if err := os.WriteFile(paths.Turns.Queue, []byte("[]"), 0600); err != nil {
						return err
					}
				case "malformed-journal":
					if err := os.WriteFile(paths.Turns.Queue, []byte("false\n"), 0600); err != nil {
						return err
					}
				case "new-control":
					db, err := statedb.Open(ctx, paths.Control)
					if err != nil {
						return err
					}
					if err := MigrateLegacySchema(db, testMigrations()); err != nil {
						db.Close()
						return err
					}
					if err := db.Close(); err != nil {
						return err
					}
				case "new-completion":
					db, err := statedb.Open(ctx, paths.Turns.Completed)
					if err != nil {
						return err
					}
					if _, err := db.Exec(turns.CompletedTurnSchema + `;
						INSERT INTO completed_turns(id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at)
						VALUES(1,'hosted','codex','turn','final','time','provider','time')`); err != nil {
						db.Close()
						return err
					}
					if err := db.Close(); err != nil {
						return err
					}
				case "missing-fingerprints":
					if _, err := candidate.Exec(`UPDATE threads_store_migrations SET manifest_json='{}'`); err != nil {
						return err
					}
				case "observer-only":
					db, err := statedb.Open(ctx, paths.Turns.Completed)
					if err != nil {
						return err
					}
					if _, err := db.Exec(`CREATE TABLE observer_fixture (id TEXT PRIMARY KEY); INSERT INTO observer_fixture VALUES ('new observer')`); err != nil {
						db.Close()
						return err
					}
					if err := db.Close(); err != nil {
						return err
					}
				}
				return verifySourceFingerprints(ctx, candidate, paths)
			}, testMigrations())
			if checks != 1 {
				t.Fatalf("fresh legacy copy checked sources %d times", checks)
			}
			if mode == "observer-only" {
				if err != nil {
					t.Fatalf("observer-only source change rejected: %v", err)
				}
			} else if !errors.Is(err, ErrLegacySourceChanged) {
				t.Fatalf("changed source accepted during fresh copy: %v", err)
			}
			if mode != "observer-only" {
				if _, statErr := os.Stat(paths.Destination); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed fresh copy published destination: %v", statErr)
				}
			}
		})
	}
}
