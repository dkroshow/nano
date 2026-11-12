package statestore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

// The live upgrade: a schema-24 store beside attachments.json. The import must
// keep what is true at the moment Nano stopped (a thread still marked ready
// was live, so it is attached; every other record is resumable), finish
// nothing, keep the JSON file byte-for-byte, and read it only once.
func TestPrepareStoreImportsAttachmentsOnceAndFinishesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacySchema(db, testMigrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnRecords(ctx, db, turns.LegacyTurnPaths{}, filepath.Join(dir, "v19")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnTransactions(ctx, db, filepath.Join(dir, "v20.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(ctx, db, filepath.Join(dir, "v21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateMemberRecovery(ctx, db, filepath.Join(dir, "v22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateParkedDiscard(ctx, db, filepath.Join(dir, "v23.sqlite")); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]string{"live": "ready", "rested": "detached", "closed": "exited", "rekeyed": "detached"} {
		if _, err := db.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,revision,created_at,updated_at) VALUES(?,'claude',?,'shared',1,'t','t')`, id, id+"-native"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,request_hash,status,run_id,created_at,updated_at,revision) VALUES(?,'h',?,'old-run','t','t',1)`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// "rekeyed" left its provisional shim-handle record beside the record for
	// its resolved native id; "unknown" has no execution record.
	legacy := []byte(`[
  {"provider":"claude","nativeThreadId":"live-native","hostedThreadId":"live","attachedAt":"2026-11-01T00:00:00Z","bin":"claude","args":["-p","--resume"],"shimSessionId":"shim-live","turnStartedAtMs":7,"turnAnchorSeq":3},
  {"provider":"claude","nativeThreadId":"rested-native","hostedThreadId":"rested","attachedAt":"2026-11-02T00:00:00Z"},
  {"provider":"claude","nativeThreadId":"closed-native","hostedThreadId":"closed","attachedAt":"2026-11-03T00:00:00Z"},
  {"provider":"claude","nativeThreadId":"rekeyed-native","hostedThreadId":"rekeyed","attachedAt":"2026-11-04T00:00:00Z","shimSessionId":"shim-rekeyed"},
  {"provider":"claude","nativeThreadId":"shim-rekeyed","hostedThreadId":"rekeyed","attachedAt":"2026-11-05T00:00:00Z","shimSessionId":"shim-rekeyed","pendingName":"staged"},
  {"provider":"codex","nativeThreadId":"unknown-native","hostedThreadId":"unknown","attachedAt":"2026-11-06T00:00:00Z"}
]`)
	legacyPath := filepath.Join(dir, "attachments.json")
	if err := os.WriteFile(legacyPath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	paths := StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "backups"), Attachments: legacyPath}
	if _, err := PrepareStore(ctx, paths, testMigrations()); err != nil {
		t.Fatal(err)
	}

	type row struct{ Native, State, Bin, Args, Shim, Pending, FinishedAt string }
	read := func() map[string]row {
		t.Helper()
		db, err := OpenReadOnly(ctx, path, testMigrations())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT hosted_thread_id,native_thread_id,state,bin,args_json,shim_session_id,pending_name,finished_at FROM hosted_attachments`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]row{}
		for rows.Next() {
			var id string
			var r row
			if err := rows.Scan(&id, &r.Native, &r.State, &r.Bin, &r.Args, &r.Shim, &r.Pending, &r.FinishedAt); err != nil {
				t.Fatal(err)
			}
			got[id] = r
		}
		return got
	}
	want := map[string]row{
		"live":    {Native: "live-native", State: "attached", Bin: "claude", Args: `["-p","--resume"]`, Shim: "shim-live"},
		"rested":  {Native: "rested-native", State: "resumable", Args: "[]"},
		"closed":  {Native: "closed-native", State: "resumable", Args: "[]"},
		"rekeyed": {Native: "rekeyed-native", State: "resumable", Args: "[]", Shim: "shim-rekeyed"},
		"unknown": {Native: "unknown-native", State: "resumable", Args: "[]"},
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("imported rows = %+v\nwant %+v", got, want)
	}
	if after, err := os.ReadFile(legacyPath); err != nil || !bytes.Equal(after, legacy) {
		t.Fatalf("attachments.json changed by the import: %v", err)
	}

	// Once the store is at schema 25 the file is never read again: a changed
	// file must not change the rows.
	if err := os.WriteFile(legacyPath, []byte(`[{"provider":"codex","nativeThreadId":"x","hostedThreadId":"rested","attachedAt":"2026-11-07T00:00:00Z"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareStore(ctx, paths, testMigrations()); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after a second prepare = %+v\nwant %+v", got, want)
	}
}
