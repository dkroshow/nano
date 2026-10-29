package history

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The opt-in source is the frozen receipt 00245-0821da629feb. It remains
// private scratch; this test prints only identities' counts and source hashes.
func TestFrozenClaudeReplayIdentityReceipts(t *testing.T) {
	path := os.Getenv("NANO_LATE_REPLAY_FIXTURE")
	if path == "" {
		t.Skip("opt-in audit against a local corpus; set NANO_LATE_REPLAY_FIXTURE to run")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "0821da629feb99e59581926dca9c15712d1a569cc8c34ec58ffa7b85b73212e0" {
		t.Fatal("frozen late-replay source changed")
	}
	lines := bytes.Split(raw, []byte("\n"))
	var target struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(lines[987], &target); err != nil || target.UUID == "" {
		t.Fatal("receipt has no UUID at line 988")
	}
	if os.Getenv("NANO_LATE_REPLAY_FULL_STORE") == "1" {
		frozenClaudeReplayConvertedStore(t, lines[:1229], target.UUID)
		return
	}
	for _, wire := range []bool{false, true} {
		name := "file"
		if wire {
			name = "wire"
		}
		t.Run(name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			projection := identityStoreProjection(t, store, "late-replay")
			ctx := context.Background()
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var state claude.State
			canonical := map[string]bool{}
			observations := map[string]bool{}
			locals := map[string]bool{}
			turns := map[string]TranscriptTurn{}
			var first TranscriptItem
			for i, row := range lines[:1229] {
				input := model.Input{Bytes: append(append([]byte(nil), row...), '\n'), Source: model.SourceRef{Label: "frozen-late-replay", Incarnation: "1"}}
				var batch model.Batch
				if wire {
					state, batch, err = claude.ParseWire(state, input)
				} else {
					state, batch, err = claude.ParseFile(state, input)
				}
				if err != nil {
					t.Fatalf("read source line %d: %v", i+1, err)
				}
				for _, change := range batch.Changes {
					if change.Item == nil {
						continue
					}
					item := *change.Item
					matches := false
					for _, obs := range item.Observations {
						if obs.NativeEventID == target.UUID {
							matches = true
							observations[obs.ID] = true
						}
					}
					if !matches {
						continue
					}
					turn, ok := turns[item.TurnKey]
					if !ok {
						turn = TranscriptTurn{Key: fmt.Sprintf("view-turn-%d", len(turns)+1), Ordinal: len(turns) + 1, Status: TurnInProgress, CompletionEvidence: CompletionNone}
						turns[item.TurnKey] = turn
						if err := writeTranscriptBatch(ctx, tx, projection, TranscriptProjectionResult{Turns: []TranscriptTurn{turn}}); err != nil {
							t.Fatal(err)
						}
					}
					got, _, err := writeParsedTranscriptItemTx(ctx, tx, projection, "frozen", turn, item)
					if err != nil {
						t.Fatalf("write source line %d: %v", i+1, err)
					}
					if first.ID == "" {
						first = got
					}
					if got.ProjectedSeq != first.ProjectedSeq || got.SourceKey != first.SourceKey || got.TurnKey != first.TurnKey || got.Text != first.Text {
						t.Fatalf("replay changed first row position, owner or text at source line %d", i+1)
					}
					canonical[got.ID] = true
					locals[item.ID] = true
				}
				if i == 1100 {
					checkpoint, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					state = claude.State{}
					if err := json.Unmarshal(checkpoint, &state); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(canonical) != 1 || len(observations) != 2 {
				t.Fatalf("native replay receipts: canonical=%d observations=%d local aliases=%d", len(canonical), len(observations), len(locals))
			}
			var persisted int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_parse_identities WHERE projection_id=? AND identity_kind='local'`, projection).Scan(&persisted); err != nil || persisted != len(locals) {
				t.Fatalf("local alias count=%d expected=%d error=%v", persisted, len(locals), err)
			}
			var rows, linked, relations int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_items WHERE projection_id=?`, projection).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("persisted item rows=%d err=%v", rows, err)
			}
			for observation := range observations {
				var count int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_item_observations m JOIN conversation_observations o ON o.id=m.observation_id JOIN conversation_parse_values k ON k.id=o.key_id WHERE m.projection_id=? AND m.item_id=? AND k.value_key=?`, projection, first.ID, testTranscriptValueKey(t, observation)).Scan(&count); err != nil || count != 1 {
					t.Fatalf("target observation missing from canonical row: count=%d err=%v", count, err)
				}
				linked++
			}
			if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_relations WHERE projection_id=? AND relation_kind='replay_of'`, projection).Scan(&relations); err != nil || relations < 1 {
				t.Fatalf("persisted replay relations=%d err=%v", relations, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			t.Logf("rows=%d target_observations=%d replay_of=true local_aliases=%d first_view_sha256=%x", rows, linked, persisted, sha256.Sum256([]byte(first.SourceKey+":"+first.TurnKey+":"+first.Text)))
		})
	}
}

func frozenClaudeReplayConvertedStore(t *testing.T, lines [][]byte, target string) {
	for _, wire := range []bool{false, true} {
		name := "file"
		if wire {
			name = "wire"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			spoolPath := filepath.Join(dir, "batches.json.gz")
			spool, err := os.Create(spoolPath)
			if err != nil {
				t.Fatal(err)
			}
			compressed := gzip.NewWriter(spool)
			encoder := json.NewEncoder(compressed)
			var state claude.State
			observations, locals := map[string]bool{}, map[string]bool{}
			var offset int64
			for i, row := range lines {
				input := model.Input{Bytes: append(append([]byte(nil), row...), '\n'), Source: model.SourceRef{Label: "frozen-late-replay", Incarnation: "1"}}
				offset += int64(len(input.Bytes))
				var batch model.Batch
				if wire {
					state, batch, err = claude.ParseWire(state, input)
				} else {
					state, batch, err = claude.ParseFile(state, input)
				}
				if err != nil {
					t.Fatalf("read source line %d: %v", i+1, err)
				}
				for _, change := range batch.Changes {
					if change.Item == nil {
						continue
					}
					for _, observation := range change.Item.Observations {
						if observation.NativeEventID == target {
							observations[observation.ID], locals[change.Item.ID] = true, true
						}
					}
				}
				if err := encoder.Encode(batch); err != nil {
					t.Fatal(err)
				}
				if i == 1100 {
					checkpoint, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					state = claude.State{}
					if err := json.Unmarshal(checkpoint, &state); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			if err := spool.Close(); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			store, err := Open(filepath.Join(dir, "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			projection := identityStoreProjection(t, store, "full-replay")
			request := TranscriptApplyRequest{Mode: TranscriptRebuild, Provider: ProviderClaude, ParserVersion: "converted-replay", CompletionRuleVersion: "converted-replay"}
			if err := store.db.QueryRow(`SELECT thread_id,source_file_id,source_generation FROM conversation_projections WHERE id=?`, projection).Scan(&request.ThreadID, &request.SourceID, &request.SourceGeneration); err != nil {
				t.Fatal(err)
			}
			request.Result = TranscriptProjectionResult{ProjectionKey: "full-converted", CompleteOffset: offset, CompleteLines: len(lines), ParserState: checkpoint, parsedBatches: func(yield func(model.Batch) error) error {
				file, err := os.Open(spoolPath)
				if err != nil {
					return err
				}
				defer file.Close()
				reader, err := gzip.NewReader(file)
				if err != nil {
					return err
				}
				defer reader.Close()
				decoder := json.NewDecoder(reader)
				for line := 1; ; line++ {
					var batch model.Batch
					if err := decoder.Decode(&batch); err != nil {
						if err == io.EOF {
							return nil
						}
						return err
					}
					if err := yield(batch); err != nil {
						return fmt.Errorf("write source line %d: %w", line, err)
					}
				}
			}}
			written, err := store.ApplyTranscript(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow(`SELECT id FROM conversation_projections WHERE projection_key=?`, written.ProjectionKey).Scan(&projection); err != nil {
				t.Fatal(err)
			}
			canonical := map[string]bool{}
			for local := range locals {
				var id string
				if err := store.db.QueryRow(`SELECT a.item_id FROM conversation_parse_identities a JOIN conversation_parse_values v ON v.id=a.identity_value_id WHERE a.projection_id=? AND a.identity_kind='local' AND v.value_key=?`, projection, testTranscriptValueKey(t, local)).Scan(&id); err != nil {
					t.Fatal(err)
				}
				canonical[id] = true
			}
			if len(canonical) != 1 || len(observations) != 2 {
				t.Fatalf("canonical=%d target_observations=%d", len(canonical), len(observations))
			}
			var itemID string
			for id := range canonical {
				itemID = id
			}
			for observation := range observations {
				var count int
				if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_item_observations m JOIN conversation_observations o ON o.id=m.observation_id JOIN conversation_parse_values k ON k.id=o.key_id WHERE m.projection_id=? AND m.item_id=? AND k.value_key=?`, projection, itemID, testTranscriptValueKey(t, observation)).Scan(&count); err != nil || count != 1 {
					t.Fatalf("target observation missing: count=%d err=%v", count, err)
				}
			}
			var replays int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_relations r JOIN conversation_item_observations m ON m.projection_id=r.projection_id JOIN conversation_observations o ON o.id=m.observation_id AND o.key_id=r.from_observation_value_id WHERE r.projection_id=? AND m.item_id=? AND r.relation_kind='replay_of'`, projection, itemID).Scan(&replays); err != nil || replays == 0 {
				t.Fatalf("replays=%d err=%v", replays, err)
			}
			var seq int64
			var sourceKey, owner, text string
			if err := store.db.QueryRow(`SELECT projected_seq,source_key,turn_key,display_text FROM conversation_items WHERE projection_id=? AND item_id=?`, projection, itemID).Scan(&seq, &sourceKey, &owner, &text); err != nil {
				t.Fatal(err)
			}
			t.Logf("full_converted_rows=%d target_rows=1 target_observations=2 replay_of=true local_aliases=%d target_view_sha256=%x", written.ItemCount, len(locals), sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s:%s", seq, sourceKey, owner, text))))
		})
	}
}
