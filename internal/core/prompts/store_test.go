package prompts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// openCurrentStore takes a fixture to the current schema the way store
// preparation takes a real store, then opens it; on a current store the
// migrations only verify their receipts.
func openCurrentStore(t *testing.T, ctx context.Context, db *sql.DB) (*Store, error) {
	t.Helper()
	dir := t.TempDir()
	// In production the control tables share this database; the current schema
	// also records each provisional handle's resolution there.
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS hosted_thread_native_ids(hosted_thread_id TEXT NOT NULL, provider TEXT NOT NULL, native_thread_id TEXT NOT NULL, PRIMARY KEY(hosted_thread_id, provider, native_thread_id));
		CREATE TABLE IF NOT EXISTS hosted_turn_operations(operation_id TEXT PRIMARY KEY, hosted_thread_id TEXT NOT NULL, transport_action_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// The usage stage that takes a store from 21 to 22 belongs to core/turns,
	// which this package must not import; its columns stand in for it here.
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version == TransactionalTurnSchemaVersion {
		if _, err := db.ExecContext(ctx, `ALTER TABLE completed_turns ADD COLUMN usage_input_tokens INTEGER; ALTER TABLE completed_turns ADD COLUMN usage_cached_input_tokens INTEGER; ALTER TABLE completed_turns ADD COLUMN usage_output_tokens INTEGER; PRAGMA user_version=22`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := MigrateMemberRecovery(ctx, db, filepath.Join(dir, "v22.sqlite")); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateParkedDiscard(ctx, db, filepath.Join(dir, "v23.sqlite")); err != nil {
		t.Fatal(err)
	}
	return OpenStore(ctx, db)
}

func transactionalStore(t *testing.T) (*Store, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	db, path := emptySchema20Fixture(t)
	if _, err := MigrateTurnTransactions(ctx, db, filepath.Join(filepath.Dir(path), "v20.sqlite")); err != nil {
		t.Fatal(err)
	}
	store, err := openCurrentStore(t, ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return store, db, path
}

func emptySchema20Fixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "threads.sqlite")
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{queuedTurnSchema, sendAttemptSchema, CompletedTurnSchema,
		`CREATE TABLE threads_store_migrations(name TEXT PRIMARY KEY, report_json TEXT NOT NULL, manifest_json TEXT NOT NULL)`,
		`INSERT INTO threads_store_migrations VALUES('turn-records-v20','{}','{}')`, `PRAGMA user_version=20`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func admission(host, action string, origin Origin) Submission {
	return Submission{
		HostedThreadID: host, Provider: model.ProviderCodex, NativeThreadID: "native-" + host,
		ActionID: action, ProviderText: "provider " + action, PresentationText: "shown " + action,
		TextHash: "text-" + action, RequestHash: "request-" + action, Origin: origin,
		CreatedAt: "2026-09-11T03:00:00Z",
	}
}

func binding(host, turn string) Binding {
	nativeThread := "native-" + host
	return Binding{Scope: model.Scope{
		Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &nativeThread, NativeTurnID: &turn,
	}}
}

// A Claude prompt's confirmation owes its own member completion, even with no
// turn identity, until the member step clears that exact revision; the owed
// and own-event reads start from their own indexes, never a table scan.
func TestSettledClaudePromptOwesItsMemberCompletionUntilItsRevisionClears(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	claude := func(action string) Submission {
		request := admission("host", action, HumanOrigin)
		request.Provider = model.ProviderClaude
		return request
	}
	for _, request := range []Submission{claude("uncertain-claude"), claude("accepted-claude"), admission("host", "codex", HumanOrigin)} {
		if _, _, err := store.Submit(ctx, request); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + request.ActionID
		if _, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: token, AttachmentEpoch: 4, Ready: true}); err != nil || !ok {
			t.Fatalf("claim %s = %v err %v", request.ActionID, ok, err)
		}
		var err error
		switch request.ActionID {
		case "accepted-claude":
			native, turn := "native-host", "turn-a"
			_, err = store.Accepted(ctx, Acceptance{ActionID: request.ActionID, ClaimToken: token, AttachmentEpoch: 4,
				Binding: Binding{EventID: "event-a", Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}})
		case "codex":
			_, err = store.Accepted(ctx, Acceptance{ActionID: request.ActionID, ClaimToken: token, AttachmentEpoch: 4, Binding: binding("host", "turn-b")})
		default:
			_, err = store.Uncertain(ctx, Uncertainty{ActionID: request.ActionID, ClaimToken: token, AttachmentEpoch: 4, Reason: "no answer"})
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, settled, err := store.Settle(ctx, request.ActionID); err != nil || !settled {
			t.Fatalf("settle %s = %v err %v", request.ActionID, settled, err)
		}
	}
	owed, err := store.Owed(ctx, "host", 0, 10)
	if err != nil || len(owed) != 2 || owed[0].Outbox.ActionID != "uncertain-claude" || owed[1].Outbox.ActionID != "accepted-claude" {
		t.Fatalf("owed = %+v, %v; want both Claude prompts and not the Codex one", owed, err)
	}
	if later, err := store.Owed(ctx, "host", owed[0].Outbox.Sequence, 10); err != nil || len(later) != 1 || later[0].Outbox.ActionID != "accepted-claude" {
		t.Fatalf("owed after the first = %+v, %v", later, err)
	}
	if byEvent, err := store.ByEvent(ctx, "host", "event-a"); err != nil || len(byEvent) != 1 || byEvent[0].Outbox.ActionID != "accepted-claude" {
		t.Fatalf("by event = %+v, %v", byEvent, err)
	}
	if cleared, err := store.ClearOwed(ctx, "uncertain-claude", owed[0].Revision+1); err != nil || cleared {
		t.Fatalf("a stale revision cleared the obligation: %v, %v", cleared, err)
	}
	if cleared, err := store.ClearOwed(ctx, "uncertain-claude", owed[0].Revision); err != nil || !cleared {
		t.Fatalf("clear = %v, %v", cleared, err)
	}
	if left, err := store.Owed(ctx, "host", 0, 10); err != nil || len(left) != 1 || left[0].Outbox.ActionID != "accepted-claude" {
		t.Fatalf("owed after clearing = %+v, %v", left, err)
	}
	// Retention may drop an operation only for a prompt that can never owe:
	// a non-Claude one, a confirmed one that owes nothing, an expired one.
	if _, _, err := store.Submit(ctx, claude("expired-claude")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim-expired", AttachmentEpoch: 4, Ready: true}); err != nil || !ok {
		t.Fatalf("claim expired-claude = %v err %v", ok, err)
	}
	if _, err := store.Uncertain(ctx, Uncertainty{ActionID: "expired-claude", ClaimToken: "claim-expired", AttachmentEpoch: 4, Reason: "no answer"}); err != nil {
		t.Fatal(err)
	}
	if _, expired, err := store.Expire(ctx, "expired-claude", time.Now()); err != nil || !expired {
		t.Fatalf("expire = %v, %v", expired, err)
	}
	if _, _, err := store.Submit(ctx, claude("waiting-claude")); err != nil {
		t.Fatal(err)
	}
	eligible, err := store.PruneEligible(ctx, "host", []string{"uncertain-claude", "accepted-claude", "codex", "expired-claude", "waiting-claude", "missing"})
	if err != nil || !reflect.DeepEqual(eligible, map[string]bool{"uncertain-claude": true, "codex": true, "expired-claude": true}) {
		t.Fatalf("prune eligible = %v, %v; want only absorbing states", eligible, err)
	}
	if _, err := store.PruneEligible(ctx, "host", make([]string, PruneEligibleBatch+1)); err == nil {
		t.Fatal("an over-bound eligibility read was accepted")
	}
	for _, read := range []struct {
		clause, index string
		args          []any
	}{
		{owedClause, "queued_turns_member_owed", []any{"host", 0, 32}},
		{eventClause, "queued_turns_claude_event", []any{"host", "event-a"}},
	} {
		clause, index := read.clause, read.index
		rows, err := db.Query(`EXPLAIN QUERY PLAN `+recordSelect+clause, read.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		if len(plan) == 0 || !strings.Contains(plan[0], index) {
			t.Fatalf("read does not start from %s: %v", index, plan)
		}
	}
}

func TestStoreAdmissionIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	request := admission("host", "action", AgentOrigin)
	record, created, err := store.Submit(ctx, request)
	if err != nil || !created || record.Outbox.Sequence != 1 || record.Outbox.State != Queued || record.Revision != 1 {
		t.Fatalf("first admission = %+v, created=%v err=%v", record, created, err)
	}
	replay, created, err := store.Submit(ctx, request)
	if err != nil || created || replay.Outbox.ActionID != record.Outbox.ActionID || replay.Outbox.Sequence != record.Outbox.Sequence {
		t.Fatalf("replay = %+v, created=%v err=%v", replay, created, err)
	}
	conflict := request
	conflict.RequestHash = "different"
	if _, _, err := store.Submit(ctx, conflict); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("conflicting replay = %v", err)
	}
	got, attempt, err := store.Get(ctx, "action")
	if err != nil || got.Outbox.ActionID != "action" || attempt.ActionID != "action" || got.Revision != attempt.Revision {
		t.Fatalf("get action = %+v %+v, %v", got, attempt, err)
	}

	if _, err := db.Exec(`CREATE TRIGGER fail_attempt_admission BEFORE INSERT ON send_attempts WHEN NEW.client_action_id='rollback' BEGIN SELECT RAISE(ABORT,'synthetic attempt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Submit(ctx, admission("host", "rollback", HumanOrigin)); err == nil {
		t.Fatal("journal failure committed the outbox")
	}
	var queue, attempts int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM queued_turns WHERE client_action_id='rollback'),(SELECT count(*) FROM send_attempts WHERE client_action_id='rollback')`).Scan(&queue, &attempts); err != nil {
		t.Fatal(err)
	}
	if queue != 0 || attempts != 0 {
		t.Fatal("failed admission was not atomic")
	}
}

type countingRowsQueryer struct {
	db      *sql.DB
	queries int
}

func (q *countingRowsQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.db.QueryContext(ctx, query, args...)
}

func TestListThreadAttemptsReadsOnlyRequestedThreadInOneQuery(t *testing.T) {
	_, db, _ := transactionalStore(t)
	attempts := make([]SendAttempt, 0, 5_003)
	for i := 0; i < 5_000; i++ {
		attempts = append(attempts, SendAttempt{
			ClientActionID: fmt.Sprintf("unrelated-%04d", i), Provider: "codex", NativeThreadID: "unrelated-thread",
			Status: "failed", CreatedAt: "2026-09-15T00:00:00Z", UpdatedAt: "2026-09-15T00:00:00Z",
		})
	}
	attempts = append(attempts,
		SendAttempt{ClientActionID: "parked", Provider: "codex", NativeThreadID: "target-thread", TextHash: "hash-parked", Status: "unknown", RunID: "run-1", Reason: "legacy", CreatedAt: "2026-09-15T01:00:00Z", UpdatedAt: "2026-09-15T01:01:00Z"},
		SendAttempt{ClientActionID: "accepted", Provider: "codex", NativeThreadID: "target-thread", TextHash: "hash-accepted", Status: "accepted", RunID: "run-2", CreatedAt: "2026-09-15T02:00:00Z", UpdatedAt: "2026-09-15T02:01:00Z", Confirmed: true, ConfirmedAt: "2026-09-15T02:02:00Z", ProviderTurnID: "provider-turn", ProviderTerminalStatus: "completed", ProviderTerminalAt: "2026-09-15T02:03:00Z", CanonicalMessageID: "message", IdentityConflict: "conflict"},
		SendAttempt{ClientActionID: "orphan", Provider: "codex", NativeThreadID: "target-thread", TextHash: "hash-orphan", Status: "refused", RunID: "run-3", Reason: "legacy orphan", CreatedAt: "2026-09-15T03:00:00Z", UpdatedAt: "2026-09-15T03:01:00Z"},
	)
	if err := SaveSendAttempts(context.Background(), db, attempts); err != nil {
		t.Fatal(err)
	}
	if err := SaveQueuedTurns(context.Background(), db, []QueuedTurn{{
		ClientActionID: "parked", Provider: "codex", HostedThreadID: "hosted", NativeThreadID: "target-thread",
		Text: "retained queue text", PresentationText: "retained presentation", TextHash: "hash-parked", RequestHash: "request-parked",
		State: "parked", Origin: "human", CreatedAt: "2026-09-15T01:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}

	counting := &countingRowsQueryer{db: db}
	got, err := listThreadAttempts(context.Background(), counting, model.ProviderCodex, "target-thread")
	if err != nil {
		t.Fatal(err)
	}
	if counting.queries != 1 {
		t.Fatalf("database queries = %d, want 1", counting.queries)
	}
	if len(got) != 3 {
		t.Fatalf("attempts = %d, want 3", len(got))
	}
	if got[0].Attempt.ActionID != "parked" || got[0].OutboxState != Parked {
		t.Fatalf("parked attempt = %+v", got[0])
	}
	if got[1].Attempt.ActionID != "accepted" || !got[1].Attempt.Confirmed || got[1].Attempt.ProviderTurnID != "provider-turn" || got[1].Attempt.CanonicalItemID != "message" || got[1].Attempt.IdentityConflict != "conflict" {
		t.Fatalf("accepted attempt fields changed: %+v", got[1])
	}
	if got[2].Attempt.ActionID != "orphan" || got[2].OutboxState != "" {
		t.Fatalf("legacy orphan attempt was lost: %+v", got[2])
	}
	queue, err := LoadQueuedTurns(context.Background(), db)
	if err != nil || len(queue) != 1 || queue[0].Text != "retained queue text" || queue[0].PresentationText != "retained presentation" {
		t.Fatalf("queue text changed: %+v, %v", queue, err)
	}
}

func TestStoreParksIncompleteBootRowAndKeepsDiagnosticOnReopen(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	const action = "agent-4eac7709f014e5a187cba60260f9e865"
	if _, _, err := store.Submit(ctx, admission("host", action, AgentOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE queued_turns SET text='',presentation_text='' WHERE client_action_id=?`, action); err != nil {
		t.Fatal(err)
	}

	parked, err := store.ParkIncompleteAtBoot(ctx)
	if err != nil || len(parked) != 1 || parked[0].ActionID != action || len(parked[0].Missing) != 1 || parked[0].Missing[0] != "text" {
		t.Fatalf("boot parks = %#v, err=%v", parked, err)
	}
	record, attempt, err := store.Get(ctx, action)
	if err != nil || record.Outbox.State != Parked || attempt.State != JournalUnknown || attempt.Reason != "boot_incomplete_outbox:text" {
		t.Fatalf("parked record = %+v, attempt=%+v, err=%v", record, attempt, err)
	}
	if _, won, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim", AttachmentEpoch: 2, Ready: true}); err != nil || won {
		t.Fatalf("boot-parked row was claimable: won=%v err=%v", won, err)
	}

	again, err := store.ParkIncompleteAtBoot(ctx)
	if err != nil || len(again) != 1 || again[0].ActionID != action {
		t.Fatalf("persisted boot diagnostic = %#v, err=%v", again, err)
	}
}

func TestStoreBootParkRollsBackOutboxWhenJournalMarkerFails(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "rollback", AgentOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE queued_turns SET text='',presentation_text='' WHERE client_action_id='rollback'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_boot_marker BEFORE UPDATE ON send_attempts WHEN NEW.client_action_id='rollback' BEGIN SELECT RAISE(ABORT,'synthetic boot marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ParkIncompleteAtBoot(ctx); err == nil {
		t.Fatal("journal marker failure committed a partial boot park")
	}
	record, attempt, err := store.Get(ctx, "rollback")
	if err != nil || record.Outbox.State != Queued || attempt.State != JournalQueued || attempt.Reason != "" {
		t.Fatalf("failed boot park changed durable rows: record=%+v attempt=%+v err=%v", record, attempt, err)
	}
}

func TestCancelQueuedBatchIsOrderedAndAtomic(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	for _, action := range []string{"first", "second"} {
		if _, _, err := store.Submit(ctx, admission("host", action, HumanOrigin)); err != nil {
			t.Fatal(err)
		}
	}
	canceled, err := store.CancelQueuedBatch(ctx, []string{"first", "second"})
	if err != nil || len(canceled) != 2 || canceled[0].Outbox.ActionID != "first" || canceled[1].Outbox.ActionID != "second" {
		t.Fatalf("batch result = %+v, err=%v", canceled, err)
	}
	for _, action := range []string{"first", "second"} {
		record, attempt, loadErr := store.Get(ctx, action)
		if loadErr != nil || record.Outbox.State != Refused || attempt.State != JournalRefused || attempt.Reason != "canceled" {
			t.Fatalf("%s = record %+v attempt %+v err=%v", action, record, attempt, loadErr)
		}
	}
	if _, _, err := store.Submit(ctx, admission("host", "parked", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE queued_turns SET state='parked' WHERE client_action_id='parked'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE send_attempts SET status='unknown',reason='synthetic parked prompt' WHERE client_action_id='parked'`); err != nil {
		t.Fatal(err)
	}
	parked, err := store.CancelQueuedBatch(ctx, []string{"parked"})
	if err != nil || len(parked) != 1 || parked[0].Outbox.State != Parked {
		t.Fatalf("parked discard = %+v err=%v", parked, err)
	}
	parkedRecord, parkedAttempt, err := store.Get(ctx, "parked")
	if err != nil || parkedRecord.Outbox.State != Parked || parkedAttempt.State != JournalUnknown {
		t.Fatalf("parked discard changed durable state: record=%+v attempt=%+v err=%v", parkedRecord, parkedAttempt, err)
	}

	for _, action := range []string{"third", "fourth"} {
		if _, _, err := store.Submit(ctx, admission("host", action, HumanOrigin)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_second_batch BEFORE UPDATE ON send_attempts WHEN NEW.client_action_id='fourth' BEGIN SELECT RAISE(ABORT,'synthetic batch failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CancelQueuedBatch(ctx, []string{"third", "fourth"}); err == nil {
		t.Fatal("journal failure committed a partial cancellation batch")
	}
	for _, action := range []string{"third", "fourth"} {
		record, attempt, loadErr := store.Get(ctx, action)
		if loadErr != nil || record.Outbox.State != Queued || attempt.State != JournalQueued {
			t.Fatalf("failed batch changed %s: record %+v attempt %+v err=%v", action, record, attempt, loadErr)
		}
	}
}

func TestStoreAdoptsOnlyExactLegacyPairsBeforeClaiming(t *testing.T) {
	ctx := context.Background()
	db, path := emptySchema20Fixture(t)
	queue := []QueuedTurn{
		{ClientActionID: "queued", Provider: "codex", HostedThreadID: "host", NativeThreadID: "native", Text: "queued text", PresentationText: "queued text", TextHash: "queued-provider", RequestHash: "queued-request", State: "queued", Origin: "human", CreatedAt: "2026-09-11T01:00:00Z"},
		{ClientActionID: "accepted-without-turn", Provider: "codex", HostedThreadID: "host", NativeThreadID: "native", Text: "uncertain text", PresentationText: "uncertain text", TextHash: "uncertain-provider", RequestHash: "uncertain-request", State: "accepted", Origin: "agent", CreatedAt: "2026-09-11T01:00:01Z"},
		{ClientActionID: "accepted-bound", Provider: "codex", HostedThreadID: "host", NativeThreadID: "native", Text: "bound text", PresentationText: "bound text", TextHash: "bound-provider", RequestHash: "bound-request", State: "accepted", Origin: "system", CreatedAt: "2026-09-11T01:00:02Z", ProviderTurnID: "turn-bound"},
		{ClientActionID: "orphan", Provider: "claude", HostedThreadID: "other", NativeThreadID: "claude-native", Text: "orphan text", PresentationText: "orphan text", TextHash: "orphan-provider", RequestHash: "orphan-request", State: "queued", Origin: "human", CreatedAt: "2026-09-11T01:00:03Z"},
	}
	attempts := []SendAttempt{
		{ClientActionID: "queued", Provider: "codex", NativeThreadID: "native", TextHash: "queued-request", Status: "queued", CreatedAt: queue[0].CreatedAt, UpdatedAt: queue[0].CreatedAt},
		{ClientActionID: "accepted-without-turn", Provider: "codex", NativeThreadID: "native", TextHash: "uncertain-request", Status: "accepted", CreatedAt: queue[1].CreatedAt, UpdatedAt: queue[1].CreatedAt},
		{ClientActionID: "accepted-bound", Provider: "codex", NativeThreadID: "native", TextHash: "bound-request", Status: "accepted", CreatedAt: queue[2].CreatedAt, UpdatedAt: queue[2].CreatedAt, ProviderTurnID: "turn-bound"},
	}
	if err := SaveQueuedTurns(ctx, db, queue); err != nil {
		t.Fatal(err)
	}
	if err := SaveSendAttempts(ctx, db, attempts); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateTurnTransactions(ctx, db, filepath.Join(filepath.Dir(path), "schema20.sqlite")); err != nil {
		t.Fatal(err)
	}
	store, err := openCurrentStore(t, ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.AdoptLegacy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Adopted != 3 || report.Uncertain != 1 || report.Unmanaged != 1 || len(report.Diagnostics) != 1 {
		t.Fatalf("adoption report = %+v", report)
	}
	records, err := store.List(ctx, "")
	if err != nil || len(records) != 4 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if records[0].Revision != 1 || records[0].Outbox.Sequence != 1 || records[0].Outbox.State != Queued {
		t.Fatalf("queued adoption = %+v", records[0])
	}
	if records[1].Outbox.State != Uncertain || records[1].ProviderText != "uncertain text" {
		t.Fatalf("uncertain adoption = %+v", records[1])
	}
	if records[2].Outbox.State != Accepted || records[2].Outbox.Binding == nil || records[2].Outbox.Binding.Scope.NativeTurnID == nil || *records[2].Outbox.Binding.Scope.NativeTurnID != "turn-bound" {
		t.Fatalf("bound adoption = %+v", records[2])
	}
	if records[3].Revision != 0 || records[3].ProviderText != "orphan text" {
		t.Fatalf("unmanaged evidence changed = %+v", records[3])
	}
	claimed, won, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim", AttachmentEpoch: 9, Ready: true})
	if err != nil || !won || claimed.Outbox.ActionID != "queued" {
		t.Fatalf("legacy queue claim = %+v won=%v err=%v", claimed, won, err)
	}
}

func TestStoreConcurrentAdmissionAndClaimHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	request := admission("host", "agent-first", AgentOrigin)
	var wait sync.WaitGroup
	wait.Add(2)
	created := make(chan bool, 2)
	errorsSeen := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wait.Done()
			_, inserted, err := store.Submit(ctx, request)
			created <- inserted
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(created)
	close(errorsSeen)
	insertions := 0
	for inserted := range created {
		if inserted {
			insertions++
		}
	}
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if insertions != 1 {
		t.Fatalf("duplicate admission insertions = %d", insertions)
	}
	if _, _, err := store.Submit(ctx, admission("host", "human-second", HumanOrigin)); err != nil {
		t.Fatal(err)
	}

	type result struct {
		record  Record
		claimed bool
		err     error
	}
	claims := make(chan result, 2)
	wait.Add(2)
	for _, token := range []string{"claim-a", "claim-b"} {
		token := token
		go func() {
			defer wait.Done()
			record, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: token, AttachmentEpoch: 7, Ready: true})
			claims <- result{record, claimed, err}
		}()
	}
	wait.Wait()
	close(claims)
	winners, held := 0, 0
	for claim := range claims {
		if claim.claimed {
			winners++
			if claim.record.Outbox.ActionID != "human-second" || claim.record.Outbox.Sequence != 2 {
				t.Fatalf("rank changed presentation sequence or selected wrong action: %+v", claim.record)
			}
		} else if errors.Is(claim.err, ErrClaimHeld) {
			held++
			if claim.record.Outbox.ActionID != "human-second" || claim.record.ClaimToken == "" {
				t.Fatalf("held claim omitted its exact row: %+v", claim.record)
			}
		} else {
			t.Fatalf("unexpected claim result: %+v", claim)
		}
	}
	if winners != 1 || held != 1 {
		t.Fatalf("claim winners=%d held=%d", winners, held)
	}
}

func TestStoreRestartMakesOldClaimUncertainWithoutRetry(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "action", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "old-token", AttachmentEpoch: 3, Ready: true})
	if err != nil || first.Outbox.State != Delivering {
		t.Fatal(err)
	}
	restarted, err := openCurrentStore(t, ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.Restore(ctx, "host", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Outbox) != 1 || restored.Outbox[0].State != Uncertain || len(restored.Attempts) != 1 || restored.Attempts[0].State != JournalUnknown || len(restored.Diagnostics) != 1 {
		t.Fatalf("stale restore = %+v", restored)
	}
	uncertain, attempt, getErr := restarted.Get(ctx, "action")
	if getErr != nil || uncertain.Outbox.State != Uncertain || uncertain.ClaimToken != "" || uncertain.ClaimEpoch != 0 || attempt.ClaimToken != "old-token" {
		t.Fatalf("restored uncertain claim: record=%+v attempt=%+v getErr=%v", uncertain, attempt, getErr)
	}
	_, claimed, claimErr := restarted.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "new-token", AttachmentEpoch: 4, Ready: true})
	if claimErr != nil || claimed {
		t.Fatalf("uncertain row retried: claimed=%v err=%v", claimed, claimErr)
	}
}

func TestStoreRestartReleasesUncertainClaimAndAcceptsNextPrompt(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "uncertain", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "old-token", AttachmentEpoch: 3, Ready: true}); err != nil || !claimed {
		t.Fatalf("old claim=%v err=%v", claimed, err)
	}
	// This is the pre-fix restart shape: delivery is already uncertain, but
	// its queue-side dispatch claim survived.
	if _, err := db.Exec(`UPDATE queued_turns SET state='uncertain' WHERE client_action_id='uncertain'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE send_attempts SET status='unknown',reason='unknown_after_restart' WHERE client_action_id='uncertain'`); err != nil {
		t.Fatal(err)
	}
	restarted, err := openCurrentStore(t, ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Restore(ctx, "host", 4); err != nil {
		t.Fatal(err)
	}
	record, attempt, err := restarted.Get(ctx, "uncertain")
	if err != nil || record.Outbox.State != Uncertain || record.ClaimToken != "" || record.ClaimEpoch != 0 || attempt.ClaimToken != "old-token" {
		t.Fatalf("restored uncertain claim: record=%+v attempt=%+v err=%v", record, attempt, err)
	}
	if _, _, err := restarted.Submit(ctx, admission("host", "follow-up", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := restarted.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "new-token", AttachmentEpoch: 4, Ready: true})
	if err != nil || !ok || claimed.Outbox.ActionID != "follow-up" {
		t.Fatalf("follow-up claim=%+v claimed=%v err=%v", claimed, ok, err)
	}
	accepted, err := restarted.Accepted(ctx, Acceptance{ActionID: "follow-up", ClaimToken: "new-token", AttachmentEpoch: 4, Binding: binding("host", "turn-follow-up")})
	if err != nil || accepted.Outbox.State != Accepted {
		t.Fatalf("follow-up acceptance=%+v err=%v", accepted, err)
	}
}

func TestUncertainDoesNotBlockNextPrompt(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "uncertain", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "first-token", AttachmentEpoch: 3, Ready: true})
	if err != nil || !ok {
		t.Fatalf("first claim=%+v claimed=%v err=%v", claimed, ok, err)
	}
	if _, err := store.Uncertain(ctx, Uncertainty{ActionID: "uncertain", ClaimToken: "first-token", AttachmentEpoch: 3, Reason: "no answer", At: ""}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Submit(ctx, admission("host", "follow-up", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "second-token", AttachmentEpoch: 3, Ready: true})
	if err != nil || !ok || claimed.Outbox.ActionID != "follow-up" {
		t.Fatalf("follow-up claim=%+v claimed=%v err=%v", claimed, ok, err)
	}
	// The provider took the first prompt after all: its late receipt, for the
	// claim it was sent under, still binds it.
	late, err := store.Accepted(ctx, Acceptance{ActionID: "uncertain", ClaimToken: "first-token", AttachmentEpoch: 3, Binding: binding("host", "turn-late")})
	if err != nil || late.Outbox.State != Accepted || late.ProviderTurnID != "turn-late" {
		t.Fatalf("late receipt=%+v err=%v; want the uncertain prompt accepted into its turn", late.Outbox, err)
	}
	if _, err := store.Accepted(ctx, Acceptance{ActionID: "uncertain", ClaimToken: "second-token", AttachmentEpoch: 3, Binding: binding("host", "turn-late")}); !errors.Is(err, ErrClaimMismatch) {
		t.Fatalf("receipt for another claim: err=%v; want a claim mismatch", err)
	}
}

func TestStoreRecordsDefiniteProviderRefusal(t *testing.T) {
	for _, test := range []struct {
		state   DeliveryState
		journal JournalState
	}{
		{state: Refused, journal: JournalRefused},
		{state: Parked, journal: JournalUnknown},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			ctx := context.Background()
			store, _, _ := transactionalStore(t)
			if _, _, err := store.Submit(ctx, admission("host", "action", HumanOrigin)); err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim", AttachmentEpoch: 3, Ready: true}); err != nil || !claimed {
				t.Fatalf("claim=%v err=%v", claimed, err)
			}
			record, err := store.Refused(ctx, Refusal{
				ActionID: "action", ClaimToken: "claim", AttachmentEpoch: 3,
				State: test.state, Reason: "provider refused",
			})
			if err != nil {
				t.Fatal(err)
			}
			persisted, attempt, err := store.Get(ctx, "action")
			if err != nil || record.Outbox.State != test.state || persisted.Outbox.State != test.state || persisted.ProviderText == "" ||
				persisted.ClaimToken != "" || persisted.ClaimEpoch != 0 || attempt.State != test.journal || attempt.Reason != "provider refused" || attempt.ClaimToken != "" {
				t.Fatalf("refusal record=%+v persisted=%+v attempt=%+v err=%v", record, persisted, attempt, err)
			}
		})
	}
}

func TestAcceptedPromptIsUnchangedByTurnCompletion(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "accepted", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim", AttachmentEpoch: 9, Ready: true}); err != nil || !ok {
		t.Fatalf("claim=%v err=%v", ok, err)
	}
	if _, err := store.Accepted(ctx, Acceptance{ActionID: "accepted", ClaimToken: "claim", AttachmentEpoch: 9, Binding: binding("host", "turn-a")}); err != nil {
		t.Fatal(err)
	}
	beforeRecord, beforeAttempt, err := store.Get(ctx, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	// The store is already current, usage columns included.
	if _, err := db.Exec(`INSERT INTO completed_turns(target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at,client_action_id,native_thread_id,binding_key,outcome) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"host", "codex", "turn-a", "final-a", "2026-09-14T10:00:00Z", "adapter", "2026-09-14T10:00:00Z", "accepted", "native-host", "turn-a", "completed"); err != nil {
		t.Fatal(err)
	}
	afterRecord, afterAttempt, err := store.Get(ctx, "accepted")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterRecord, beforeRecord) || !reflect.DeepEqual(afterAttempt, beforeAttempt) {
		t.Fatalf("accepted prompt changed after completion:\nrecord before=%+v\nrecord after=%+v\nattempt before=%+v\nattempt after=%+v", beforeRecord, afterRecord, beforeAttempt, afterAttempt)
	}
}

func TestStoreRestoreReleasesLegacyAcceptedDispatchClaim(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "accepted", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "old-token", AttachmentEpoch: 3, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if _, err := store.Accepted(ctx, Acceptance{ActionID: "accepted", ClaimToken: "old-token", AttachmentEpoch: 3, Binding: binding("host", "turn-old")}); err != nil {
		t.Fatal(err)
	}
	// Rows accepted before decision 14 kept the queue-side dispatch claim.
	if _, err := db.Exec(`UPDATE queued_turns SET claim_token='old-token',claim_epoch=3 WHERE client_action_id='accepted'`); err != nil {
		t.Fatal(err)
	}
	restarted, err := openCurrentStore(t, ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Restore(ctx, "host", 4); err != nil {
		t.Fatal(err)
	}
	var queueToken, attemptToken string
	var queueEpoch int
	if err := db.QueryRow(`SELECT q.claim_token,q.claim_epoch,a.claim_token FROM queued_turns q JOIN send_attempts a USING(client_action_id) WHERE q.client_action_id='accepted'`).Scan(&queueToken, &queueEpoch, &attemptToken); err != nil {
		t.Fatal(err)
	}
	if queueToken != "" || queueEpoch != 0 || attemptToken != "old-token" {
		t.Fatalf("restored claims queue=%q/%d attempt=%q", queueToken, queueEpoch, attemptToken)
	}
	if _, _, err := restarted.Submit(ctx, admission("host", "follow-up", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := restarted.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "new-token", AttachmentEpoch: 4, Ready: true})
	if err != nil || !ok || claimed.Outbox.ActionID != "follow-up" {
		t.Fatalf("follow-up claim=%+v claimed=%v err=%v", claimed, ok, err)
	}
}

func TestStoreRebindsNativeSuccessionAtomically(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	if _, _, err := store.Submit(ctx, admission("host", "accepted", HumanOrigin)); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim", AttachmentEpoch: 9, Ready: true}); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if _, err := store.Accepted(ctx, Acceptance{ActionID: "accepted", ClaimToken: "claim", AttachmentEpoch: 9, Binding: binding("host", "turn-a")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Submit(ctx, admission("host", "queued", HumanOrigin)); err != nil {
		t.Fatal(err)
	}

	outbox, err := store.RebindNativeThread(ctx, "host", model.ProviderCodex, "native-host", "native-successor")
	if err != nil || len(outbox) != 1 || outbox[0].ActionID != "queued" {
		t.Fatalf("rebind outbox=%+v err=%v", outbox, err)
	}
	queued, queuedAttempt, err := store.Get(ctx, "queued")
	if err != nil || queued.NativeThreadID != "native-successor" || queuedAttempt.NativeThreadID != "native-successor" {
		t.Fatalf("queued row did not rebind: record=%+v attempt=%+v err=%v", queued, queuedAttempt, err)
	}
	accepted, _, err := store.Get(ctx, "accepted")
	if err != nil || accepted.NativeThreadID != "native-host" || accepted.ProviderTurnID != "turn-a" || accepted.Outbox.Binding == nil || *accepted.Outbox.Binding.Scope.NativeTurnID != "turn-a" {
		t.Fatalf("accepted row changed during rebind: %+v err=%v", accepted, err)
	}
}

// Settlement is the only way a delivered prompt leaves attach-time
// publication. The store must keep its text, make it terminal, and leave
// prompts it cannot settle untouched. Boundary: the prompt store.
func TestSettledPromptKeepsTextAndLeavesRestore(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	deliver := func(action string, finish func(token string) error) {
		t.Helper()
		if _, _, err := store.Submit(ctx, admission("host", action, HumanOrigin)); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + action
		if _, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: token, AttachmentEpoch: 4, Ready: true}); err != nil || !ok {
			t.Fatalf("claim %s = %v err %v", action, ok, err)
		}
		if err := finish(token); err != nil {
			t.Fatal(err)
		}
	}
	deliver("accepted", func(token string) error {
		_, err := store.Accepted(ctx, Acceptance{ActionID: "accepted", ClaimToken: token, AttachmentEpoch: 4, Binding: binding("host", "turn-a")})
		return err
	})
	deliver("uncertain", func(token string) error {
		_, err := store.Uncertain(ctx, Uncertainty{ActionID: "uncertain", ClaimToken: token, AttachmentEpoch: 4, Reason: "no answer"})
		return err
	})
	deliver("refused", func(token string) error {
		_, err := store.Refused(ctx, Refusal{ActionID: "refused", ClaimToken: token, AttachmentEpoch: 4, State: Refused, Reason: "no"})
		return err
	})
	if _, _, err := store.Submit(ctx, admission("host", "queued", HumanOrigin)); err != nil {
		t.Fatal(err)
	}

	for _, action := range []string{"accepted", "uncertain"} {
		if _, settled, err := store.Settle(ctx, action); err != nil || !settled {
			t.Fatalf("settle %s = %v err %v", action, settled, err)
		}
		record, attempt, err := store.Get(ctx, action)
		if err != nil || record.Outbox.State != Confirmed || record.ProviderText != "provider "+action ||
			record.Outbox.PresentationText != "shown "+action || !attempt.Confirmed || attempt.ConfirmedAt == "" || attempt.State != JournalConfirmed {
			t.Fatalf("settled %s record=%+v attempt=%+v err=%v", action, record, attempt, err)
		}
		if again, settled, err := store.Settle(ctx, action); err != nil || settled || again.Revision != record.Revision {
			t.Fatalf("second settle of %s changed the record: settled=%v revision %d->%d err=%v", action, settled, record.Revision, again.Revision, err)
		}
		retry, created, err := store.Submit(ctx, admission("host", action, HumanOrigin))
		if err != nil || created || retry.Outbox.State != Confirmed {
			t.Fatalf("same-id retry of settled %s = %+v created=%v err=%v", action, retry, created, err)
		}
	}
	for _, action := range []string{"refused", "queued"} {
		before, _, _ := store.Get(ctx, action)
		if after, settled, err := store.Settle(ctx, action); err != nil || settled || after.Outbox.State != before.Outbox.State || after.Revision != before.Revision {
			t.Fatalf("settle changed %s: settled=%v before=%+v after=%+v err=%v", action, settled, before, after, err)
		}
	}

	restored, err := store.Restore(ctx, "host", 5)
	if err != nil {
		t.Fatal(err)
	}
	var outbox, attempts []string
	for _, entry := range restored.Outbox {
		outbox = append(outbox, entry.ActionID)
	}
	for _, attempt := range restored.Attempts {
		attempts = append(attempts, attempt.ActionID)
	}
	if !reflect.DeepEqual(outbox, []string{"refused", "queued"}) || !reflect.DeepEqual(attempts, []string{"refused", "queued"}) {
		t.Fatalf("restore published settled prompts: outbox=%v attempts=%v", outbox, attempts)
	}
}

// Live turn events name their prompt through this read: a miss loses the
// prompt's turn (and the completed-turn feed its operation), and a hit in
// another thread or turn gives the turn to the wrong prompt. Boundary: the
// thread-scoped provider-turn read, with the same turn id accepted in another
// thread and a legacy prompt whose turn only its send journal kept.
func TestProviderTurnReadsOnlyThatThreadsPromptForTheTurn(t *testing.T) {
	ctx := context.Background()
	db, path := emptySchema20Fixture(t)
	if err := SaveQueuedTurns(ctx, db, []QueuedTurn{{ClientActionID: "adopted", Provider: "codex", HostedThreadID: "host", NativeThreadID: "native-host",
		Text: "adopted text", PresentationText: "adopted text", TextHash: "adopted-provider", RequestHash: "adopted-request",
		State: "accepted", Origin: "human", CreatedAt: "2026-09-11T01:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveSendAttempts(ctx, db, []SendAttempt{{ClientActionID: "adopted", Provider: "codex", NativeThreadID: "native-host", TextHash: "adopted-request",
		Status: "accepted", CreatedAt: "2026-09-11T01:00:00Z", UpdatedAt: "2026-09-11T01:00:00Z", ProviderTurnID: "turn-journal"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateTurnTransactions(ctx, db, filepath.Join(filepath.Dir(path), "schema20.sqlite")); err != nil {
		t.Fatal(err)
	}
	store, err := openCurrentStore(t, ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if report, err := store.AdoptLegacy(ctx); err != nil || report.Adopted != 1 || report.Uncertain != 0 {
		t.Fatalf("adoption = %+v, %v", report, err)
	}
	for _, accepted := range []struct{ host, action, turn string }{
		{"host", "first", "turn-a"}, {"host", "second", "turn-b"}, {"other", "elsewhere", "turn-a"},
	} {
		if _, _, err := store.Submit(ctx, admission(accepted.host, accepted.action, HumanOrigin)); err != nil {
			t.Fatal(err)
		}
		token := "claim-" + accepted.action
		if _, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: accepted.host, Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !ok {
			t.Fatalf("claim %s=%v err=%v", accepted.action, ok, err)
		}
		if _, err := store.Accepted(ctx, Acceptance{ActionID: accepted.action, ClaimToken: token, AttachmentEpoch: 1, Binding: binding(accepted.host, accepted.turn)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, read := range []struct {
		host, native, turn string
		want               []string
	}{
		{"host", "native-host", "turn-a", []string{"first"}},
		{"host", "", "turn-b", []string{"second"}},
		{"host", "native-host", "turn-journal", []string{"adopted"}},
		{"host", "native-other", "turn-a", nil},
		{"other", "native-other", "turn-a", []string{"elsewhere"}},
	} {
		records, err := store.ProviderTurn(ctx, read.host, model.ProviderCodex, read.native, read.turn)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, record := range records {
			got = append(got, record.Outbox.ActionID)
		}
		if !reflect.DeepEqual(got, read.want) {
			t.Fatalf("%s %s %s = %v, want %v", read.host, read.native, read.turn, got, read.want)
		}
	}
}

// A queue position counts the prompts delivered before this one: a reader in
// submission order, or across threads, reports the wrong place. Boundary: the
// thread's queued read, with every delivery class and another thread queued.
func TestQueuedListsTheThreadsQueueInDeliveryOrder(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	for _, queued := range []struct {
		host, action string
		origin       Origin
	}{
		{"host", "system", SystemOrigin}, {"host", "agent-1", AgentOrigin}, {"other", "elsewhere", HumanOrigin},
		{"host", "human", HumanOrigin}, {"host", "agent-2", AgentOrigin},
	} {
		if _, _, err := store.Submit(ctx, admission(queued.host, queued.action, queued.origin)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.Queued(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, record := range records {
		got = append(got, record.Outbox.ActionID)
	}
	if want := []string{"human", "agent-1", "agent-2", "system"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("queued = %v, want %v", got, want)
	}
}

// An expired prompt must never be shown again, nor claimed delivered, and
// keeps its text; a recent or claimed prompt must not expire. Boundary: the
// store's expiry and its publication reads (Unsettled, Restore).
func TestExpiredPromptIsHiddenForGoodAndKeepsItsText(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	accept := func(action, turn string) {
		t.Helper()
		if _, _, err := store.Submit(ctx, admission("host", action, HumanOrigin)); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !ok {
			t.Fatalf("claim %s=%v err=%v", action, ok, err)
		}
		if _, err := store.Accepted(ctx, Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: binding("host", turn)}); err != nil {
			t.Fatal(err)
		}
	}
	accept("old", "turn-old")
	accept("recent", "turn-recent")
	if _, err := db.Exec(`UPDATE queued_turns SET created_at=? WHERE client_action_id='recent'`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	stale, err := store.Stale(ctx, cutoff)
	if err != nil || len(stale) != 1 || stale[0].Outbox.ActionID != "old" {
		t.Fatalf("stale = %+v, %v", stale, err)
	}
	if _, expired, err := store.Expire(ctx, "recent", cutoff); err != nil || expired {
		t.Fatalf("recent prompt expired=%v err=%v", expired, err)
	}
	record, expired, err := store.Expire(ctx, "old", cutoff)
	if err != nil || !expired || record.Outbox.State != Expired || record.ProviderText != "provider old" || record.Outbox.PresentationText != "shown old" {
		t.Fatalf("expire old = %+v expired=%v err=%v", record, expired, err)
	}
	if _, again, err := store.Expire(ctx, "old", cutoff); err != nil || again {
		t.Fatalf("second expiry changed=%v err=%v", again, err)
	}
	if _, settled, err := store.Settle(ctx, "old"); err != nil || settled {
		t.Fatalf("settle after expiry changed=%v err=%v", settled, err)
	}
	// A replayed delivery receipt must not revive it.
	if replayed, err := store.Accepted(ctx, Acceptance{ActionID: "old", ClaimToken: "claim-old", AttachmentEpoch: 1, Binding: binding("host", "turn-old")}); err != nil || replayed.Outbox.State != Expired {
		t.Fatalf("accepted replay after expiry = %+v, %v", replayed, err)
	}
	unsettled, err := store.Unsettled(ctx, "host")
	if err != nil || len(unsettled) != 1 || unsettled[0].ActionID != "recent" {
		t.Fatalf("unsettled = %+v, %v", unsettled, err)
	}
	restored, err := store.Restore(ctx, "host", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range restored.Outbox {
		if entry.ActionID == "old" {
			t.Fatal("restore republished the expired prompt")
		}
	}
	for _, attempt := range restored.Attempts {
		if attempt.ActionID == "old" {
			t.Fatal("restore returned the expired prompt's attempt")
		}
	}
}

// Prompts steered into one running turn bind to the same provider turn. Each
// keeps its own native event, so the second acceptance must not collide with
// the first; a repeat of the same event is still a conflict.
func TestPromptsSharingARunningTurnAcceptWithTheirOwnEvents(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	accept := func(action, token, event string) error {
		if _, _, err := store.Submit(ctx, admission("host", action, AgentOrigin)); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: token, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", action, claimed, err)
		}
		shared := binding("host", "turn-shared")
		shared.EventID = event
		_, err := store.Accepted(ctx, Acceptance{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Binding: shared})
		return err
	}
	if err := accept("starter", "claim-1", "event-starter"); err != nil {
		t.Fatal(err)
	}
	if err := accept("steered", "claim-2", "event-steered"); err != nil {
		t.Fatalf("second prompt in the running turn: %v", err)
	}
	if err := accept("repeat", "claim-3", "event-steered"); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("repeated event err=%v; want binding conflict", err)
	}
}

// A running turn may take only ordinary prompts. A prompt held for the next
// turn and a system prompt wait for the turn to end even when they are first
// in line, also after a restart; once the thread is idle they keep their
// original order.
func TestRunningTurnClaimSkipsHeldAndSystemPrompts(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	held := admission("host", "held", HumanOrigin)
	held.NextTurn = true
	for _, request := range []Submission{held, admission("host", "notice", SystemOrigin), admission("host", "ordinary", AgentOrigin)} {
		if _, _, err := store.Submit(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	store, err := openCurrentStore(t, ctx, db) // a new Store on the same database: the choice is in the rows, not the first Store
	if err != nil {
		t.Fatal(err)
	}
	record, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "steer", AttachmentEpoch: 1, Ready: true, IntoRunningTurn: true})
	if err != nil || !claimed || record.Outbox.ActionID != "ordinary" {
		t.Fatalf("running-turn claim=%q claimed=%v err=%v; want ordinary", record.Outbox.ActionID, claimed, err)
	}
	if _, err := store.Requeue(ctx, Requeue{ActionID: "ordinary", ClaimToken: "steer", AttachmentEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for index := range 3 {
		token := fmt.Sprintf("idle-%d", index)
		record, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: token, AttachmentEpoch: 1, Ready: true})
		if err != nil || !claimed {
			t.Fatalf("idle claim %d: claimed=%v err=%v", index, claimed, err)
		}
		order = append(order, record.Outbox.ActionID)
		if _, err := store.Refused(ctx, Refusal{ActionID: record.Outbox.ActionID, ClaimToken: token, AttachmentEpoch: 1, State: Refused, Reason: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"held", "ordinary", "notice"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("idle order=%v; want %v", order, want)
	}
}

// A provider that refuses a steer because the turn already ended never saw the
// prompt. Requeue keeps the same action, text and place, and holds it for the
// next turn so it cannot join a different running turn; it is delivered once,
// at idle. Acceptance clears the marker, which only ever describes a queued
// prompt.
func TestRequeueHoldsTheSameActionForTheNextTurn(t *testing.T) {
	ctx := context.Background()
	store, _, _ := transactionalStore(t)
	for _, action := range []string{"first", "second"} {
		if _, _, err := store.Submit(ctx, admission("host", action, HumanOrigin)); err != nil {
			t.Fatal(err)
		}
	}
	if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "steer", AttachmentEpoch: 2, Ready: true, IntoRunningTurn: true}); err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	if _, err := store.Requeue(ctx, Requeue{ActionID: "first", ClaimToken: "other", AttachmentEpoch: 2}); !errors.Is(err, ErrClaimMismatch) {
		t.Fatalf("foreign claim err=%v; want mismatch", err)
	}
	record, err := store.Requeue(ctx, Requeue{ActionID: "first", ClaimToken: "steer", AttachmentEpoch: 2})
	if err != nil || record.Outbox.State != Queued || record.ClaimToken != "" || record.ProviderText != "provider first" || record.RequestHash != "request-first" {
		t.Fatalf("requeued=%+v err=%v", record, err)
	}
	if _, attempt, err := store.Get(ctx, "first"); err != nil || attempt.State != JournalQueued || attempt.Reason != ReasonNextTurn || attempt.ClaimToken != "" {
		t.Fatalf("journal=%+v err=%v", attempt, err)
	}
	steered, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "steer-again", AttachmentEpoch: 2, Ready: true, IntoRunningTurn: true})
	if err != nil || !claimed || steered.Outbox.ActionID != "second" {
		t.Fatalf("running-turn claim=%q claimed=%v err=%v; want second", steered.Outbox.ActionID, claimed, err)
	}
	if _, err := store.Refused(ctx, Refusal{ActionID: "second", ClaimToken: "steer-again", AttachmentEpoch: 2, State: Refused, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	next, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "idle", AttachmentEpoch: 2, Ready: true})
	if err != nil || !claimed || next.Outbox.ActionID != "first" {
		t.Fatalf("idle claim=%q claimed=%v err=%v; want first", next.Outbox.ActionID, claimed, err)
	}
	if _, err := store.Accepted(ctx, Acceptance{ActionID: "first", ClaimToken: "idle", AttachmentEpoch: 2, Binding: binding("host", "turn-next")}); err != nil {
		t.Fatal(err)
	}
	if _, attempt, err := store.Get(ctx, "first"); err != nil || attempt.State != JournalAccepted || attempt.Reason != "" {
		t.Fatalf("accepted journal=%+v err=%v; want the next-turn marker cleared", attempt, err)
	}
}

// A legacy prompt's live row is proven by elimination: the turn's other rows
// belong to prompts that claim them by client id. A claim counts only from a
// binding on this provider, native thread and turn at the root, never a child
// agent's call; it comes from the durable record, so a confirmed prompt still
// claims. A binding that cannot be read must fail, not read as no claims.
func TestTurnEventsReadsOnlyQualifiedClaims(t *testing.T) {
	ctx := context.Background()
	store, db, _ := transactionalStore(t)
	accept := func(action string, bind Binding) {
		if _, _, err := store.Submit(ctx, admission("host", action, AgentOrigin)); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.Claim(ctx, ClaimRequest{HostedThreadID: "host", Token: "claim-" + action, AttachmentEpoch: 1, Ready: true}); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", action, claimed, err)
		}
		if _, err := store.Accepted(ctx, Acceptance{ActionID: action, ClaimToken: "claim-" + action, AttachmentEpoch: 1, Binding: bind}); err != nil {
			t.Fatal(err)
		}
	}
	withEvent := func(bind Binding, event string) Binding {
		bind.EventID = event
		return bind
	}
	accept("legacy", binding("host", "turn-a"))
	accept("correction", withEvent(binding("host", "turn-a"), "client-correction"))
	accept("other-turn", withEvent(binding("host", "turn-b"), "client-other-turn"))
	child := withEvent(binding("host", "turn-a"), "client-child")
	call := "spawn-call"
	child.Scope.NativeParentCallID = &call
	accept("child", child)
	if _, settled, err := store.Settle(ctx, "correction"); err != nil || !settled {
		t.Fatalf("settle correction: %v %v", settled, err)
	}

	events, err := store.TurnEvents(ctx, "host", model.ProviderCodex, "native-host", "turn-a")
	if err != nil || !reflect.DeepEqual(events, []string{"client-correction"}) {
		t.Fatalf("events = %v err=%v; want only the confirmed root correction", events, err)
	}
	if events, err := store.TurnEvents(ctx, "host", model.ProviderCodex, "native-elsewhere", "turn-a"); err != nil || len(events) != 0 {
		t.Fatalf("another native thread: events = %v err=%v", events, err)
	}
	if _, err := db.Exec(`UPDATE queued_turns SET binding_json='{"scope":1}' WHERE client_action_id='correction'`); err != nil {
		t.Fatal(err)
	}
	if events, err := store.TurnEvents(ctx, "host", model.ProviderCodex, "native-host", "turn-a"); err == nil {
		t.Fatalf("unreadable binding read as events %v; want an error", events)
	}
}
