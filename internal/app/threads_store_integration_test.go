package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/threads"
)

func TestThreadsApplicationBindingRejectsInvalidDurableCounters(t *testing.T) {
	for _, generation := range []int64{-1, math.MaxInt64} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			store, err := openControlMetadataStore(filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.EnsureHostedThread("thread-a", "codex", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CanonicalDatabase().Exec(`UPDATE hosted_threads SET attachment_generation=?`, generation); err != nil {
				t.Fatal(err)
			}
			before := threadsDatabaseSnapshot(t, store.CanonicalDatabase())
			if got, err := store.BeginTransportBinding("thread-a"); err == nil || got != 0 {
				t.Fatalf("application accepted invalid durable epoch: %d, %v", got, err)
			}
			if after := threadsDatabaseSnapshot(t, store.CanonicalDatabase()); !reflect.DeepEqual(after, before) {
				t.Fatal("refused attachment changed canonical facts")
			}
		})
	}
}

func TestThreadsPrimitivesUseCanonicalAttachmentCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	store, err := openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.EnsureHostedThread("thread-a", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	old, err := store.BeginTransportBinding("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openControlMetadataStore(path)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := threads.BeginTransportBinding(context.Background(), store.CanonicalDatabase(), "thread-a")
	if err != nil || bound.Generation != old+1 {
		t.Fatalf("restarted binding: %+v, %v", bound, err)
	}
	current, err := lifecycle.New(uint64(bound.Generation))
	if err != nil {
		t.Fatal(err)
	}
	late, err := lifecycle.Transition(current, lifecycle.Observation{Event: lifecycle.EventReadyWorking, Epoch: uint64(old), Sequence: 999})
	if err != nil || late.Snapshot != current || late.Drain {
		t.Fatalf("old provider evidence crossed durable fence: %+v, %v", late, err)
	}
	view, err := store.View("thread-a")
	if err != nil || view.AttachmentGeneration != bound.Generation || view.Revision != bound.Revision {
		t.Fatalf("canonical view disagrees with binding: %+v, %v", view, err)
	}
}

func TestThreadsBackupAllowsControlMigrationOnDisposableCopy(t *testing.T) {
	dir := t.TempDir()
	source, err := openControlMetadataStore(filepath.Join(dir, "synthetic-control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.EnsureHostedThread("thread-a", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.ClaimNativeID("thread-a", "claude", "settled-a", false); err != nil {
		t.Fatal(err)
	}
	// A provisional handle retired before claims recorded resolutions, as
	// existing stores hold; a claim can no longer retire one without its pair.
	if _, err := source.CanonicalDatabase().Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,retired_at,first_seen_at,last_seen_at)
		VALUES('thread-a','claude','provisional-a',1,'2026-10-01T00:00:01Z','2026-10-01T00:00:00Z','2026-10-01T00:00:01Z')`); err != nil {
		t.Fatal(err)
	}
	if err := source.EnsureHostedThread("thread-b", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := source.ClaimNativeID("thread-b", "claude", "provisional-b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := source.BeginTransportBinding("thread-a"); err != nil {
		t.Fatal(err)
	}
	// Reproduce the existing version-17 fixture in this disposable database.
	// Its migration path is still owned by control until the store cutover.
	if _, err := source.CanonicalDatabase().Exec(`DROP TABLE browser_duplicate_incidents; PRAGMA user_version=17; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	before := threadsDatabaseSnapshot(t, source.CanonicalDatabase())
	backupPath := filepath.Join(dir, "before-migration.sqlite")
	if err := threads.Backup(context.Background(), source.CanonicalDatabase(), backupPath); err != nil {
		t.Fatal(err)
	}
	backupDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := threadsDatabaseSnapshot(t, backupDB); !reflect.DeepEqual(got, before) {
		t.Fatal("backup changed schema, row identities, or field values")
	}
	if err := backupDB.Close(); err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(dir, "migration-copy.sqlite")
	if err := threads.Restore(context.Background(), backupPath, candidatePath); err != nil {
		t.Fatal(err)
	}
	candidate, err := openControlMetadataStore(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := candidate.CanonicalDatabase().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != controlSchemaVersion {
		t.Fatalf("migrated version %d, %v", version, err)
	}
	first := threadsDatabaseSnapshot(t, candidate.CanonicalDatabase())
	for table, rows := range before {
		if table == "schema" || table == "version" {
			continue
		}
		if !reflect.DeepEqual(first[table], rows) {
			t.Fatalf("migration changed retained rows in %s", table)
		}
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	candidate, err = openControlMetadataStore(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if got := threadsDatabaseSnapshot(t, candidate.CanonicalDatabase()); !reflect.DeepEqual(got, first) {
		t.Fatal("migration rerun changed records")
	}
	if got := threadsDatabaseSnapshot(t, source.CanonicalDatabase()); !reflect.DeepEqual(got, before) {
		t.Fatal("copy migration changed source")
	}
	backupDB, err = sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backupDB.Close()
	if got := threadsDatabaseSnapshot(t, backupDB); !reflect.DeepEqual(got, before) {
		t.Fatal("copy migration changed backup")
	}
}

// Compare every column and row, including sqlite_sequence. Only row ordering is
// normalized; nil, numeric, text and blob values remain distinguishable.
func threadsDatabaseSnapshot(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	out["version"] = []string{fmt.Sprint(version)}
	rows, err := db.Query(`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var kind, name, table string
		var statement sql.NullString
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal([]any{kind, name, table, statement})
		if err != nil {
			t.Fatal(err)
		}
		out["schema"] = append(out["schema"], string(encoded))
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		out[table] = []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			cells := make([]any, len(values))
			for i, value := range values {
				cells[i] = []any{fmt.Sprintf("%T", value), value}
			}
			encoded, err := json.Marshal(cells)
			if err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], string(encoded))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(out[table])
	}
	return out
}
