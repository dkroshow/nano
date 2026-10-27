package history

import (
	"context"
	"path/filepath"
	"testing"
)

// turnTotalsFixture builds the minimum row graph ThreadTurnTotals reads: one
// thread with an ACTIVE projection and — because that is the case the aggregate
// exists to survive — a retained SUPERSEDED one carrying its own copy of the
// same turns. Rows are inserted directly rather than projected from a fixture
// file: the shape under test is the SQL's scoping, not the parser's.
func turnTotalsFixture(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT INTO source_roots(id, provider, root_kind, canonical_root) VALUES (1,'codex','sessions','/tmp/roots')`)
	exec(`INSERT INTO source_files(id, source_root_id, canonical_path, classification, status)
	      VALUES (1,1,'/tmp/roots/a.jsonl','session','present')`)
	exec(`INSERT INTO threads(id, source_root_id, provider, native_thread_id, kind, metadata_json, source_status)
	      VALUES ('nth_1',1,'codex','native-1','main','{}','present')`)

	projection := func(id int64, key, state string, latestSeq int64) {
		exec(`INSERT INTO conversation_projections(
		        id, projection_key, thread_id, source_file_id, source_generation, provider,
		        parser_version, completion_rule_version, schema_version, taxonomy_version,
		        state, latest_seq, last_indexed_at)
		      VALUES (?,?,'nth_1',1,1,'codex','v5','v1',1,'v2',?,?, '2026-07-31T00:00:00Z')`,
			id, key, state, latestSeq)
	}
	turn := func(projectionID int64, ordinal int, completionSeq any, duration any) {
		exec(`INSERT INTO conversation_turns(
		        projection_id, turn_key, ordinal, start_seq, end_seq, completion_seq,
		        status, completion_evidence, duration_ms)
		      VALUES (?,?,?,?,?,?,'completed','task_complete',?)`,
			projectionID, "turn-", ordinal, ordinal, ordinal, completionSeq, duration)
		// turn_key must be unique per projection; rewrite it with the ordinal.
		exec(`UPDATE conversation_turns SET turn_key = ? WHERE projection_id = ? AND ordinal = ?`,
			"turn-"+string(rune('a'+ordinal)), projectionID, ordinal)
	}

	projection(1, "ntp_active", "active", 100)
	turn(1, 1, 10, 60_000)  // counted
	turn(1, 2, 20, 700_000) // counted
	turn(1, 3, 30, nil)     // no native duration — NULL, not zero
	turn(1, 4, nil, 5_000)  // never completed; duration is not countable
	turn(1, 5, 999, 9_000)  // completes past the projection head — in flight here

	projection(2, "ntp_dead", "superseded", 100)
	turn(2, 1, 10, 60_000)
	turn(2, 2, 20, 700_000)

	return store
}

// The whole point of the aggregate: a partial sum, honestly counted, taken from
// the active generation alone. The superseded generation in this fixture holds
// the same two countable turns — an unscoped SUM reports 1,520,000 over 4
// turns, which is the bug this fixture exists to catch.
func TestThreadTurnTotalsScopesToTheActiveGeneration(t *testing.T) {
	store := turnTotalsFixture(t)
	totals, err := store.ThreadTurnTotals(context.Background(), "nth_1")
	if err != nil {
		t.Fatal(err)
	}
	if totals.DurationMS != 760_000 || totals.Turns != 2 {
		t.Fatalf("totals = %+v, want 760000ms over 2 turns (NULL, unfinished and past-head turns excluded)", totals)
	}
}

// Zero coverage is a real state, not an error: the caller drops the field
// rather than rendering "0s · 0 turns".
func TestThreadTurnTotalsReportsZeroCoverage(t *testing.T) {
	store := turnTotalsFixture(t)
	if _, err := store.db.Exec(`UPDATE conversation_turns SET duration_ms = NULL`); err != nil {
		t.Fatal(err)
	}
	totals, err := store.ThreadTurnTotals(context.Background(), "nth_1")
	if err != nil {
		t.Fatal(err)
	}
	if totals.Turns != 0 || totals.DurationMS != 0 {
		t.Fatalf("totals = %+v, want zero coverage", totals)
	}
}

// An unknown thread is zero coverage too — never an error the detail response
// would have to decide what to do with.
func TestThreadTurnTotalsUnknownThread(t *testing.T) {
	store := turnTotalsFixture(t)
	totals, err := store.ThreadTurnTotals(context.Background(), "nth_missing")
	if err != nil {
		t.Fatal(err)
	}
	if totals.Turns != 0 {
		t.Fatalf("totals = %+v, want zero coverage", totals)
	}
}
