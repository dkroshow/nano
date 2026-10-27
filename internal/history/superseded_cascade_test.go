package history

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSupersededCollectorTableOrderCoversProjectionOwnedTables(t *testing.T) {
	store, _, _, _ := newCollectorTestStore(t)
	rows, err := store.db.Query(`
SELECT DISTINCT m.name
FROM sqlite_master m, pragma_table_info(m.name) p
WHERE m.type='table' AND p.name='projection_id'
ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var schemaTables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		schemaTables = append(schemaTables, table)
	}
	ordered := append([]string(nil), supersededProjectionTables...)
	sorted := append([]string(nil), ordered...)
	slices.Sort(sorted)
	if !reflect.DeepEqual(sorted, schemaTables) {
		t.Fatalf("collector tables=%v; projection-owned schema tables=%v", sorted, schemaTables)
	}
	wantOrder := []string{
		"conversation_parser_state_entries",
		"conversation_item_observations",
		"conversation_turn_observations",
		"conversation_item_bodies",
		"conversation_search_documents",
		"conversation_parse_diagnostics",
		"conversation_relations",
		"conversation_parse_identities",
		"conversation_observation_aliases",
		"conversation_turn_aliases",
		"conversation_observations",
		"conversation_items",
		"conversation_turns",
		"conversation_parse_values",
	}
	if !reflect.DeepEqual(ordered, wantOrder) {
		t.Fatalf("collector order=%v, want child-first order=%v", ordered, wantOrder)
	}
	for _, table := range ordered {
		plans, err := store.db.Query(fmt.Sprintf(`EXPLAIN QUERY PLAN SELECT rowid FROM %s WHERE projection_id=? LIMIT ?`, table), 1, maxSupersededProjectionBatchItems)
		if err != nil {
			t.Fatal(err)
		}
		usesProjectionIndex := false
		for plans.Next() {
			var id, parent, unused int
			var detail string
			if err := plans.Scan(&id, &parent, &unused, &detail); err != nil {
				plans.Close()
				t.Fatal(err)
			}
			usesProjectionIndex = usesProjectionIndex || strings.Contains(detail, "SEARCH "+table)
		}
		plans.Close()
		if !usesProjectionIndex {
			t.Fatalf("%s projection delete does not use a projection-leading index", table)
		}
	}
}

func newSupersededCascadeFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	insertCollectorThread(t, store, rootID, "cascade-thread")
	oldID := insertCollectorProjection(t, store, sourceID, generation, "cascade-thread", "cascade-old", "superseded", 1)
	activeID := insertCollectorProjection(t, store, sourceID, generation, "cascade-thread", "cascade-active", "active", 1)

	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for projectionID, count := range map[int64]int{oldID: 1_000, activeID: 30_000} {
		value, err := tx.Prepare(`INSERT INTO conversation_parse_values(projection_id,value_key) VALUES(?,?)`)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := tx.Prepare(`INSERT INTO conversation_observations(
projection_id,key_id,scope_id,source_id,block_path_id,source_line,source_frame,
byte_start,byte_length,source_sha256,facts_blob,sha256)
VALUES(?,?,?,?,?,?,0,0,1,?,'{}',?)`)
		if err != nil {
			value.Close()
			t.Fatal(err)
		}
		itemObservation, err := tx.Prepare(`INSERT INTO conversation_item_observations(projection_id,item_id,observation_id) VALUES(?,?,?)`)
		if err != nil {
			value.Close()
			observation.Close()
			t.Fatal(err)
		}
		turnObservation, err := tx.Prepare(`INSERT INTO conversation_turn_observations(projection_id,turn_key,observation_id) VALUES(?,'turn',?)`)
		if err != nil {
			value.Close()
			observation.Close()
			itemObservation.Close()
			t.Fatal(err)
		}
		itemID := "cascade-old-item-00001"
		if projectionID == activeID {
			itemID = "cascade-active-item-00001"
		}
		for index := 0; index < count; index++ {
			valueResult, err := value.Exec(projectionID, fmt.Sprintf("value-%d", index))
			if err != nil {
				t.Fatal(err)
			}
			valueID, err := valueResult.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			observationResult, err := observation.Exec(projectionID, valueID, valueID, valueID, valueID, index+1, []byte{1}, []byte{2})
			if err != nil {
				t.Fatal(err)
			}
			observationID, err := observationResult.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := itemObservation.Exec(projectionID, itemID, observationID); err != nil {
				t.Fatal(err)
			}
			if _, err := turnObservation.Exec(projectionID, observationID); err != nil {
				t.Fatal(err)
			}
		}
		value.Close()
		observation.Close()
		itemObservation.Close()
		turnObservation.Close()
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return store, oldID
}

func TestSupersededCollectorAvoidsCascadeScans(t *testing.T) {
	store, oldID := newSupersededCascadeFixture(t)
	started := time.Now()
	batch, err := store.CollectSupersededProjectionsBatch(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("one bounded collection pass took %s; foreign-key cascade scans are still active", elapsed)
	}
	if batch.Collected != 1 {
		t.Fatalf("collected=%d, want 1", batch.Collected)
	}
	if len(batch.Progress) != 1 || batch.Progress[0].ProjectionID != oldID {
		t.Fatalf("progress=%+v, want projection %d", batch.Progress, oldID)
	}
	progress := make(map[string]int64)
	for _, table := range batch.Progress[0].Tables {
		progress[table.Table] = table.Rows
	}
	for table, want := range map[string]int64{
		"conversation_item_observations": 1_000,
		"conversation_turn_observations": 1_000,
		"conversation_observations":      1_000,
		"conversation_parse_values":      1_000,
		"conversation_items":             1,
		"conversation_turns":             1,
		"conversation_projections":       1,
	} {
		if progress[table] != want {
			t.Fatalf("progress[%s]=%d, want %d; all progress=%v", table, progress[table], want, progress)
		}
	}
	rows, err := store.db.Query(`
SELECT m.name
FROM sqlite_master m, pragma_table_info(m.name) p
WHERE m.type='table' AND p.name='projection_id'
ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := store.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE projection_id=?`, table), oldID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows for collected projection", table, count)
		}
	}
}

func TestSupersededCollectorDisablesForeignKeysForCollectionTransaction(t *testing.T) {
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	insertCollectorThread(t, store, rootID, "foreign-key-thread")
	oldID := insertCollectorProjection(t, store, sourceID, generation, "foreign-key-thread", "foreign-key-old", "superseded", 0)
	insertCollectorProjection(t, store, sourceID, generation, "foreign-key-thread", "foreign-key-active", "active", 0)

	inside := -1
	_, eligible, err := store.supersededTx(context.Background(), oldID, func(tx *sql.Tx) (int64, error) {
		return 0, tx.QueryRowContext(context.Background(), `PRAGMA foreign_keys`).Scan(&inside)
	})
	if err != nil || !eligible {
		t.Fatalf("eligible=%v err=%v", eligible, err)
	}
	if inside != 0 {
		t.Fatalf("foreign_keys=%d inside collection transaction, want 0", inside)
	}
	var restored int
	if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("foreign_keys=%d after collection transaction, want 1", restored)
	}
}
