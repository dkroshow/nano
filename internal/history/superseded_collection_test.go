package history

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Enough superseded generations for about twenty batches of 64, so a write has
// room to land between batches. The 2026-09 incident had 7,207; draining that
// many took two to three minutes and outran the hang guard under load in the
// final check, while the batching and yielding it shows do not depend on size.
const supersededFixtureThreadCount = 640

func TestSupersededProjectionIndexesMigrateAdditively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX conversation_projections_state_thread_id; DELETE FROM schema_migrations WHERE version=23`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var migration, index int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=23`).Scan(&migration); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='conversation_projections_state_thread_id'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if migration != 1 || index != 1 {
		t.Fatalf("migration rows=%d index rows=%d, want 1 each", migration, index)
	}
	projectionPlan := explainQueryPlan(t, store, `SELECT id FROM conversation_projections WHERE thread_id='thread' AND state='superseded' ORDER BY id DESC LIMIT -1 OFFSET 1`)
	if !strings.Contains(projectionPlan, "conversation_projections_state_thread_id") || strings.Contains(projectionPlan, "SCAN conversation_projections") {
		t.Fatalf("superseded lookup plan = %s", projectionPlan)
	}
	itemPlan := explainQueryPlan(t, store, `DELETE FROM conversation_items WHERE projection_id IN (1,2,3)`)
	if !strings.Contains(itemPlan, "INDEX") || !strings.Contains(itemPlan, "projection_id=?)") || strings.Contains(itemPlan, "SCAN conversation_items") {
		t.Fatalf("item deletion lost its projection-leading index: %s", itemPlan)
	}
}

func explainQueryPlan(t *testing.T, store *Store, statement string) string {
	t.Helper()
	rows, err := store.db.Query("EXPLAIN QUERY PLAN " + statement)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "; ")
}

func newSupersededCollectionFixture(t *testing.T) (*Store, []string, TranscriptApplyRequest) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	input := testSource(root, "fixture.jsonl", "native-superseded", ThreadMain, nil)
	input.Source.Size = 1
	applied, err := store.ApplySource(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, root)
	stored, ok, err := store.LoadSource(ctx, rootID, input.Source.Path)
	if err != nil || !ok {
		t.Fatalf("load fixture source: found=%v err=%v", ok, err)
	}
	writerInput := testSource(root, "writer.jsonl", "native-writer", ThreadMain, nil)
	writerInput.Source.Size = 1
	writerApplied, err := store.ApplySource(ctx, writerInput)
	if err != nil {
		t.Fatal(err)
	}
	writerStored, ok, err := store.LoadSource(ctx, rootID, writerInput.Source.Path)
	if err != nil || !ok {
		t.Fatalf("load writer source: found=%v err=%v", ok, err)
	}
	writer := TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: writerApplied.ThreadID,
		SourceID: writerStored.ID, SourceGeneration: writerStored.Generation,
		Provider: ProviderCodex, ParserVersion: "hotfix-writer",
		CompletionRuleVersion: "hotfix-writer",
		Result:                TranscriptProjectionResult{ProjectionKey: "hotfix-writer", ParserState: []byte(`{}`)},
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	thread, err := tx.PrepareContext(ctx, `INSERT INTO threads(
id,source_root_id,provider,native_thread_id,kind,metadata_json,source_status)
VALUES(?,?,? ,?,'main','{}','present')`)
	if err != nil {
		t.Fatal(err)
	}
	defer thread.Close()
	projection, err := tx.PrepareContext(ctx, `INSERT INTO conversation_projections(
projection_key,thread_id,source_file_id,source_generation,provider,parser_version,
completion_rule_version,schema_version,taxonomy_version,state,complete_offset,
complete_lines,parser_state_json,item_count,latest_seq,last_indexed_at,read_offset)
VALUES(?,?,?,?,?,'fixture','fixture',1,'fixture',?,0,0,'{}',?,?,?,0)`)
	if err != nil {
		t.Fatal(err)
	}
	defer projection.Close()
	turn, err := tx.PrepareContext(ctx, `INSERT INTO conversation_turns(
projection_id,turn_key,ordinal,start_seq,end_seq,status,completion_evidence,config_json,usage_native_json,parsed_json)
VALUES(?,'turn',1,1,1,'completed','fixture','{}','{}','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	defer turn.Close()
	item, err := tx.PrepareContext(ctx, `INSERT INTO conversation_items(
projection_id,item_id,projected_seq,source_key,turn_key,turn_ordinal,item_type,
item_status,display_text,detail_json,filter_tags_json,source_line,source_row_type,
source_line_sha256,parsed_json)
VALUES(?,'item',1,'source','turn',1,'agent_message','completed','fixture','{}','[]',1,'fixture',?,'{}')`)
	if err != nil {
		t.Fatal(err)
	}
	defer item.Close()
	now := formatTime(time.Now().UTC())
	threadIDs := make([]string, 0, supersededFixtureThreadCount)
	projectionIndex := 0
	for threadIndex := 0; threadIndex < supersededFixtureThreadCount; threadIndex++ {
		threadID := applied.ThreadID
		if threadIndex > 0 {
			threadID = fmt.Sprintf("nth_hotfix_%05d", threadIndex)
			if _, err := thread.ExecContext(ctx, threadID, rootID, ProviderCodex, fmt.Sprintf("native-hotfix-%05d", threadIndex)); err != nil {
				t.Fatal(err)
			}
		}
		threadIDs = append(threadIDs, threadID)
		for generation := 0; generation < 3; generation++ {
			state := "superseded"
			if generation == 2 {
				state = "active"
			}
			withItem := projectionIndex < 3_000
			count := 0
			if withItem {
				count = 1
			}
			result, err := projection.ExecContext(ctx, fmt.Sprintf("hotfix-%05d", projectionIndex), threadID, stored.ID, stored.Generation, ProviderCodex, state, count, count, now)
			if err != nil {
				t.Fatal(err)
			}
			id, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if withItem {
				if _, err := turn.ExecContext(ctx, id); err != nil {
					t.Fatal(err)
				}
				if _, err := item.ExecContext(ctx, id, fmt.Sprintf("%064x", projectionIndex)); err != nil {
					t.Fatal(err)
				}
			}
			projectionIndex++
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return store, threadIDs, writer
}

func TestSupersededProjectionCollectionIsBatchedAndYieldsToTranscriptWrites(t *testing.T) {
	if raceEnabled {
		t.Skip("drains a large fixture through batched deletes; minutes under -race, see race_enabled_test.go")
	}
	store, _, writer := newSupersededCollectionFixture(t)
	ctx := context.Background()
	firstBatch := make(chan struct{})
	type collectionResult struct {
		count   int
		elapsed time.Duration
	}
	collectionDone := make(chan collectionResult, 1)
	collectionErrors := make(chan error, 1)
	collectionStarted := time.Now()
	go func() {
		collected := 0
		for {
			batch, err := store.CollectSupersededProjectionsBatch(ctx, 64)
			if err != nil {
				collectionErrors <- err
				return
			}
			if batch.Collected > 64 {
				collectionErrors <- fmt.Errorf("batch collected %d projections, limit 64", batch.Collected)
				return
			}
			collected += batch.Collected
			select {
			case <-firstBatch:
			default:
				close(firstBatch)
			}
			if !batch.Remaining {
				collectionDone <- collectionResult{count: collected, elapsed: time.Since(collectionStarted)}
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-firstBatch
	writeDone := make(chan error, 1)
	go func() {
		_, err := store.ApplyTranscript(ctx, writer)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent ApplyTranscript stayed blocked behind bounded collection")
	}
	select {
	case result := <-collectionDone:
		t.Fatalf("collection finished before the concurrent write could interleave: %+v", result)
	default:
	}
	select {
	case err := <-collectionErrors:
		t.Fatal(err)
	case result := <-collectionDone:
		if result.count != supersededFixtureThreadCount*2 {
			t.Fatalf("collected %d projections, want %d", result.count, supersededFixtureThreadCount*2)
		}
		t.Logf("bounded collection duration: %s; concurrent ApplyTranscript completed between batches", result.elapsed)
	// Hang guard, not a performance bound: T1b deletes thirteen tables per
	// generation in bounded batches, and the final check runs this beside every
	// other package.
	case <-time.After(5 * time.Minute):
		t.Fatal("bounded superseded collection did not finish within the hang guard")
	}
	var retained int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='superseded'`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 0 {
		t.Fatalf("retained %d superseded projections, want 0", retained)
	}
}

func newCollectorTestStore(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	input := testSource(root, "collector.jsonl", "collector-native", ThreadMain, nil)
	input.Source.Size = 1
	if _, err := store.ApplySource(ctx, input); err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, root)
	stored, ok, err := store.LoadSource(ctx, rootID, input.Source.Path)
	if err != nil || !ok {
		t.Fatalf("load collector source: found=%v err=%v", ok, err)
	}
	return store, rootID, stored.ID, stored.Generation
}

func insertCollectorThread(t *testing.T, store *Store, rootID int64, threadID string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO threads(
id,source_root_id,provider,native_thread_id,kind,metadata_json,source_status)
VALUES(?,?,?,?,'main','{}','present')`, threadID, rootID, ProviderCodex, "native-"+threadID); err != nil {
		t.Fatal(err)
	}
}

func insertCollectorProjection(t *testing.T, store *Store, sourceID, generation int64, threadID, key, state string, itemCount int) int64 {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO conversation_projections(
projection_key,thread_id,source_file_id,source_generation,provider,parser_version,
completion_rule_version,schema_version,taxonomy_version,state,complete_offset,
complete_lines,parser_state_json,item_count,latest_seq,last_indexed_at,read_offset)
VALUES(?,?,?,?,?,'collector','collector',1,'collector',?,0,0,'{}',?,?,?,0)`,
		key, threadID, sourceID, generation, ProviderCodex, state, itemCount, itemCount, formatTime(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	projectionID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if itemCount > 0 {
		if _, err := tx.Exec(`INSERT INTO conversation_turns(
projection_id,turn_key,ordinal,start_seq,end_seq,status,completion_evidence,config_json,usage_native_json,parsed_json)
VALUES(?,'turn',1,1,?,'completed','collector','{}','{}','{}')`, projectionID, itemCount); err != nil {
			t.Fatal(err)
		}
		statement, err := tx.Prepare(`INSERT INTO conversation_items(
projection_id,item_id,projected_seq,source_key,turn_key,turn_ordinal,item_type,
item_status,display_text,detail_json,filter_tags_json,source_line,source_row_type,
source_line_sha256,parsed_json)
VALUES(?,?,?,?, 'turn',1,'agent_message','completed','collector','{}','[]',?,'collector',?,'{}')`)
		if err != nil {
			t.Fatal(err)
		}
		for index := 1; index <= itemCount; index++ {
			itemID := fmt.Sprintf("%s-item-%05d", key, index)
			if _, err := statement.Exec(projectionID, itemID, index, "source-"+itemID, index, fmt.Sprintf("%064x", index)); err != nil {
				statement.Close()
				t.Fatal(err)
			}
		}
		if err := statement.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return projectionID
}

func drainSupersededForTest(t *testing.T, store *Store) {
	t.Helper()
	for pass := 0; pass < 100; pass++ {
		batch, err := store.CollectSupersededProjectionsBatch(context.Background(), 64)
		if err != nil {
			t.Fatal(err)
		}
		if !batch.Remaining {
			return
		}
	}
	t.Fatal("superseded collection did not drain")
}

func TestSupersededCollectorDrainsOldestAcrossThreads(t *testing.T) {
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	for _, fixture := range []struct {
		threadID string
		items    int
	}{
		{threadID: "thread-oversized", items: maxSupersededProjectionBatchItems + 1},
		{threadID: "thread-small-a", items: 1},
		{threadID: "thread-small-b", items: 2},
	} {
		insertCollectorThread(t, store, rootID, fixture.threadID)
		insertCollectorProjection(t, store, sourceID, generation, fixture.threadID, fixture.threadID+"-old", "superseded", fixture.items)
		insertCollectorProjection(t, store, sourceID, generation, fixture.threadID, fixture.threadID+"-active", "active", 1)
	}
	first, err := store.CollectSupersededProjectionsBatch(context.Background(), 64)
	if err != nil {
		t.Fatal(err)
	}
	if first.Collected != 1 {
		t.Fatalf("first batch collected=%d, want oldest oversized generation alone", first.Collected)
	}
	var oversized int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE projection_key='thread-oversized-old'`).Scan(&oversized); err != nil {
		t.Fatal(err)
	}
	if oversized != 0 {
		t.Fatal("oldest oversized generation was not collected first")
	}

	drainSupersededForTest(t, store)
	var superseded, active int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='superseded'`).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='active' AND thread_id LIKE 'thread-%'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if superseded != 0 || active != 3 {
		t.Fatalf("superseded=%d active=%d, want 0 and 3", superseded, active)
	}
}

func activeGenerationSnapshot(t *testing.T, store *Store, threadID string, projectionID int64) []string {
	t.Helper()
	queries := []struct {
		statement string
		args      []any
	}{
		{`SELECT group_concat(hex(CAST(projection_key||':'||state||':'||parser_state_json AS BLOB)), ',') FROM conversation_projections WHERE id=?`, []any{projectionID}},
		{`SELECT group_concat(hex(CAST(item_id||':'||source_key||':'||display_text||':'||detail_json||':'||parsed_json AS BLOB)), ',') FROM conversation_items WHERE projection_id=? ORDER BY projected_seq`, []any{projectionID}},
		{`SELECT group_concat(hex(CAST(turn_key||':'||status||':'||config_json||':'||parsed_json AS BLOB)), ',') FROM conversation_turns WHERE projection_id=? ORDER BY ordinal`, []any{projectionID}},
		{`SELECT group_concat(hex(CAST(item_id||':'||content_ref||':'||body_text||':'||body_json AS BLOB)), ',') FROM conversation_item_bodies WHERE projection_id=? ORDER BY item_id`, []any{projectionID}},
		{`SELECT group_concat(hex(value_key), ',') FROM conversation_parse_values WHERE projection_id=? ORDER BY id`, []any{projectionID}},
		{`SELECT group_concat(hex(CAST(source_line||':'||facts_blob AS BLOB)), ',') FROM conversation_observations WHERE projection_id=? ORDER BY id`, []any{projectionID}},
		{`SELECT group_concat(hex(CAST(old_item_id||':'||canonical_item_id||':'||source_key AS BLOB)), ',') FROM conversation_item_aliases WHERE thread_id=? AND canonical_item_id IN (SELECT item_id FROM conversation_items WHERE projection_id=?) ORDER BY old_item_id`, []any{threadID, projectionID}},
	}
	result := make([]string, 0, len(queries))
	for _, query := range queries {
		var value string
		if err := store.db.QueryRow(query.statement, query.args...).Scan(&value); err != nil {
			t.Fatal(err)
		}
		result = append(result, value)
	}
	return result
}

func TestSupersededCollectorNeverTouchesActiveGeneration(t *testing.T) {
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	threadID := "thread-active-proof"
	insertCollectorThread(t, store, rootID, threadID)
	insertCollectorProjection(t, store, sourceID, generation, threadID, "active-proof-old", "superseded", 1)
	activeID := insertCollectorProjection(t, store, sourceID, generation, threadID, "active-proof-active", "active", 1)
	if _, err := store.db.Exec(`INSERT INTO conversation_item_bodies(
projection_id,item_id,content_ref,body_text,body_json,original_bytes,stored_bytes,truncated)
VALUES(?,?,?,'body',X'7B7D',4,4,0)`, activeID, "active-proof-active-item-00001", "active-body"); err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{{1}, {2}, {3}, {4}} {
		if _, err := store.db.Exec(`INSERT INTO conversation_parse_values(projection_id,value_key) VALUES(?,?)`, activeID, value); err != nil {
			t.Fatal(err)
		}
	}
	var firstValueID int64
	if err := store.db.QueryRow(`SELECT MIN(id) FROM conversation_parse_values WHERE projection_id=?`, activeID).Scan(&firstValueID); err != nil {
		t.Fatal(err)
	}
	observation, err := store.db.Exec(`INSERT INTO conversation_observations(
projection_id,key_id,scope_id,source_id,block_path_id,source_line,source_frame,
byte_start,byte_length,source_sha256,facts_blob,sha256)
VALUES(?,?,?,?,?,1,0,0,4,?,X'6661637473',?)`, activeID, firstValueID, firstValueID+1, firstValueID+2, firstValueID+3, make([]byte, 32), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	observationID, err := observation.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO conversation_item_observations(projection_id,item_id,observation_id) VALUES(?,?,?)`, activeID, "active-proof-active-item-00001", observationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO conversation_item_aliases(thread_id,old_item_id,canonical_item_id,source_key,created_at) VALUES(?,?,?,?,?)`, threadID, "legacy-active-item", "active-proof-active-item-00001", "legacy-active-source", formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	before := activeGenerationSnapshot(t, store, threadID, activeID)

	drainSupersededForTest(t, store)
	after := activeGenerationSnapshot(t, store, threadID, activeID)
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("active generation changed\nbefore=%q\nafter=%q", before, after)
	}
	var superseded int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE thread_id=? AND state='superseded'`, threadID).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if superseded != 0 {
		t.Fatalf("superseded generations remaining=%d, want 0", superseded)
	}
}

func TestItemAliasesAreCollectedWithTheirGeneration(t *testing.T) {
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	threadID := "thread-alias-proof"
	insertCollectorThread(t, store, rootID, threadID)
	oldID := insertCollectorProjection(t, store, sourceID, generation, threadID, "alias-old", "superseded", 1)
	insertCollectorProjection(t, store, sourceID, generation, threadID, "alias-newer", "superseded", 0)
	insertCollectorProjection(t, store, sourceID, generation, threadID, "alias-active", "active", 1)
	var canonical string
	if err := store.db.QueryRow(`SELECT item_id FROM conversation_items WHERE projection_id=?`, oldID).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO conversation_item_aliases(thread_id,old_item_id,canonical_item_id,source_key,created_at) VALUES(?,?,?,?,?)`, threadID, "legacy-dead-item", canonical, "legacy-dead-source", formatTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	drainSupersededForTest(t, store)
	var dangling int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_aliases alias
WHERE NOT EXISTS (
  SELECT 1 FROM conversation_items item
  JOIN conversation_projections projection ON projection.id=item.projection_id
  WHERE projection.thread_id=alias.thread_id AND item.item_id=alias.canonical_item_id
)`).Scan(&dangling); err != nil {
		t.Fatal(err)
	}
	if dangling != 0 {
		t.Fatalf("dangling alias rows=%d, want 0", dangling)
	}
}
