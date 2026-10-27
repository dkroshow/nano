package threads

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	statedb "github.com/dkroshow/nano/internal/state/db"
)

func creationNameRepairFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := statedb.Open(context.Background(), filepath.Join(t.TempDir(), "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE hosted_threads (
		hosted_thread_id TEXT PRIMARY KEY, provider TEXT NOT NULL, current_native_id TEXT,
		presentation_mode TEXT NOT NULL, attachment_generation INTEGER NOT NULL DEFAULT 0,
		revision INTEGER NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
		CREATE TABLE hosted_thread_native_ids (
		hosted_thread_id TEXT NOT NULL, provider TEXT NOT NULL, native_thread_id TEXT NOT NULL,
		provisional INTEGER NOT NULL DEFAULT 0, retired_at TEXT, first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL, PRIMARY KEY(hosted_thread_id,provider,native_thread_id));
		CREATE TABLE thread_metadata_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT, hosted_thread_id TEXT NOT NULL, field TEXT NOT NULL,
		value_json TEXT, disposition TEXT NOT NULL, source TEXT NOT NULL, evidence TEXT,
		native_thread_id TEXT, nano_session_id TEXT, attachment_generation INTEGER,
		evidence_freshness TEXT NOT NULL, causation_event_id INTEGER, source_event_key TEXT,
		observed_at TEXT NOT NULL, committed_at TEXT NOT NULL);
		CREATE TABLE thread_metadata_current (
		hosted_thread_id TEXT NOT NULL, field TEXT NOT NULL, desired_value_json TEXT,
		desired_disposition TEXT NOT NULL, desired_source TEXT, desired_event_id INTEGER,
		desired_settlement_event_id INTEGER, effective_value_json TEXT,
		effective_disposition TEXT NOT NULL, effective_source TEXT, effective_event_id INTEGER,
		thread_revision INTEGER NOT NULL, updated_at TEXT NOT NULL,
		PRIMARY KEY(hosted_thread_id,field));`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if _, err := db.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,revision,created_at,updated_at) VALUES(?,'codex',?,'shared',1,'before','before')`, id, "native-"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,first_seen_at,last_seen_at) VALUES(?,'codex',?,0,'before','before')`, id, "native-"+id); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestCreationNameRepairPreviewsBacksUpAndAppliesOneTransaction(t *testing.T) {
	db := creationNameRepairFixture(t)
	request := CreationNameRepairRequest{Records: []CreationNameRepairRecord{
		{HostedThreadID: "one", Provider: "codex", NativeThreadID: "native-one", Name: "first", Source: "creation_request"},
	}, Unrecoverable: []CreationNameUnrecoverableRecord{
		{HostedThreadID: "two", Provider: "codex", NativeThreadID: "native-two", Reason: "no creation record"},
	}}
	preview, err := RepairCreationNames(context.Background(), db, request, "")
	if err != nil || preview.Applied || preview.Preview.Fingerprint == "" || len(preview.Preview.Records) != 1 || len(preview.Preview.Unrecoverable) != 1 || preview.Preview.Unrecoverable[0].State != "no_recoverable_name" {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if _, ok, err := ReadCreationName(context.Background(), db, "one"); err != nil || ok {
		t.Fatalf("preview mutated store: ok=%v err=%v", ok, err)
	}
	request.Apply = true
	request.ExpectedFingerprint = preview.Preview.Fingerprint
	receipt, err := RepairCreationNames(context.Background(), db, request, filepath.Join(t.TempDir(), "before.sqlite"))
	if err != nil || !receipt.Applied || receipt.BackupSHA256 == "" || receipt.BackupPath == "" {
		t.Fatalf("apply = %+v, %v", receipt, err)
	}
	for id, want := range map[string]string{"one": "first"} {
		if got, ok, err := ReadCreationName(context.Background(), db, id); err != nil || !ok || got != want {
			t.Fatalf("%s name = %q, %v, %v", id, got, ok, err)
		}
	}
	if _, ok, err := ReadCreationName(context.Background(), db, "two"); err != nil || ok {
		t.Fatalf("unrecoverable record was mutated: ok=%v err=%v", ok, err)
	}
}

func TestCreationNameRepairRefusesDriftAndRollsBackEveryName(t *testing.T) {
	for _, mode := range []string{"drift", "late failure"} {
		t.Run(mode, func(t *testing.T) {
			db := creationNameRepairFixture(t)
			request := CreationNameRepairRequest{Records: []CreationNameRepairRecord{
				{HostedThreadID: "one", Provider: "codex", NativeThreadID: "native-one", Name: "first", Source: "creation_request"},
				{HostedThreadID: "two", Provider: "codex", NativeThreadID: "native-two", Name: "second", Source: "creation_request"},
			}}
			preview, err := RepairCreationNames(context.Background(), db, request, "")
			if err != nil {
				t.Fatal(err)
			}
			request.Apply = true
			request.ExpectedFingerprint = preview.Preview.Fingerprint
			if mode == "drift" {
				if _, err := db.Exec(`UPDATE hosted_threads SET current_native_id='changed' WHERE hosted_thread_id='two'`); err != nil {
					t.Fatal(err)
				}
			} else if _, err := db.Exec(`CREATE TRIGGER fail_second_creation_name BEFORE INSERT ON thread_metadata_events WHEN NEW.hosted_thread_id='two' AND NEW.field='creationName' BEGIN SELECT RAISE(ABORT,'synthetic late failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := RepairCreationNames(context.Background(), db, request, filepath.Join(t.TempDir(), "before.sqlite")); err == nil {
				t.Fatal("unsafe apply succeeded")
			}
			for _, id := range []string{"one", "two"} {
				if _, ok, err := ReadCreationName(context.Background(), db, id); err != nil || ok {
					t.Fatalf("failed apply persisted %s: ok=%v err=%v", id, ok, err)
				}
			}
		})
	}
}

func TestCreationNameRepairRefusesUnreviewableSource(t *testing.T) {
	db := creationNameRepairFixture(t)
	_, err := RepairCreationNames(context.Background(), db, CreationNameRepairRequest{Records: []CreationNameRepairRecord{{
		HostedThreadID: "one", Provider: "codex", NativeThreadID: "native-one", Name: "first", Source: "guess",
	}}}, "")
	if err == nil {
		t.Fatal("repair accepted an unsupported evidence source")
	}
}

func TestCreationNameRepairRefusesProvisionalIdentity(t *testing.T) {
	db := creationNameRepairFixture(t)
	if _, err := db.Exec(`UPDATE hosted_thread_native_ids SET provisional=1 WHERE hosted_thread_id='one'`); err != nil {
		t.Fatal(err)
	}
	_, err := RepairCreationNames(context.Background(), db, CreationNameRepairRequest{Records: []CreationNameRepairRecord{{
		HostedThreadID: "one", Provider: "codex", NativeThreadID: "native-one", Name: "first", Source: "creation_request",
	}}}, "")
	if err == nil {
		t.Fatal("repair accepted a provisional provider-native identity")
	}
}
