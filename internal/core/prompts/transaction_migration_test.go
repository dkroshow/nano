package prompts

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	statedb "github.com/dkroshow/nano/internal/state/db"
)

func schema20TurnFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "threads.sqlite")
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		queuedTurnSchema,
		sendAttemptSchema,
		CompletedTurnSchema,
		`CREATE TABLE threads_store_migrations(name TEXT PRIMARY KEY, report_json TEXT NOT NULL, manifest_json TEXT NOT NULL)`,
		`INSERT INTO threads_store_migrations VALUES('turn-records-v20','{}','{}')`,
		`PRAGMA user_version=20`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	queue := []QueuedTurn{
		{ClientActionID: "agent", Provider: "codex", HostedThreadID: "hosted", NativeThreadID: "native", Text: "retain agent text", PresentationText: "agent text", TextHash: "provider-hash", RequestHash: "request-hash", State: "accepted", Origin: "agent", CreatedAt: "2026-09-10T00:00:00Z", ProviderTurnID: "turn-a", ProviderTerminalStatus: "completed", ProviderTerminalAt: "2026-09-10T00:00:01Z"},
		{ClientActionID: "legacy-orphan", Provider: "claude", NativeThreadID: "legacy-native", Text: "retain orphan text", TextHash: "legacy-hash", State: "uncertain", CreatedAt: "2026-09-10T00:00:02Z"},
	}
	attempts := []SendAttempt{
		{ClientActionID: "agent", Provider: "codex", NativeThreadID: "native", TextHash: "request-hash", Status: "accepted", RunID: "old", CreatedAt: "2026-09-10T00:00:00Z", UpdatedAt: "2026-09-10T00:00:01Z", ProviderTurnID: "turn-a", IdentityConflict: "retained-conflict"},
		{ClientActionID: "attempt-orphan", Provider: "grok", NativeThreadID: "orphan", TextHash: "orphan-hash", Status: "unknown", RunID: "old", CreatedAt: "2026-09-10T00:00:03Z", UpdatedAt: "2026-09-10T00:00:03Z"},
	}
	if err := SaveQueuedTurns(context.Background(), db, queue); err != nil {
		t.Fatal(err)
	}
	if err := SaveSendAttempts(context.Background(), db, attempts); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO completed_turns(id,target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at)
		VALUES(4,'hosted','codex','turn-a','final','2026-09-10T00:00:04Z','provider','2026-09-10T00:00:04Z');
		UPDATE sqlite_sequence SET seq=19 WHERE name='completed_turns'`); err != nil {
		t.Fatal(err)
	}
	return db, path
}

func TestMigrateTurnTransactionsPreservesAllThreeFamilies(t *testing.T) {
	ctx := context.Background()
	db, path := schema20TurnFixture(t)
	queueBefore, err := LoadQueuedTurns(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	attemptsBefore, err := LoadSendAttempts(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(path), "schema-20-backup.sqlite")
	report, err := MigrateTurnTransactions(ctx, db, backup)
	if err != nil {
		t.Fatal(err)
	}
	if report.BackupPath != backup || report.Queued != 2 || report.Attempts != 2 || report.Completed != 1 || report.CompletedSequence != 19 {
		t.Fatalf("migration report = %+v", report)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != TransactionalTurnSchemaVersion {
		t.Fatalf("version = %d, %v", version, err)
	}
	rows, err := db.Query(`SELECT client_action_id,provider,hosted_thread_id,native_thread_id,text,presentation_text,text_hash,request_hash,state,origin,created_at,provider_turn_id,provider_terminal_status,provider_terminal_at,delivery_rank,revision,binding_json,binding_key,claim_token,claim_epoch FROM queued_turns ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	var queueAfter []QueuedTurn
	var ranks []int
	for rows.Next() {
		var row QueuedTurn
		var rank, revision, epoch int
		var bindingJSON, bindingKey, claim string
		if err := rows.Scan(&row.ClientActionID, &row.Provider, &row.HostedThreadID, &row.NativeThreadID, &row.Text, &row.PresentationText, &row.TextHash, &row.RequestHash, &row.State, &row.Origin, &row.CreatedAt, &row.ProviderTurnID, &row.ProviderTerminalStatus, &row.ProviderTerminalAt, &rank, &revision, &bindingJSON, &bindingKey, &claim, &epoch); err != nil {
			t.Fatal(err)
		}
		if revision != 0 || bindingJSON != "" || bindingKey != "" || claim != "" || epoch != 0 {
			t.Fatal("migration invented queue identity, revision, or claim")
		}
		queueAfter, ranks = append(queueAfter, row), append(ranks, rank)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(queueAfter, queueBefore) || !reflect.DeepEqual(ranks, []int{1, 0}) {
		t.Fatalf("queue changed: %+v ranks=%v", queueAfter, ranks)
	}
	rows, err = db.Query(`SELECT client_action_id,provider,native_thread_id,text_hash,status,run_id,reason,created_at,updated_at,confirmed,confirmed_at,provider_turn_id,provider_terminal_status,provider_terminal_at,canonical_message_id,identity_conflict,hosted_thread_id,revision,binding_json,claim_token FROM send_attempts ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	var attemptsAfter []SendAttempt
	var owners []string
	for rows.Next() {
		var row SendAttempt
		var confirmed int
		var owner, bindingJSON, claim string
		var revision int
		if err := rows.Scan(&row.ClientActionID, &row.Provider, &row.NativeThreadID, &row.TextHash, &row.Status, &row.RunID, &row.Reason, &row.CreatedAt, &row.UpdatedAt, &confirmed, &row.ConfirmedAt, &row.ProviderTurnID, &row.ProviderTerminalStatus, &row.ProviderTerminalAt, &row.CanonicalMessageID, &row.IdentityConflict, &owner, &revision, &bindingJSON, &claim); err != nil {
			t.Fatal(err)
		}
		row.Confirmed = confirmed != 0
		if revision != 0 || bindingJSON != "" || claim != "" {
			t.Fatal("migration invented attempt identity, revision, or claim")
		}
		attemptsAfter, owners = append(attemptsAfter, row), append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(attemptsAfter, attemptsBefore) || !reflect.DeepEqual(owners, []string{"hosted", ""}) {
		t.Fatalf("attempts changed: %+v owners=%v", attemptsAfter, owners)
	}
	var completionID, sequence int64
	var action, nativeThread, bindingKey, outcome string
	if err := db.QueryRow(`SELECT id,client_action_id,native_thread_id,binding_key,outcome FROM completed_turns`).Scan(&completionID, &action, &nativeThread, &bindingKey, &outcome); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='completed_turns'`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if completionID != 4 || sequence != 19 || action != "" || nativeThread != "" || bindingKey != "" || outcome != "" {
		t.Fatal("migration changed completion identity or invented authority")
	}
	second, err := MigrateTurnTransactions(ctx, db, filepath.Join(filepath.Dir(path), "must-not-exist.sqlite"))
	if err != nil || !reflect.DeepEqual(second, report) {
		t.Fatalf("idempotent migration = %+v, %v", second, err)
	}
}

func TestTurnTransactionMigrationIsAdditiveForRollback(t *testing.T) {
	for _, statement := range turnTransactionMigrationStatements {
		upper := strings.ToUpper(statement)
		if strings.Contains(upper, " RENAME ") || strings.Contains(upper, "DROP ") {
			t.Fatalf("destructive schema statement: %s", statement)
		}
	}
	ctx := context.Background()
	db, path := schema20TurnFixture(t)
	before, err := LoadQueuedTurns(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateTurnTransactions(ctx, db, filepath.Join(filepath.Dir(path), "additive-v20.sqlite")); err != nil {
		t.Fatal(err)
	}
	after, err := LoadQueuedTurns(ctx, db)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("v20 queue reader after additive migration = %+v, %v", after, err)
	}
	attempts, err := LoadSendAttempts(ctx, db)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("v20 attempt reader after additive migration = %+v, %v", attempts, err)
	}
	var completions int
	if err := db.QueryRow(`SELECT count(*) FROM completed_turns WHERE target_hosted_thread_id <> '' AND turn_id <> ''`).Scan(&completions); err != nil || completions != 1 {
		t.Fatalf("v20 completion reader after additive migration = %d, %v", completions, err)
	}
	for table, column := range map[string]string{"queued_turns": "position", "send_attempts": "position", "completed_turns": "id"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count); err != nil || count != 1 {
			t.Fatalf("legacy column %s.%s missing: %d, %v", table, column, count, err)
		}
	}
}

func TestMigrateTurnTransactionsRollsBackFailedCandidate(t *testing.T) {
	ctx := context.Background()
	db, path := schema20TurnFixture(t)
	wantQueue, err := LoadQueuedTurns(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = migrateTurnTransactions(ctx, db, filepath.Join(filepath.Dir(path), "failed-backup.sqlite"), func(*sql.Tx) error {
		return errors.New("synthetic publication stop")
	})
	if err == nil {
		t.Fatal("failed migration committed")
	}
	var version, renamed int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('queued_turns') WHERE name='revision'`).Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	if version != LegacyTurnSchemaVersion || renamed != 0 {
		t.Fatalf("failed migration left version=%d renamed=%d", version, renamed)
	}
	if got, err := LoadQueuedTurns(ctx, db); err != nil || !reflect.DeepEqual(got, wantQueue) {
		t.Fatalf("failed migration changed source rows: %+v, %v", got, err)
	}
}
