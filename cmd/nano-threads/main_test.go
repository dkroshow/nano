package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	"github.com/dkroshow/nano/internal/history"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestBackupRestoreCommandPreservesPrivateCanonicalFacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	if _, err := state.PrepareStore(ctx, state.StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "migration")}, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	text := "private unconfirmed prompt"
	queue := []turns.QueuedTurn{{ClientActionID: "action", Provider: "codex", HostedThreadID: "owner", Text: text, State: "uncertain"}}
	if err := turns.SaveQueuedTurns(ctx, db, queue); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", "snapshot.sqlite")
	var output bytes.Buffer
	if err := run(ctx, []string{"backup", "--database", path, "--out", backup}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), text) {
		t.Fatal("command output leaked prompt text")
	}
	original, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveQueuedTurns(ctx, db, nil); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.sqlite")
	output.Reset()
	if err := run(ctx, []string{"restore", "--backup", backup, "--out", restored}, &output); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{backup, restored} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v %v", file, info, err)
		}
	}
	copyDB, err := state.OpenReadOnly(ctx, restored, statewiring.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	got, err := turns.LoadQueuedTurns(ctx, copyDB)
	if err != nil || !reflect.DeepEqual(got, queue) {
		t.Fatalf("restored facts = %+v %v", got, err)
	}
	if err := run(ctx, []string{"restore", "--backup", backup, "--out", restored}, &output); err == nil {
		t.Fatal("restore replaced an existing database")
	}
	if err := run(ctx, []string{"backup", "--database", path, "--out", backup}, &output); err == nil {
		t.Fatal("backup replaced a retained copy")
	}
	if now, err := os.ReadFile(backup); err != nil || !bytes.Equal(now, original) {
		t.Fatal("original backup changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(backup + suffix); !os.IsNotExist(err) {
			t.Fatal("restore created a sidecar at the backup")
		}
	}
}

func TestPromptsCleanupDryRunAndApply(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	if _, err := state.PrepareStore(ctx, state.StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "migration")}, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	promptStore, err := turns.OpenStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, submission := range []turns.Submission{
		{ActionID: "with-turn", Provider: model.ProviderCodex, HostedThreadID: "host", NativeThreadID: "native", ProviderText: "secret one", PresentationText: "secret one", TextHash: "hash-1", RequestHash: "request-1", CreatedAt: "2026-01-01T00:00:00Z"},
		{ActionID: "without-turn", Provider: model.ProviderClaude, HostedThreadID: "host", NativeThreadID: "native", ProviderText: "secret two", PresentationText: "secret two", TextHash: "hash-2", RequestHash: "request-2", CreatedAt: "2026-01-02T00:00:00Z"},
	} {
		if _, _, err := promptStore.Submit(ctx, submission); err != nil {
			t.Fatal(err)
		}
	}
	if _, claimed, err := promptStore.Claim(ctx, turns.ClaimRequest{HostedThreadID: "host", Token: "claim-1", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	native, turnID := "native", "turn-1"
	if _, err := promptStore.Accepted(ctx, turns.Acceptance{ActionID: "with-turn", ClaimToken: "claim-1", AttachmentEpoch: 1, Binding: turns.Binding{Scope: model.Scope{Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turnID}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE queued_turns SET state='confirmed' WHERE client_action_id='with-turn'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE send_attempts SET status='confirmed' WHERE client_action_id='with-turn'`); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := promptStore.Claim(ctx, turns.ClaimRequest{HostedThreadID: "host", Token: "claim-2", AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
		t.Fatalf("second claim=%v err=%v", claimed, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE queued_turns SET state='accepted' WHERE client_action_id='without-turn'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE send_attempts SET status='accepted' WHERE client_action_id='without-turn'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(ctx, []string{"prompts-cleanup", "--db", path, "--dry-run"}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "secret") {
		t.Fatal("cleanup report leaked prompt text")
	}
	check, err := state.OpenReadOnly(ctx, path, statewiring.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := turns.LoadQueuedTurns(ctx, check)
	_ = check.Close()
	if err != nil || rows[0].State != "confirmed" || rows[1].State != "accepted" {
		t.Fatalf("dry-run changed rows: %+v err=%v", rows, err)
	}
	output.Reset()
	if err := run(ctx, []string{"prompts-cleanup", "--db", path}, &output); err != nil {
		t.Fatal(err)
	}
	check, err = state.OpenReadOnly(ctx, path, statewiring.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	rows, err = turns.LoadQueuedTurns(ctx, check)
	// A confirmed prompt is settled by its saved transcript. Demoting it would
	// make every later attach publish it again, so cleanup must leave it alone.
	if err != nil || rows[0].State != "confirmed" || rows[1].State != "uncertain" {
		t.Fatalf("cleanup rows: %+v err=%v", rows, err)
	}
	var retainedClaims int
	if err := check.QueryRowContext(ctx, `SELECT count(*) FROM queued_turns WHERE state<>'confirmed' AND (claim_token<>'' OR claim_epoch<>0)`).Scan(&retainedClaims); err != nil {
		t.Fatal(err)
	}
	if retainedClaims != 0 {
		t.Fatalf("cleanup retained %d claims", retainedClaims)
	}
	if err := check.QueryRowContext(ctx, `SELECT count(*) FROM send_attempts a JOIN queued_turns q ON q.client_action_id=a.client_action_id WHERE q.state<>'confirmed' AND a.claim_token<>''`).Scan(&retainedClaims); err != nil {
		t.Fatal(err)
	}
	if retainedClaims != 0 {
		t.Fatalf("cleanup retained %d journal claims", retainedClaims)
	}
	var guard int
	if err := check.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='queued_turns_managed_update'`).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if guard != 1 {
		t.Fatal("cleanup removed the managed update guard")
	}
}

func TestSizeCommandIncludesWALAndRefusesUnsupportedSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	if _, err := state.PrepareStore(ctx, state.StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "backups")}, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveQueuedTurns(ctx, db, []turns.QueuedTurn{{ClientActionID: "a", Text: strings.Repeat("x", 128*1024)}}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(ctx, []string{"size", "--database", path}, &output); err != nil {
		t.Fatal(err)
	}
	var size state.SizeReport
	if err := json.Unmarshal(output.Bytes(), &size); err != nil {
		t.Fatal(err)
	}
	if size.QueuedTurns != 1 || size.WALBytes < 128*1024 || size.TotalBytes != size.MainBytes+size.WALBytes+size.SHMBytes || size.PageSize <= 0 || size.Pages <= 0 || size.FreePages > size.Pages {
		t.Fatalf("size report=%+v", size)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version=%d", turns.TurnSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"size", "--database", path}, &output); err == nil {
		t.Fatal("command accepted future schema")
	}
	missing := filepath.Join(dir, "missing.sqlite")
	if err := run(ctx, []string{"size", "--database", missing}, &output); err == nil {
		t.Fatal("command accepted missing store")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only command created a store")
	}
}

func TestBackupCommandPreservesLegacySchemaWithoutMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "control.sqlite")
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE backup_evidence (value TEXT); INSERT INTO backup_evidence VALUES ('committed WAL fact')`); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup.sqlite")
	var output bytes.Buffer
	if err := run(ctx, []string{"backup", "--database", path, "--out", backup}, &output); err != nil {
		t.Fatal(err)
	}
	copyDB, err := statedb.Open(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var version int
	var value string
	if err := copyDB.QueryRow(`SELECT value FROM backup_evidence`).Scan(&value); err != nil || value != "committed WAL fact" {
		t.Fatalf("backup evidence=%q: %v", value, err)
	}
	for _, source := range []*sql.DB{db, copyDB} {
		if err := source.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 19 {
			t.Fatalf("schema=%d: %v", version, err)
		}
	}
	if err := run(ctx, []string{"size", "--database", path}, &output); err == nil {
		t.Fatal("legacy backup support relaxed canonical diagnosis")
	}
	if _, err := db.Exec(`PRAGMA user_version=21`); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"backup", "--database", path, "--out", filepath.Join(dir, "future.sqlite")}, &output); err == nil {
		t.Fatal("backup accepted an unsupported schema")
	}
}

func TestCreationNamePreviewCommandIsReadOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	if _, err := state.PrepareStore(ctx, state.StorePaths{Destination: path, BackupRoot: filepath.Join(dir, "migration")}, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	db, err := statedb.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hosted_threads(hosted_thread_id,provider,current_native_id,presentation_mode,revision,created_at,updated_at) VALUES('nht_preview','codex','native-preview','shared',1,'before','before')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hosted_thread_native_ids(hosted_thread_id,provider,native_thread_id,provisional,first_seen_at,last_seen_at) VALUES('nht_preview','codex','native-preview',0,'before','before')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(dir, "request.json")
	request := `{"apply":false,"records":[{"hostedThreadId":"nht_preview","provider":"codex","nativeThreadId":"native-preview","name":"preview name","source":"creation_request"}]}`
	if err := os.WriteFile(requestPath, []byte(request), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(ctx, []string{"creation-name-preview", "--database", path, "--request", requestPath}, &output); err != nil {
		t.Fatal(err)
	}
	var receipt threads.CreationNameRepairReceipt
	if err := json.Unmarshal(output.Bytes(), &receipt); err != nil || receipt.Applied || receipt.Preview.Missing != 1 || receipt.Preview.Fingerprint == "" {
		t.Fatalf("preview=%+v err=%v", receipt, err)
	}
	check, err := state.OpenReadOnly(ctx, path, statewiring.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if _, ok, err := threads.ReadCreationName(ctx, check, "nht_preview"); err != nil || ok {
		t.Fatalf("preview mutated store: ok=%v err=%v", ok, err)
	}
	if err := run(ctx, []string{"creation-name-preview", "--database", path, "--request", requestPath}, &output); err != nil {
		t.Fatal(err)
	}
	request = strings.Replace(request, `"apply":false`, `"apply":true`, 1)
	if err := os.WriteFile(requestPath, []byte(request), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"creation-name-preview", "--database", path, "--request", requestPath}, &output); err == nil {
		t.Fatal("preview command accepted apply=true")
	}
}

func newHistoryCollectCommandFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO source_roots(id,provider,root_kind,canonical_root) VALUES(1,'codex','sessions','/tmp/sessions');
INSERT INTO source_files(id,source_root_id,canonical_path,classification,status) VALUES(1,1,'/tmp/sessions/thread.jsonl','transcript','present');
INSERT INTO threads(id,source_root_id,provider,native_thread_id,kind,metadata_json,source_status) VALUES('thread',1,'codex','native','main','{}','present');
INSERT INTO conversation_projections(id,projection_key,thread_id,source_file_id,source_generation,provider,parser_version,completion_rule_version,schema_version,taxonomy_version,state,parser_state_json,item_count,last_indexed_at)
VALUES(1,'old','thread',1,1,'codex','test','test',1,'test','superseded','{}',7,'2026-09-14T00:00:00Z');
INSERT INTO conversation_projections(id,projection_key,thread_id,source_file_id,source_generation,provider,parser_version,completion_rule_version,schema_version,taxonomy_version,state,parser_state_json,item_count,last_indexed_at)
VALUES(2,'active','thread',1,1,'codex','test','test',1,'test','active','{}',1,'2026-09-14T00:00:01Z');
INSERT INTO conversation_item_aliases(thread_id,old_item_id,canonical_item_id,source_key,created_at)
VALUES('thread','old-orphan','missing-item','old-source','2026-09-14T00:00:00Z');`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHistoryCollectDryRunIsReadOnly(t *testing.T) {
	ctx := context.Background()
	path := newHistoryCollectCommandFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(ctx, []string{"history-collect", "--db", path, "--dry-run"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "history collector: dry-run candidates=1 items=7") || !strings.Contains(output.String(), "projection=1 thread=thread items=7") {
		t.Fatalf("dry-run output=%q", output.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("history-collect dry-run changed the database")
	}
}

func TestHistoryCollectCommandDrainsSyntheticStore(t *testing.T) {
	ctx := context.Background()
	path := newHistoryCollectCommandFixture(t)
	var output bytes.Buffer
	if err := run(ctx, []string{"history-collect", "--db", path, "--max-minutes", "1"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "history collector: pass=1 collected=1 items=0") || !strings.Contains(output.String(), "complete stopped=drained") || !strings.Contains(output.String(), "aliases=1") {
		t.Fatalf("collection output=%q", output.String())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var superseded, active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='superseded'`).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if superseded != 0 || active != 1 {
		t.Fatalf("superseded=%d active=%d, want 0 and 1", superseded, active)
	}
}

func TestHistoryCollectCommandReportsPerTableProgress(t *testing.T) {
	path := newHistoryCollectCommandFixture(t)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"history-collect", "--db", path, "--max-minutes", "1"}, &output); err != nil {
		t.Fatal(err)
	}
	want := "history collector: pass=1 projection=1 rows=conversation_parser_state_entries:0,conversation_item_observations:0,conversation_turn_observations:0,conversation_item_bodies:0,conversation_search_documents:0,conversation_parse_diagnostics:0,conversation_relations:0,conversation_parse_identities:0,conversation_observation_aliases:0,conversation_turn_aliases:0,conversation_observations:0,conversation_items:0,conversation_turns:0,conversation_parse_values:0,conversation_projections:1"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("progress output=%q, want line containing %q", output.String(), want)
	}
}

func TestHistoryCollectCommandRetriesBusyWriter(t *testing.T) {
	ctx := context.Background()
	path := newHistoryCollectCommandFixture(t)
	locker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	lockConn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(ctx, `PRAGMA busy_timeout=1; BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	time.AfterFunc(5200*time.Millisecond, func() {
		_, err := lockConn.ExecContext(ctx, `COMMIT`)
		released <- err
	})

	var output bytes.Buffer
	err = run(ctx, []string{"history-collect", "--db", path, "--max-minutes", "1"}, &output)
	if releaseErr := <-released; releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err != nil {
		t.Fatalf("history-collect did not retry busy writer: %v; output=%q", err, output.String())
	}
	if !strings.Contains(output.String(), "history collector: retry=1 wait=100ms reason=busy") {
		t.Fatalf("missing busy retry log: %q", output.String())
	}
	if !strings.Contains(output.String(), "complete stopped=drained") {
		t.Fatalf("collector did not drain after lock release: %q", output.String())
	}
}

func TestHistoryCollectBusyRetryDoesNotDeadlineHealthyAttempt(t *testing.T) {
	busyErr := fmt.Errorf("synthetic busy")
	calls := 0
	batch, err := collectSupersededWithRetry(context.Background(), &bytes.Buffer{}, func(ctx context.Context) (history.SupersededProjectionBatch, error) {
		calls++
		if calls == 1 {
			return history.SupersededProjectionBatch{}, busyErr
		}
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			t.Fatal("busy retry imposed its 30-second wait budget on healthy collection work")
		}
		return history.SupersededProjectionBatch{Collected: 1}, nil
	}, func(err error) bool { return err == busyErr })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || batch.Collected != 1 {
		t.Fatalf("calls=%d batch=%+v", calls, batch)
	}
}
