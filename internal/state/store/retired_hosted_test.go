package statestore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)


func retirementFixture(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := statedb.Open(ctx, filepath.Join(t.TempDir(), "nano_threads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := MigrateLegacySchema(db, testMigrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnRecords(ctx, db, turns.LegacyTurnPaths{}, filepath.Join(t.TempDir(), "migration")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateTurnTransactions(ctx, db, filepath.Join(t.TempDir(), "schema20.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := coreturns.MigrateCompletedTurnUsage(ctx, db, filepath.Join(t.TempDir(), "schema21.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateMemberRecovery(ctx, db, filepath.Join(t.TempDir(), "schema22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := turns.MigrateParkedDiscard(ctx, db, filepath.Join(t.TempDir(), "schema23.sqlite")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "owner", "child", "unrelated"} {
		if _, err := db.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,revision,created_at,updated_at) VALUES(?,'codex',?,'shared',1,'before','before')`, id, id+"-native"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,request_hash,status,run_id,created_at,updated_at,revision) VALUES(?,'hash','exited','test','before','before',1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`UPDATE hosted_threads SET current_native_id='native' WHERE hosted_thread_id IN ('old','owner')`,
		`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,retired_at,first_seen_at,last_seen_at) VALUES('old','codex','native','retired','before','before'),('owner','codex','native',NULL,'before','before')`,
		`UPDATE hosted_threads SET invoker_hosted_id='old',invoker_native_id='native',invoker_provider='codex' WHERE hosted_thread_id='child'`,
		`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,retired_at,first_seen_at,last_seen_at) VALUES('old','codex','old-placeholder','retired','before','before')`,
		`INSERT INTO hosted_attachment_generations VALUES('old',1,'before'),('owner',1,'before')`,
		`INSERT INTO thread_metadata_events(event_id,hosted_thread_id,field,value_json,disposition,source,evidence_freshness,observed_at,committed_at) VALUES(1,'old','cwd','"/old"','requested','test','live','before','before'),(2,'owner','cwd','"/owner"','requested','test','live','before','before')`,
		`INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_disposition,effective_disposition,desired_event_id,thread_revision,updated_at) VALUES('old','cwd','requested','unknown',1,1,'before'),('owner','cwd','requested','unknown',2,1,'before')`,
		`INSERT INTO hosted_turn_operations(operation_id,hosted_thread_id,client_action_id,transport_action_id,prompt_hash,status,run_id,created_at,updated_at,revision,sender_hosted_id,sender_native_id,sender_provider) VALUES('old-op','old','old-action','old-transport','hash','completed','test','before','before',1,NULL,NULL,NULL),('child-op','child','child-action','child-transport','hash','completed','test','before','before',1,'old','native','codex')`,
		`CREATE TABLE unrelated_facts(id INTEGER PRIMARY KEY, value TEXT)`,
		`INSERT INTO unrelated_facts VALUES(1,'preserve exactly')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := turns.SaveQueuedTurns(ctx, db, []turns.QueuedTurn{{ClientActionID: "outbox", Provider: "codex", HostedThreadID: "old", NativeThreadID: "native", Text: "unconfirmed evidence", State: "accepted"}}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRetiredHostedPreviewAndApply(t *testing.T) {
	for _, disposition := range []string{"repoint", "delete"} {
		t.Run(disposition, func(t *testing.T) {
			db := retirementFixture(t)
			ctx := context.Background()
			request := RetirementRequest{OutboxDisposition: disposition}
			preview, err := RetireHostedRecords(ctx, db, request, "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Preview.Counts != (RetirementCounts{Records: 1, Claims: 2, Dependents: 5, Lineage: 2, Outbox: 1}) {
				t.Fatalf("wrong preview: %+v", preview.Preview)
			}
			request.Apply = true
			request.ExpectedFingerprint = preview.Preview.Fingerprint
			request.ExpectedCounts = &preview.Preview.Counts
			receipt, err := RetireHostedRecords(ctx, db, request, filepath.Join(t.TempDir(), "before.sqlite"), nil, func(tx *sql.Tx, targets []RetiredHostedRecord) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !receipt.Applied || receipt.BackupSHA256 == "" || !reflect.DeepEqual(receipt.Preview, preview.Preview) {
				t.Fatalf("wrong receipt: %+v", receipt)
			}
			backup, err := threads.OpenCheckedCopy(ctx, receipt.BackupPath)
			if err != nil {
				t.Fatal(err)
			}
			defer backup.Close()
			var backedUp int
			if err := backup.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id='old'`).Scan(&backedUp); err != nil || backedUp != 1 {
				t.Fatalf("backup lost retired record: %d %v", backedUp, err)
			}
			data, err := os.ReadFile(receipt.BackupPath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			if receipt.BackupSHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("incorrect backup hash")
			}
			if info, err := os.Stat(receipt.BackupPath); err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("backup mode: %v %v", info, err)
			}
			var old int
			if err := db.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id='old'`).Scan(&old); err != nil || old != 0 {
				t.Fatalf("retired record remains: %d %v", old, err)
			}
			var invoker string
			if err := db.QueryRow(`SELECT invoker_hosted_id FROM hosted_threads WHERE hosted_thread_id='child'`).Scan(&invoker); err != nil || invoker != "owner" {
				t.Fatalf("lineage: %s %v", invoker, err)
			}
			queue, err := turns.LoadQueuedTurns(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if disposition == "delete" {
				if len(queue) != 0 {
					t.Fatal("outbox not deleted")
				}
			} else if len(queue) != 1 || queue[0].HostedThreadID != "owner" || queue[0].State != "accepted" || queue[0].Text != "unconfirmed evidence" {
				t.Fatalf("outbox: %+v", queue)
			}
			if receipt.Before["unrelated_facts"] != 1 || receipt.After["unrelated_facts"] != 1 {
				t.Fatal("unrelated facts changed")
			}
		})
	}
}

func retirementDatabaseHash(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scan, err := scanRetirement(context.Background(), tx, nil, "repoint")
	if err != nil {
		t.Fatal(err)
	}
	return scan.actual
}

func TestRetiredHostedRefusesDrift(t *testing.T) {
	for _, test := range []struct {
		name, sql, reason string
		countsOnly        bool
	}{
		{"new-dependent", `INSERT INTO hosted_attachment_generations VALUES('old',2,'after')`, "fingerprint", false},
		{"same-count-different-content", `UPDATE queued_turns SET text='changed evidence'`, "fingerprint", false},
		{"count-guard", `INSERT INTO hosted_attachment_generations VALUES('old',2,'after')`, "counts", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := retirementFixture(t)
			ctx := context.Background()
			preview, err := RetireHostedRecords(ctx, db, RetirementRequest{OutboxDisposition: "repoint"}, "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(test.sql); err != nil {
				t.Fatal(err)
			}
			expected := retirementDatabaseHash(t, db)
			fingerprint := preview.Preview.Fingerprint
			if test.countsOnly {
				current, err := RetireHostedRecords(ctx, db, RetirementRequest{OutboxDisposition: "repoint"}, "", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				fingerprint = current.Preview.Fingerprint
			}
			destination := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
			result, err := RetireHostedRecords(ctx, db, RetirementRequest{Apply: true, OutboxDisposition: "repoint", ExpectedFingerprint: fingerprint, ExpectedCounts: &preview.Preview.Counts}, destination, nil, func(*sql.Tx, []RetiredHostedRecord) error { return nil })
			if err == nil || !strings.Contains(err.Error(), test.reason) || result.Applied {
				t.Fatalf("drift accepted: %+v %v", result, err)
			}
			if !reflect.DeepEqual(expected, retirementDatabaseHash(t, db)) {
				t.Fatal("refused apply changed rows")
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("backup created before rejecting drift: %v", err)
			}
		})
	}
}

func TestRetiredHostedRollsBackEveryChange(t *testing.T) {
	for _, failure := range []string{"late-delete", "trigger-other-value", "trigger-other-count", "trigger-same-table", "resolver", "backup", "runtime-check"} {
		t.Run(failure, func(t *testing.T) {
			db := retirementFixture(t)
			ctx := context.Background()
			var trigger string
			switch failure {
			case "late-delete":
				trigger = `CREATE TRIGGER block_retired BEFORE DELETE ON hosted_threads BEGIN SELECT RAISE(ABORT,'injected late failure'); END`
			case "trigger-other-value":
				trigger = `CREATE TRIGGER corrupt_other AFTER DELETE ON hosted_threads BEGIN UPDATE unrelated_facts SET value='corrupted'; END`
			case "trigger-other-count":
				trigger = `CREATE TRIGGER corrupt_other AFTER DELETE ON hosted_threads BEGIN INSERT INTO unrelated_facts VALUES(2,'corrupted'); END`
			case "trigger-same-table":
				trigger = `CREATE TRIGGER corrupt_other AFTER DELETE ON hosted_threads BEGIN UPDATE hosted_threads SET presentation_mode='background' WHERE hosted_thread_id='owner'; END`
			}
			if trigger != "" {
				if _, err := db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			preview, err := RetireHostedRecords(ctx, db, RetirementRequest{OutboxDisposition: "repoint"}, "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			expected := retirementDatabaseHash(t, db)
			backup := filepath.Join(t.TempDir(), "before.sqlite")
			if failure == "backup" {
				if err := os.WriteFile(backup, []byte("do not overwrite"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			verify := func(tx *sql.Tx, targets []RetiredHostedRecord) error {
				var old int
				if err := tx.QueryRow(`SELECT count(*) FROM hosted_threads WHERE hosted_thread_id='old'`).Scan(&old); err != nil {
					return err
				}
				if old != 0 {
					t.Fatal("resolver ran before deletions")
				}
				if failure == "resolver" {
					return errors.New("injected resolver failure")
				}
				return nil
			}
			check := func(RetirementPreview) error {
				if failure == "runtime-check" {
					return errors.New("runtime attached")
				}
				return nil
			}
			result, err := RetireHostedRecords(ctx, db, RetirementRequest{Apply: true, OutboxDisposition: "repoint", ExpectedFingerprint: preview.Preview.Fingerprint}, backup, check, verify)
			if err == nil || result.Applied || result.After != nil {
				t.Fatalf("failure committed or claimed success: %+v %v", result, err)
			}
			if !reflect.DeepEqual(expected, retirementDatabaseHash(t, db)) {
				t.Fatal("failure did not roll back every table")
			}
		})
	}
}

func TestRetiredHostedRefusesUnsafeReferences(t *testing.T) {
	for _, test := range []struct{ name, sql, reason string }{
		{"unretired-claim", `INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,first_seen_at,last_seen_at) VALUES('old','codex','another-native','before','before')`, "unretired claim"},
		{"unknown-reference", `INSERT INTO unrelated_facts VALUES(2,'old')`, "unexpected retired hosted reference"},
		{"outbox-native", `UPDATE queued_turns SET native_thread_id='different'`, "outbox does not name"},
		{"lineage-native", `UPDATE hosted_threads SET invoker_native_id='different' WHERE hosted_thread_id='child'`, "conflicting lineage"},
		{"owner-changed-conversation", `UPDATE hosted_threads SET current_native_id='different' WHERE hosted_thread_id='owner'`, "surviving owner"},
		{"provisional-owner", `UPDATE hosted_thread_native_ids SET provisional=1 WHERE hosted_thread_id='owner'`, "surviving owner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := retirementFixture(t)
			if _, err := db.Exec(test.sql); err != nil {
				t.Fatal(err)
			}
			before := retirementDatabaseHash(t, db)
			_, err := RetireHostedRecords(context.Background(), db, RetirementRequest{OutboxDisposition: "repoint"}, "", nil, nil)
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("unsafe preview: %v", err)
			}
			if !reflect.DeepEqual(before, retirementDatabaseHash(t, db)) {
				t.Fatal("preview changed data")
			}
		})
	}
}

func TestRetiredHostedPreviewsLineageEvenWhenItsRowWillBeDeleted(t *testing.T) {
	db := retirementFixture(t)
	if _, err := db.Exec(`UPDATE hosted_threads SET invoker_hosted_id='old' WHERE hosted_thread_id='old'; UPDATE hosted_turn_operations SET sender_hosted_id='old',sender_native_id='native',sender_provider='codex' WHERE operation_id='old-op'`); err != nil {
		t.Fatal(err)
	}
	request := RetirementRequest{OutboxDisposition: "repoint"}
	preview, err := RetireHostedRecords(context.Background(), db, request, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Preview.Counts.Lineage != 4 {
		t.Fatalf("preview omitted references inside deleted rows: %+v", preview.Preview.Lineage)
	}
	request.Apply = true
	request.ExpectedFingerprint = preview.Preview.Fingerprint
	if receipt, err := RetireHostedRecords(context.Background(), db, request, filepath.Join(t.TempDir(), "before.sqlite"), nil, func(*sql.Tx, []RetiredHostedRecord) error { return nil }); err != nil || !receipt.Applied {
		t.Fatalf("apply: %+v %v", receipt, err)
	}
}

func TestRetiredHostedMutationIsInvisibleUntilCommit(t *testing.T) {
	db := retirementFixture(t)
	ctx := context.Background()
	var seq int
	var name, path string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	outside, err := sql.Open("sqlite", statedb.URI(path, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	preview, err := RetireHostedRecords(ctx, db, RetirementRequest{OutboxDisposition: "delete"}, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	proofRan := false
	verify := func(tx *sql.Tx, targets []RetiredHostedRecord) error {
		proofRan = true
		for _, table := range retirementDeleteTables {
			expected := len(preview.Preview.DeleteRows[table])
			if expected == 0 {
				continue
			}
			var inside, committed int
			query := `SELECT count(*) FROM ` + quoteRetirementSQL(table) + ` WHERE hosted_thread_id='old'`
			if err := tx.QueryRow(query).Scan(&inside); err != nil {
				return err
			}
			if err := outside.QueryRow(query).Scan(&committed); err != nil {
				return err
			}
			if inside != 0 || committed != expected {
				return fmt.Errorf("%s leaked uncommitted removal: inside=%d outside=%d", table, inside, committed)
			}
		}
		var outbox int
		if err := outside.QueryRow(`SELECT count(*) FROM queued_turns`).Scan(&outbox); err != nil {
			return err
		}
		if outbox != 1 {
			return errors.New("outbox deletion visible before commit")
		}
		var invoker string
		if err := outside.QueryRow(`SELECT invoker_hosted_id FROM hosted_threads WHERE hosted_thread_id='child'`).Scan(&invoker); err != nil {
			return err
		}
		if invoker != "old" {
			return errors.New("lineage update visible before commit")
		}
		return nil
	}
	result, err := RetireHostedRecords(ctx, db, RetirementRequest{Apply: true, OutboxDisposition: "delete", ExpectedFingerprint: preview.Preview.Fingerprint}, filepath.Join(t.TempDir(), "before.sqlite"), nil, verify)
	if err != nil || !proofRan || !result.Applied {
		t.Fatalf("transaction isolation: %+v %v", result, err)
	}
}
