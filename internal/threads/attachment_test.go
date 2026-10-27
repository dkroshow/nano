package threads

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

func testDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	return db
}

func execSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func bindingDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := testDatabase(t, path)
	execSQL(t, db, `CREATE TABLE hosted_threads (
		hosted_thread_id TEXT PRIMARY KEY, attachment_generation INTEGER NOT NULL,
		revision INTEGER NOT NULL, updated_at TEXT NOT NULL);
		CREATE TABLE hosted_attachment_generations (
		hosted_thread_id TEXT NOT NULL, generation INTEGER NOT NULL, bound_at TEXT NOT NULL,
		PRIMARY KEY(hosted_thread_id,generation),
		FOREIGN KEY(hosted_thread_id) REFERENCES hosted_threads(hosted_thread_id));
		INSERT INTO hosted_threads VALUES('thread-a',0,8,'before');`)
	return db
}

func TestAttachmentGenerationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.sqlite")
	db := bindingDatabase(t, path)
	first, err := BeginTransportBinding(context.Background(), db, "thread-a")
	if err != nil || first != (AttachmentBinding{Generation: 1, Revision: 9}) {
		t.Fatalf("first binding: %+v, %v", first, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testDatabase(t, path)
	second, err := BeginTransportBinding(context.Background(), db, "thread-a")
	if err != nil || second != (AttachmentBinding{Generation: 2, Revision: 10}) {
		t.Fatalf("binding after restart: %+v, %v", second, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM hosted_attachment_generations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("committed bindings: %d, %v", count, err)
	}
	var updated, bound string
	if err := db.QueryRow(`SELECT t.updated_at,g.bound_at FROM hosted_threads t JOIN hosted_attachment_generations g ON g.hosted_thread_id=t.hosted_thread_id AND g.generation=t.attachment_generation`).Scan(&updated, &bound); err != nil || updated != bound || updated == "before" {
		t.Fatalf("binding timestamps: %q / %q, %v", updated, bound, err)
	}
}

func TestAttachmentFailureRollsBackCounterAndRevision(t *testing.T) {
	db := bindingDatabase(t, filepath.Join(t.TempDir(), "threads.sqlite"))
	execSQL(t, db, `CREATE TRIGGER reject_binding BEFORE INSERT ON hosted_attachment_generations BEGIN SELECT RAISE(ABORT,'synthetic audit failure'); END;`)
	if got, err := BeginTransportBinding(context.Background(), db, "thread-a"); err == nil || got != (AttachmentBinding{}) {
		t.Fatalf("failed binding exposed: %+v, %v", got, err)
	}
	var generation, revision int64
	var updated string
	if err := db.QueryRow(`SELECT attachment_generation,revision,updated_at FROM hosted_threads`).Scan(&generation, &revision, &updated); err != nil || generation != 0 || revision != 8 || updated != "before" {
		t.Fatalf("failed binding mutated record: %d/%d/%q, %v", generation, revision, updated, err)
	}
	execSQL(t, db, `DROP TRIGGER reject_binding`)
	if got, err := BeginTransportBinding(context.Background(), db, "thread-a"); err != nil || got.Generation != 1 {
		t.Fatalf("retry: %+v, %v", got, err)
	}
}

func TestAttachmentRejectsMissingAndInvalidCounters(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		generation, revision int64
		hostedID             string
	}{
		{"missing", 0, 8, "absent"},
		{"empty", 0, 8, ""},
		{"negative generation", -1, 8, "thread-a"},
		{"exhausted generation", math.MaxInt64, 8, "thread-a"},
		{"negative revision", 0, -1, "thread-a"},
		{"exhausted revision", 0, math.MaxInt64, "thread-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := bindingDatabase(t, filepath.Join(t.TempDir(), "threads.sqlite"))
			execSQL(t, db, `UPDATE hosted_threads SET attachment_generation=?,revision=?`, tc.generation, tc.revision)
			if got, err := BeginTransportBinding(context.Background(), db, tc.hostedID); err == nil || got != (AttachmentBinding{}) {
				t.Fatalf("invalid binding accepted: %+v, %v", got, err)
			}
			var generation, revision int64
			if err := db.QueryRow(`SELECT attachment_generation,revision FROM hosted_threads`).Scan(&generation, &revision); err != nil || generation != tc.generation || revision != tc.revision {
				t.Fatalf("counter changed: %d/%d, %v", generation, revision, err)
			}
		})
	}
}

func TestConcurrentAttachmentsHaveDistinctDurableGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.sqlite")
	one := bindingDatabase(t, path)
	two := testDatabase(t, path)
	const n = 12
	results := make(chan AttachmentBinding, n)
	errors := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := one
			if i%2 == 0 {
				db = two
			}
			binding, err := BeginTransportBinding(context.Background(), db, "thread-a")
			results <- binding
			errors <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int64]bool{}
	for got := range results {
		if got.Generation < 1 || got.Generation > n || got.Revision != got.Generation+8 || seen[got.Generation] {
			t.Fatalf("invalid or duplicate binding: %+v", got)
		}
		seen[got.Generation] = true
	}
}
