package turns

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestStoreStartFinishAndFeed(t *testing.T) {
	ctx := context.Background()
	db := turnFixture(t)
	store, err := OpenStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	if row, created, err := store.Started(ctx, Start{HostedThreadID: "host", Provider: "codex", NativeThreadID: "native", TurnID: "turn-1", ActionID: "action", At: startedAt}); err != nil || !created || row.Outcome != "" {
		t.Fatalf("started row=%+v created=%v err=%v", row, created, err)
	}
	input, output := int64(10), int64(4)
	row, _, err := store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "codex", NativeThreadID: "native", TurnID: "turn-1", ActionID: "action", FinalMessageID: "final", Outcome: "completed", At: startedAt.Add(time.Second), Usage: Usage{InputTokens: &input, OutputTokens: &output}})
	if err != nil || row.Outcome != "completed" || row.FinalMessageID != "final" || row.UsageInputTokens == nil || *row.UsageInputTokens != input {
		t.Fatalf("finished row=%+v err=%v", row, err)
	}
	// A repeated finish of the ended turn keeps its recorded usage, zero or
	// not, and only fills what was unknown; a different terminal conflicts.
	zero, cached := int64(0), int64(3)
	row, _, err = store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "codex", NativeThreadID: "native", TurnID: "turn-1", FinalMessageID: "final", Outcome: "completed", At: startedAt.Add(2 * time.Second), Usage: Usage{InputTokens: &zero, CachedInputTokens: &cached, OutputTokens: &zero}})
	if err != nil || row.UsageInputTokens == nil || *row.UsageInputTokens != input || row.UsageOutputTokens == nil || *row.UsageOutputTokens != output ||
		row.UsageCachedInputTokens == nil || *row.UsageCachedInputTokens != cached {
		t.Fatalf("repeated finish row=%+v err=%v; want usage %d/%d kept and cached %d filled", row, err, input, output, cached)
	}
	if _, _, err := store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "codex", TurnID: "turn-1", FinalMessageID: "other", Outcome: "completed", At: startedAt}); err == nil {
		t.Fatal("a different final message replaced the retained terminal")
	}
	// A turn that ended with an explicit zero keeps it when a later finish
	// carries usage: the shared reply's usage is counted once, elsewhere.
	if _, _, err := store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "codex", TurnID: "turn-2", FinalMessageID: "final", Outcome: "completed", At: startedAt, Usage: Usage{InputTokens: &zero, CachedInputTokens: &zero, OutputTokens: &zero}}); err != nil {
		t.Fatal(err)
	}
	row, _, err = store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "codex", TurnID: "turn-2", FinalMessageID: "final", Outcome: "completed", At: startedAt, Usage: Usage{InputTokens: &input, OutputTokens: &output}})
	if err != nil || row.UsageInputTokens == nil || *row.UsageInputTokens != 0 || row.UsageOutputTokens == nil || *row.UsageOutputTokens != 0 {
		t.Fatalf("zero-usage turn after a later finish row=%+v err=%v; want explicit zero kept", row, err)
	}
	// A turn finished from the saved transcript records zero for a counter it
	// has none for, a placeholder the provider's own result replaces, once:
	// after that the provider's counts are recorded and a repeat keeps them.
	// A count the saved finish kept is never replaced.
	held := int64(7)
	for turn, saved := range map[string]Usage{"turn-3": {InputTokens: &zero, CachedInputTokens: &zero, OutputTokens: &zero}, "turn-4": {InputTokens: &held, CachedInputTokens: &zero, OutputTokens: &zero}} {
		if _, _, err := store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "claude", TurnID: turn, FinalMessageID: "final", Outcome: "completed", At: startedAt, Source: "saved", Usage: saved}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "claude", TurnID: turn, FinalMessageID: "final", Outcome: "completed", At: startedAt, Usage: Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
			t.Fatal(err)
		}
		row, _, err = store.Finished(ctx, Finish{HostedThreadID: "host", Provider: "claude", TurnID: turn, FinalMessageID: "final", Outcome: "completed", At: startedAt, Usage: Usage{InputTokens: &zero, OutputTokens: &zero}})
		want := map[string]int64{"turn-3": input, "turn-4": held}[turn]
		if err != nil || row.UsageInputTokens == nil || *row.UsageInputTokens != want || row.UsageOutputTokens == nil || *row.UsageOutputTokens != output || row.UsageCachedInputTokens == nil || *row.UsageCachedInputTokens != 0 {
			t.Fatalf("%s after saved, provider and repeated finishes = %+v, %v; want input %d, output %d, cached 0", turn, row, err, want, output)
		}
	}
	feed, err := store.After(ctx, "host", "", 10)
	if err != nil || len(feed) != 4 || feed[0].TurnID != "turn-1" {
		t.Fatalf("feed=%+v err=%v", feed, err)
	}
}

func turnFixture(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE completed_turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target_hosted_thread_id TEXT NOT NULL,
		provider TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		final_message_id TEXT NOT NULL,
		completed_at TEXT NOT NULL,
		source TEXT NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE(target_hosted_thread_id, turn_id)
	);
		ALTER TABLE completed_turns ADD COLUMN client_action_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE completed_turns ADD COLUMN native_thread_id TEXT NOT NULL DEFAULT '';
		ALTER TABLE completed_turns ADD COLUMN binding_key TEXT NOT NULL DEFAULT '';
		ALTER TABLE completed_turns ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
		ALTER TABLE completed_turns ADD COLUMN usage_input_tokens INTEGER;
		ALTER TABLE completed_turns ADD COLUMN usage_cached_input_tokens INTEGER;
		ALTER TABLE completed_turns ADD COLUMN usage_output_tokens INTEGER;
		PRAGMA user_version=22;`); err != nil {
		t.Fatal(err)
	}
	return db
}
