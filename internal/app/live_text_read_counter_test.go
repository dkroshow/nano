package app

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

var liveReadCounterID atomic.Uint64

// registerLiveReadCounter measures the real saved-page SELECT, including
// duplicate queries. It is registered before the store opens its connections.
func registerLiveReadCounter(t *testing.T, counter *atomic.Int64, fail *atomic.Bool) string {
	t.Helper()
	name := fmt.Sprintf("live_page_read_%d", liveReadCounterID.Add(1))
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if fail != nil && fail.Swap(false) {
			return nil, fmt.Errorf("synthetic read failure")
		}
		counter.Add(1)
		return int64(-1), nil
	}); err != nil {
		t.Fatal(err)
	}
	return name
}

// installLiveReadCounter preserves the real table and observes each page
// through a view. The LIMIT expression executes once even for an empty table.
func installLiveReadCounter(t *testing.T, path, name string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`ALTER TABLE conversation_item_alias_conflicts RENAME TO observed_alias_conflicts`,
		`CREATE VIEW conversation_item_alias_conflicts AS SELECT * FROM observed_alias_conflicts LIMIT ` + name + `()`,
		`CREATE TRIGGER observed_alias_insert INSTEAD OF INSERT ON conversation_item_alias_conflicts BEGIN INSERT INTO observed_alias_conflicts VALUES(NEW.thread_id,NEW.old_item_id,NEW.existing_canonical_item_id,NEW.candidate_canonical_item_id,NEW.source_key,NEW.detected_at); END`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}
