package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func codexTurnRows(n int) string {
	return strings.Join([]string{
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"p%d"}}`, n),
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"user_message","message":"Prompt %d"}}`, n),
		fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":"u%d","role":"user","content":"Prompt %d"}}`, n, n),
		fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":"a%d","role":"assistant","content":"Answer %d"}}`, n, n),
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"p%d"}}`, n),
	}, "\n") + "\n"
}

// The live store held 7 GB of items that belonged to each file's latest,
// already completed turn, written before the readers' retention rule changed.
// The background pass rewrites such a checkpoint through its reader, marks it,
// and the reader still continues the file from it.
func TestCompactParserStateBatchRewritesStoredCheckpointsOnce(t *testing.T) {
	ctx := context.Background()
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	insertCollectorThread(t, store, rootID, "compaction-thread")
	projectionID := insertCollectorProjection(t, store, sourceID, generation, "compaction-thread", "compaction-key", "active", 1)

	reader, err := newModuleFileReader(ProviderCodex, nil)
	if err != nil {
		t.Fatal(err)
	}
	input := model.Input{Source: model.SourceRef{Label: "compaction.jsonl", Incarnation: "compaction-key"}}
	for turn := 1; turn <= 2; turn++ {
		input.Bytes = []byte(codexTurnRows(turn))
		if _, err := reader.feed(input); err != nil {
			t.Fatal(err)
		}
	}
	slim, _, _, _, err := reader.save()
	if err != nil {
		t.Fatal(err)
	}
	// The stored shape before the rule: the completed turn's tool output too.
	var state map[string]json.RawMessage
	if err := json.Unmarshal(slim, &state); err != nil {
		t.Fatal(err)
	}
	var items map[string]json.RawMessage
	if err := json.Unmarshal(state["items"], &items); err != nil {
		t.Fatal(err)
	}
	var turnKey string
	for _, item := range items {
		var brief struct {
			TurnKey string `json:"turnKey"`
		}
		if err := json.Unmarshal(item, &brief); err != nil {
			t.Fatal(err)
		}
		turnKey = brief.TurnKey
	}
	if len(items) != 1 || turnKey == "" {
		t.Fatalf("fresh checkpoint retains %d items, want the final answer only", len(items))
	}
	fat, _ := json.Marshal(map[string]any{"id": "local:fat-output", "turnKey": turnKey, "type": "tool_result", "text": strings.Repeat("output ", 20000), "scope": json.RawMessage(`{"kind":"item","provider":"codex"}`)})
	items["local:fat-output"] = fat
	state["items"], _ = json.Marshal(items)
	fatReader, _ := json.Marshal(state)
	raw, err := json.Marshal(parsedFileCheckpoint{Version: parsedFileVersion, Provider: ProviderCodex, Source: "compaction.jsonl", ProjectionKey: "compaction-key", Format: "jsonl", Reader: fatReader, Metadata: ThreadProjection{Provider: ProviderCodex}})
	if err != nil {
		t.Fatal(err)
	}
	base, entries, err := splitTranscriptContinuation(raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_projections SET parser_state_json=? WHERE id=?`, base, projectionID); err != nil {
		t.Fatal(err)
	}
	if err := syncContinuationEntries(ctx, tx, projectionID, entries); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	batch, err := store.CompactParserStateBatch(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Projections != 1 || batch.Remaining || batch.BytesAfter >= batch.BytesBefore || batch.BytesBefore < 140000 {
		t.Fatalf("compaction batch = %+v", batch)
	}
	stored, err := loadContinuationEntries(ctx, store.db, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range stored {
		if entry.field == "items" && entry.key == "local:fat-output" {
			t.Fatal("compaction kept the completed turn's tool output")
		}
	}
	var compactedAt string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(parser_retention_checked_at,'') FROM conversation_projections WHERE id=?`, projectionID).Scan(&compactedAt); err != nil || compactedAt == "" {
		t.Fatalf("projection not marked compacted: %q %v", compactedAt, err)
	}
	if again, err := store.CompactParserStateBatch(ctx, 8); err != nil || again.Projections != 0 || again.Remaining {
		t.Fatalf("second pass = %+v, %v", again, err)
	}

	checkpoint, ok, err := store.LoadTranscriptCheckpoint(ctx, "compaction-thread")
	if err != nil || !ok {
		t.Fatalf("reload checkpoint: ok=%v err=%v", ok, err)
	}
	var restored parsedFileCheckpoint
	if err := json.Unmarshal(checkpoint.ParserState, &restored); err != nil {
		t.Fatal(err)
	}
	continued, err := newModuleFileReader(ProviderCodex, restored.Reader)
	if err != nil {
		t.Fatal(err)
	}
	input.Bytes = []byte(codexTurnRows(3))
	next, err := continued.feed(input)
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]bool{}
	for _, change := range next.Changes {
		if change.Item != nil {
			texts[change.Item.Text] = true
		}
	}
	if !texts["Prompt 3"] || !texts["Answer 3"] {
		t.Fatalf("compacted checkpoint did not continue the file: %v", texts)
	}
}

// A checkpoint an older reader wrote decodes as JSON but its reader refuses
// it; left unvisited it stopped every pass at the same row.
func TestCompactParserStateBatchSkipsIncompatibleReaderCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, rootID, sourceID, generation := newCollectorTestStore(t)
	insertCollectorThread(t, store, rootID, "old-reader-thread")
	projectionID := insertCollectorProjection(t, store, sourceID, generation, "old-reader-thread", "old-reader-key", "active", 1)
	reader, err := newModuleFileReader(ProviderCodex, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.feed(model.Input{Source: model.SourceRef{Label: "old.jsonl", Incarnation: "old-reader-key"}}); err != nil {
		t.Fatal(err)
	}
	raw, _, _, _, err := reader.save()
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state["version"] = json.RawMessage(`21`)
	old, _ := json.Marshal(state)
	checkpoint, _ := json.Marshal(parsedFileCheckpoint{Version: parsedFileVersion, Provider: ProviderCodex, Reader: old})
	if _, err := store.db.ExecContext(ctx, `UPDATE conversation_projections SET parser_state_json=? WHERE id=?`, checkpoint, projectionID); err != nil {
		t.Fatal(err)
	}
	batch, err := store.CompactParserStateBatch(ctx, 8)
	if err != nil || batch.Skipped != 1 || batch.Projections != 0 || batch.Remaining {
		t.Fatalf("batch = %+v, %v", batch, err)
	}
	if again, err := store.CompactParserStateBatch(ctx, 8); err != nil || again.Skipped != 0 || again.Projections != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}
}
