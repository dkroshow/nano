package history

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTranscriptModuleFixtures(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		root     RootKind
		name     string
		items    int64
	}{
		{ProviderClaude, RootClaudeProjects, "tool-call-completeness-and-failure", 2},
		{ProviderCodex, RootCodexSessions, "edge-usage-three-grains", 0},
		{ProviderGrok, RootGrokSessions, "duplicate-versus-identical-delta", 1},
	} {
		t.Run(string(tc.provider)+"/"+tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../transcriptparse", string(tc.provider), "testdata/research", tc.name, "input.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			input := model.Input{Bytes: data, Source: model.SourceRef{Label: "fixture", Incarnation: "1"}}
			var batch model.Batch
			switch tc.provider {
			case ProviderClaude:
				_, batch, err = claude.ParseFile(claude.State{}, input)
			case ProviderCodex:
				_, batch, err = codex.ParseFile(codex.State{}, input)
			case ProviderGrok:
				var state grok.State
				for _, frame := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
					input.Bytes = frame
					var next model.Batch
					state, next, err = grok.ParseWire(state, input)
					if err != nil {
						break
					}
					batch.Versions = next.Versions
					batch.Changes = append(batch.Changes, next.Changes...)
					batch.Observations = append(batch.Observations, next.Observations...)
					batch.Diagnostics = append(batch.Diagnostics, next.Diagnostics...)
					batch.Records = append(batch.Records, next.Records...)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			root := SourceRoot{Provider: tc.provider, Kind: tc.root, Path: t.TempDir()}
			metadata := testSource(root, "fixture.jsonl", "fixture", ThreadMain, nil)
			thread, err := store.ApplySource(ctx, metadata)
			if err != nil {
				t.Fatal(err)
			}
			source, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
			if err != nil {
				t.Fatal(err)
			}
			written, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: thread.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: tc.provider, ParserVersion: batch.Versions.Reader, CompletionRuleVersion: batch.Versions.Assembly, Result: TranscriptProjectionResult{ProjectionKey: "fixture", CompleteOffset: int64(len(data)), CompleteLines: len(batch.Records), ParserState: []byte(`{}`), parsedBatches: func(yield func(model.Batch) error) error { return yield(batch) }}})
			if err != nil || written.ItemCount != tc.items {
				t.Fatalf("fixture apply: %+v %v", written, err)
			}
			var projection int64
			if err := store.db.QueryRow(`SELECT id FROM conversation_projections WHERE projection_key='fixture'`).Scan(&projection); err != nil {
				t.Fatal(err)
			}
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			switch tc.provider {
			case ProviderClaude:
				rows, err := tx.Query(`SELECT item_id,parsed_json FROM conversation_items WHERE projection_id=? ORDER BY projected_seq`, projection)
				if err != nil {
					t.Fatal(err)
				}
				var items []model.Item
				var ids []string
				for rows.Next() {
					var raw []byte
					var id string
					if err := rows.Scan(&id, &raw); err != nil {
						t.Fatal(err)
					}
					item, err := decodeParsedItemTx(ctx, tx, projection, raw)
					if err != nil {
						t.Fatal(err)
					}
					items, ids = append(items, item), append(ids, id)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				if len(items) != 2 || !items[0].Complete || items[0].Outcome != model.OutcomeUnknown || items[1].Outcome != model.OutcomeFailed {
					t.Fatal("tool result changed invocation completion/outcome")
				}
				var from, to string
				if err := tx.QueryRow(`SELECT from_item_id,to_item_id FROM conversation_relations WHERE projection_id=? AND relation_kind='result_of'`, projection).Scan(&from, &to); err != nil || from != ids[1] || to != ids[0] {
					t.Fatalf("result endpoints were not resolved to history rows: %v", err)
				}
			case ProviderCodex:
				var input int64
				var raw []byte
				if err := tx.QueryRow(`SELECT usage_input,parsed_json FROM conversation_turns WHERE projection_id=?`, projection).Scan(&input, &raw); err != nil || input != 10 {
					t.Fatalf("usage grain changed: input=%d err=%v", input, err)
				}
				turn, err := decodeParsedTurnTx(ctx, tx, projection, raw)
				if err != nil {
					t.Fatal(err)
				}
				grains := map[model.UsageGrain]int64{}
				for _, sample := range turn.UsageObservations {
					if sample.Values.Input != nil {
						grains[sample.Grain] = *sample.Values.Input
					}
				}
				if !reflect.DeepEqual(grains, map[model.UsageGrain]int64{model.UsageResponse: 10, model.UsageTurn: 10, model.UsageThread: 100}) {
					t.Fatalf("usage facts lost their native grain: %v", grains)
				}
			case ProviderGrok:
				var text string
				var observations, replays int
				if err := tx.QueryRow(`SELECT display_text FROM conversation_items WHERE projection_id=?`, projection).Scan(&text); err != nil || text != "AA" {
					t.Fatalf("delta/replay text=%q err=%v", text, err)
				}
				if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_item_observations WHERE projection_id=?`, projection).Scan(&observations); err != nil || observations != 3 {
					t.Fatalf("delta/replay observations=%d err=%v", observations, err)
				}
				if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_relations WHERE projection_id=? AND relation_kind='replay_of'`, projection).Scan(&replays); err != nil || replays != 1 {
					t.Fatalf("delta/replay relationships=%d err=%v", replays, err)
				}
			}
		})
	}
}
