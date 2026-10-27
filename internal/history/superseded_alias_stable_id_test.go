package history

import (
	"context"
	"testing"
	"time"
)

func TestSupersededCollectionKeepsAliasesWhoseRowStillLives(t *testing.T) {
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	threadID := "thread-stable-alias"
	insertCollectorThread(t, store, rootID, threadID)
	oldID := insertCollectorProjection(t, store, sourceID, generation, threadID, "stable-old", "superseded", 2)
	activeID := insertCollectorProjection(t, store, sourceID, generation, threadID, "stable-active", "active", 1)
	const shared, orphan = "nit_shared_canonical", "stable-old-item-00001"
	for _, projection := range []struct {
		id  int64
		seq int
	}{{oldID, 3}, {activeID, 2}} {
		if _, err := store.db.Exec(`INSERT INTO conversation_items(
projection_id,item_id,projected_seq,source_key,turn_key,turn_ordinal,item_type,
item_status,display_text,detail_json,filter_tags_json,source_line,source_row_type,
source_line_sha256,parsed_json)
VALUES(?,?,?,'source-shared','turn',1,'agent_message','completed','shared','{}','[]',?,'collector',?,'{}')`,
			projection.id, shared, projection.seq, projection.seq, "00"); err != nil {
			t.Fatal(err)
		}
	}
	for old, canonical := range map[string]string{"legacy-live-item": shared, "legacy-orphan-item": orphan} {
		if _, err := store.db.Exec(`INSERT INTO conversation_item_aliases(thread_id,old_item_id,canonical_item_id,source_key,created_at) VALUES(?,?,?,?,?)`,
			threadID, old, canonical, "source-"+old, formatTime(time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}

	drainSupersededForTest(t, store)

	resolved, err := store.ResolveTranscriptItemID(context.Background(), threadID, "legacy-live-item")
	if err != nil || resolved != shared {
		t.Fatalf("alias to a row still in the active generation resolves to %q (%v), want %q", resolved, err, shared)
	}
	var orphans int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_aliases WHERE thread_id=? AND old_item_id='legacy-orphan-item'`, threadID).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatal("an alias whose canonical row exists in no remaining generation was kept")
	}
}
