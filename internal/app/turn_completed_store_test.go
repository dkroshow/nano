package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
)

func recordTestCompletedTurn(store *completedTurnStore, finish coreturns.Finish) (completedTurn, bool, error) {
	core, err := store.coreStore(context.Background())
	if err != nil {
		return completedTurn{}, false, err
	}
	return core.Finished(context.Background(), finish)
}

func mustCompletedStore(t *testing.T) *completedTurnStore {
	t.Helper()
	directory := t.TempDir()
	stores, err := openThreadRuntimeStores(context.Background(), directory, filepath.Join(directory, "legacy-completed.sqlite"), "completed-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.control.Close() })
	return stores.completed
}

func TestCompletedTurnStoreOpenerConfiguresSQLite(t *testing.T) {
	s := mustCompletedStore(t)

	var journalMode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	var busyTimeout int
	if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}
}

func TestCompletedTurnStoreCommitWithExternalReaderDoesNotWedge(t *testing.T) {
	s := mustCompletedStore(t)
	path := filepath.Join(t.TempDir(), "unused")
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recordTestCompletedTurn(s, coreturns.Finish{HostedThreadID: "target", Provider: "codex", TurnID: "seed", FinalMessageID: "final-seed", Outcome: "completed", At: time.Now(), Source: "test"}); err != nil {
		t.Fatal(err)
	}

	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	reader.SetMaxOpenConns(1)
	rows, err := reader.Query(`SELECT id FROM completed_turns`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("external reader returned no seed row")
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		t.Fatal(err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO completed_turns(target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at) VALUES(?,?,?,?,?,?,?)`, "target", "codex", "after-reader", "final-after-reader", time.Now().UTC().Format(time.RFC3339Nano), "test", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit with held external reader: %v", err)
	}

	if _, _, err := recordTestCompletedTurn(s, coreturns.Finish{HostedThreadID: "target", Provider: "codex", TurnID: "after-commit", FinalMessageID: "final-after-commit", Outcome: "completed", At: time.Now(), Source: "test"}); err != nil {
		t.Fatalf("store wedged after commit: %v", err)
	}
}

func TestCompletedTurnStoreWaitsForExternalWriter(t *testing.T) {
	s := mustCompletedStore(t)
	path := ""
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &path); err != nil {
		t.Fatal(err)
	}

	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_, err := conn.ExecContext(ctx, `COMMIT`)
		released <- err
	}()

	started := time.Now()
	_, _, err = recordTestCompletedTurn(s, coreturns.Finish{HostedThreadID: "target", Provider: "codex", TurnID: "contended", FinalMessageID: "final-contended", Outcome: "completed", At: time.Now(), Source: "test"})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("record during external writer contention: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("release external writer: %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("record returned before external writer released: %s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("record waited too long for external writer: %s", elapsed)
	}
	if _, err := s.latest("target"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "busy") || strings.Contains(strings.ToLower(err.Error()), "locked") {
			t.Fatalf("store remained locked after contention: %v", err)
		}
		t.Fatal(err)
	}
}

func TestCompletedTurnStoreOrdersAndDeduplicates(t *testing.T) {
	s := mustCompletedStore(t)
	now := time.Now()
	if _, new, e := recordTestCompletedTurn(s, coreturns.Finish{HostedThreadID: "nht", Provider: "codex", TurnID: "one", FinalMessageID: "f1", Outcome: "completed", At: now, Source: "test"}); e != nil || !new {
		t.Fatal(e)
	}
	if _, new, e := recordTestCompletedTurn(s, coreturns.Finish{HostedThreadID: "nht", Provider: "codex", TurnID: "two", FinalMessageID: "f2", Outcome: "completed", At: now, Source: "test"}); e != nil || !new {
		t.Fatal(e)
	}
	rows, e := s.after("nht", "one", 10)
	if e != nil || len(rows) != 1 || rows[0].TurnID != "two" {
		t.Fatalf("%#v %v", rows, e)
	}
	rows, e = s.after("nht", "", 10)
	if e != nil || len(rows) != 2 || rows[0].TurnID != "one" || rows[1].TurnID != "two" {
		t.Fatalf("empty bookmark = %#v %v", rows, e)
	}
}
