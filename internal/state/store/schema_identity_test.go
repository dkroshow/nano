package statestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"testing"

	statedb "github.com/dkroshow/nano/internal/state/db"
)


const statestoreFreshSchemaHash = "e3a50e8d0705ad87203b221adfbed3a279a79d5b4aeacbd282088bbd78f1612b"

func TestFreshStatestoreSchemaIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nano_threads.sqlite")
	if _, err := PrepareStore(context.Background(), StorePaths{Destination: path, BackupRoot: filepath.Join(t.TempDir(), "backups")}, testMigrations()); err != nil {
		t.Fatal(err)
	}
	database, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if got := schemaHash(t, database); got != statestoreFreshSchemaHash {
		t.Fatalf("fresh statestore schema hash = %s, want %s", got, statestoreFreshSchemaHash)
	}
}

func TestCurrentStoreReopensWithoutMigration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nano_threads.sqlite")
	paths := StorePaths{Destination: path, BackupRoot: filepath.Join(directory, "backups")}
	if _, err := PrepareStore(context.Background(), paths, testMigrations()); err != nil {
		t.Fatal(err)
	}
	database, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	wantSchema := schemaHash(t, database)
	if _, err := database.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,presentation_mode,revision,created_at,updated_at) VALUES('fixture','codex','shared',1,'before','before')`); err != nil {
		t.Fatal(err)
	}
	var wantReceipts int
	if err := database.QueryRow(`SELECT count(*) FROM threads_store_migrations`).Scan(&wantReceipts); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareStore(context.Background(), paths, testMigrations()); err != nil {
		t.Fatal(err)
	}
	database, err = statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var gotReceipts int
	if err := database.QueryRow(`SELECT count(*) FROM threads_store_migrations`).Scan(&gotReceipts); err != nil {
		t.Fatal(err)
	}
	var fixtureRows int
	if err := database.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id='fixture'`).Scan(&fixtureRows); err != nil {
		t.Fatal(err)
	}
	if got := schemaHash(t, database); got != wantSchema || gotReceipts != wantReceipts || fixtureRows != 1 {
		t.Fatalf("reopen changed schema or receipts: schema=%s want=%s receipts=%d want=%d", got, wantSchema, gotReceipts, wantReceipts)
	}
}

func schemaHash(t *testing.T, database *sql.DB) string {
	t.Helper()
	rows, err := database.Query(`SELECT type,name,tbl_name,sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	digest := sha256.New()
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(digest, "%s|%s|%s|%s\n", kind, name, table, statement)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
