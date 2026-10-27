package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type rebuildFixture struct {
	store  *Store
	path   string
	source StoredSource
	base   TranscriptCheckpoint
	parsed *ParsedFile
	input  TranscriptApplyRequest
}

func newRebuildFixture(t *testing.T) *rebuildFixture {
	t.Helper()
	return newRebuildFixtureForProvider(t, ProviderClaude)
}

func newRebuildFixtureForProvider(t *testing.T, provider Provider) *rebuildFixture {
	t.Helper()
	ctx := context.Background()
	f := &rebuildFixture{path: filepath.Join(t.TempDir(), "history.sqlite")}
	var err error
	f.store, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	root := SourceRoot{Provider: provider, Kind: RootClaudeProjects, Path: t.TempDir()}
	if provider == ProviderGrok {
		root.Kind = RootGrokSessions
	}
	source := SourceCandidate{Root: root, Path: filepath.Join(root.Path, "fixture.jsonl"), Kind: ThreadMain, ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	// A small captured Claude JSONL shape with synthetic identities and content.
	raw := `{"type":"user","uuid":"prompt","sessionId":"native","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"alpha prompt"}}` + "\n" +
		`{"type":"assistant","uuid":"reply","sessionId":"native","timestamp":"2026-01-01T00:00:01Z","message":{"id":"message","role":"assistant","model":"model","content":[{"type":"text","text":"beta answer"}],"usage":{"input_tokens":4,"output_tokens":2},"stop_reason":"end_turn"}}` + "\n"
	if provider == ProviderGrok {
		raw = grokUpdatesFixture
		source.Path = filepath.Join(root.Path, "native", "updates.jsonl")
	}
	source.Size = int64(len(raw))
	parse := func() (*ParsedFile, error) {
		request := ParsedFileRequest{Source: source, Reader: strings.NewReader(raw), ReadSize: 128, SpoolDir: t.TempDir()}
		if provider == ProviderGrok {
			catalogSource := source
			catalogSource.Path = filepath.Join(filepath.Dir(source.Path), "summary.json")
			request.Catalog = &ParsedCatalogInput{Source: catalogSource, Reader: strings.NewReader(grokCurrentSummary), Format: "json"}
		}
		return ProjectParsedFile(ctx, request)
	}
	old, err := parse()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	applied, err := f.store.ApplySource(ctx, SourceProjection{Source: source, Result: &old.Metadata, ParserVersion: "old", Status: SourcePresent})
	if err != nil {
		t.Fatal(err)
	}
	f.source, _, err = f.store.LoadSource(ctx, mustRootID(t, f.store, root), source.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: f.source.ID, SourceGeneration: f.source.Generation, Provider: provider, ParserVersion: "old", CompletionRuleVersion: old.CompletionRuleVersion, Result: old.Transcript}); err != nil {
		t.Fatal(err)
	}
	f.base, _, err = f.store.LoadTranscriptCheckpointHeader(ctx, applied.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	f.parsed, err = parse()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.parsed.Close() })
	f.input = TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: f.source.ID, SourceGeneration: f.source.Generation, Provider: provider, ParserVersion: f.parsed.ParserVersion, CompletionRuleVersion: f.parsed.CompletionRuleVersion, ExpectedProjectionKey: f.base.ProjectionKey, Result: f.parsed.Transcript, IndexedAt: time.Now().UTC()}
	return f
}

func (f *rebuildFixture) begin(t *testing.T) *TranscriptRebuildHandle {
	t.Helper()
	build, err := f.store.BeginTranscriptRebuild(context.Background(), f.input, f.base)
	if err != nil {
		t.Fatal(err)
	}
	return build
}

func (f *rebuildFixture) complete(t *testing.T) *TranscriptRebuildHandle {
	t.Helper()
	ctx := context.Background()
	build := f.begin(t)
	if err := f.store.WriteTranscriptRebuild(ctx, build, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.IndexTranscriptRebuild(ctx, build, ThreadMain, nil); err != nil {
		t.Fatal(err)
	}
	return build
}

func (f *rebuildFixture) active(t *testing.T) string {
	t.Helper()
	var key string
	if err := f.store.db.QueryRow(`SELECT projection_key FROM conversation_projections WHERE state='active' AND thread_id=?`, f.source.ThreadID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	return key
}

// A shadow must preserve the one-pass rows and continuation, including final-answer references.
func TestTranscriptRebuildPreservesRowsAndSearchAtAtomicSwitch(t *testing.T) {
	for _, provider := range []Provider{ProviderClaude, ProviderGrok} {
		t.Run(string(provider), func(t *testing.T) {
			f := newRebuildFixtureForProvider(t, provider)
			ctx := context.Background()
			buildSearchModes(t, ctx, f.store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 10)
			before, err := f.store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: f.source.ThreadID})
			if err != nil {
				t.Fatal(err)
			}
			// Capture canonical A before injecting a historical id for the alias check.
			// This keeps the field comparison's only expected difference the turn key.
			canonicalItemID := before.Turns[0].FinalItemID
			if canonicalItemID == "" {
				t.Fatal("fixture has no public final-answer reference")
			}
			oldItemID := "legacy-" + canonicalItemID
			tx, err := f.store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"conversation_items", "conversation_item_bodies", "conversation_search_documents", "conversation_item_observations"} {
				if _, err := tx.Exec(`UPDATE `+table+` SET item_id=? WHERE projection_id=(SELECT id FROM conversation_projections WHERE state='active') AND item_id=?`, oldItemID, canonicalItemID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(`UPDATE conversation_turns SET final_item_id=? WHERE projection_id=(SELECT id FROM conversation_projections WHERE state='active') AND final_item_id=?`, oldItemID, canonicalItemID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			visit := f.input.Result.parsedBatches
			callbacks := 0
			f.input.Result.parsedBatches = func(yield func(model.Batch) error) error {
				return visit(func(batch model.Batch) error {
					err := yield(batch)
					callbacks++
					if callbacks == 1 {
						time.Sleep(backgroundWriteTarget + time.Millisecond)
					}
					return err
				})
			}
			build := f.begin(t)
			chunks := 0
			if err := f.store.WriteTranscriptRebuild(ctx, build, func() bool { chunks++; return false }); err != nil {
				t.Fatal(err)
			}
			if chunks < 2 {
				t.Fatal("preservation fixture did not cross a chunk commit")
			}
			if err := f.store.IndexTranscriptRebuild(ctx, build, ThreadMain, nil); err != nil {
				t.Fatal(err)
			}
			if f.active(t) != f.base.ProjectionKey {
				t.Fatal("partial shadow became active")
			}
			if _, err := f.store.ApplySource(ctx, SourceProjection{Source: f.source.Source, Result: &f.parsed.Metadata, ParserVersion: f.parsed.ParserVersion, Status: SourcePresent}, ApplySourceOptions{SkipThreadSearch: f.source.ThreadID}); err != nil {
				t.Fatal(err)
			}
			searchBefore, err := f.store.ConversationSearchStatuses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.store.SwitchTranscriptRebuild(ctx, build)
			if err != nil || !result.Changed {
				t.Fatal(result, err)
			}
			searchAfter, err := f.store.ConversationSearchStatuses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i, previous := range searchBefore {
				if searchAfter[i].Mode != previous.Mode || searchAfter[i].Revision != previous.Revision+1 {
					t.Fatal("switch did not publish the search revision", previous, searchAfter[i])
				}
			}
			after, err := f.store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: f.source.ThreadID})
			if err != nil {
				t.Fatal(err)
			}
			// A one-pass rebuild of this same candidate uses the same projection id,
			// so it proves whether a changed field came from chunking or today's writer.
			baseline, err := Open(filepath.Join(t.TempDir(), "one-pass.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer baseline.Close()
			sourceApplied, err := baseline.ApplySource(ctx, SourceProjection{Source: f.source.Source, Result: &f.parsed.Metadata, ParserVersion: "old", Status: SourcePresent})
			if err != nil {
				t.Fatal(err)
			}
			old := f.input
			old.ThreadID = sourceApplied.ThreadID
			old.ParserVersion = "old"
			old.ExpectedProjectionKey = ""
			old.Result.ProjectionKey = "baseline-old"
			if _, err := baseline.ApplyTranscript(ctx, old); err != nil {
				t.Fatal(err)
			}
			next := f.input
			next.ThreadID = sourceApplied.ThreadID
			next.ExpectedProjectionKey = "baseline-old"
			if _, err := baseline.ApplyTranscript(ctx, next); err != nil {
				t.Fatal(err)
			}
			onePass, err := baseline.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: sourceApplied.ThreadID})
			if err != nil {
				t.Fatal(err)
			}
			if provider == ProviderGrok {
				// Session display metadata lives in SQL; parsed provider evidence stays unchanged.
				var onePassModel, onePassEffort, chunkedModel, chunkedEffort string
				query := `SELECT usage_model, usage_effort FROM conversation_turns WHERE projection_id=(SELECT id FROM conversation_projections WHERE state='active') AND usage_contract=1`
				if err := baseline.db.QueryRowContext(ctx, query).Scan(&onePassModel, &onePassEffort); err != nil {
					t.Fatal(err)
				}
				if onePassModel != "grok-4-fast" || onePassEffort != "high" {
					t.Fatal("one-pass fixture did not exercise Grok session usage")
				}
				if err := f.store.db.QueryRowContext(ctx, query).Scan(&chunkedModel, &chunkedEffort); err != nil {
					t.Fatal("chunked rebuild lost Grok session usage", err)
				}
				if chunkedModel != onePassModel || chunkedEffort != onePassEffort {
					t.Fatal("chunked rebuild changed Grok session usage", chunkedModel, chunkedEffort)
				}
			}
			if !reflect.DeepEqual(onePass.Items, after.Items) || !reflect.DeepEqual(onePass.Turns, after.Turns) {
				t.Fatal("chunking changed fields relative to the one-pass writer")
			}
			resolved, err := f.store.ResolveTranscriptItemID(ctx, f.source.ThreadID, oldItemID)
			if err != nil || resolved != canonicalItemID {
				t.Fatal("switch lost the historical item reference", resolved, err)
			}
			// Local turn keys include the projection id in the existing writer. Report
			// that generation change explicitly; every other public field must survive.
			if len(before.Items) != len(after.Items) || len(before.Turns) != len(after.Turns) {
				t.Fatal("rebuild lost rows")
			}
			for i := range before.Items {
				previous, current := before.Items[i], after.Items[i]
				if previous.TurnKey == current.TurnKey {
					t.Fatal("fixture did not exercise the existing generation-scoped turn key")
				}
				t.Logf("existing one-pass generation change: item=%s turnKey=%s -> %s", previous.ID, previous.TurnKey, current.TurnKey)
				previous.TurnKey = current.TurnKey
				if !reflect.DeepEqual(previous, current) {
					t.Fatal("rebuild changed an item value or public item reference")
				}
			}
			for i := range before.Turns {
				previous, current := before.Turns[i], after.Turns[i]
				t.Logf("existing one-pass generation change: turnKey=%s -> %s", previous.Key, current.Key)
				previous.Key = current.Key
				if !reflect.DeepEqual(previous, current) {
					t.Fatal("rebuild changed turn values or the final-answer item reference")
				}
			}
			cp, ok, err := f.store.LoadTranscriptCheckpoint(ctx, f.source.ThreadID)
			var gotState, wantState any
			gotErr := json.Unmarshal(cp.ParserState, &gotState)
			wantErr := json.Unmarshal(f.parsed.Transcript.ParserState, &wantState)
			if err != nil || !ok || gotErr != nil || wantErr != nil || !reflect.DeepEqual(gotState, wantState) {
				t.Fatal("switch lost continuation", err)
			}
			if err := reconcileConversationSearch(ctx, f.store.db, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Every invalidating switch check must leave A active and allow B to be abandoned.
func TestTranscriptRebuildSwitchChecksFailClosed(t *testing.T) {
	cases := []struct{ name, statement string }{
		{"active-id", `UPDATE conversation_projections SET state='superseded',projection_key='replaced-active' WHERE state='active'`},
		{"active-key", `UPDATE conversation_projections SET projection_key='changed' WHERE state='active'`},
		{"active-offset", `UPDATE conversation_projections SET read_offset=read_offset+1 WHERE state='active'`},
		{"active-parser", `UPDATE conversation_projections SET parser_version='changed' WHERE state='active'`},
		{"active-source", `UPDATE conversation_projections SET source_generation=source_generation+1 WHERE state='active'`},
		{"shadow-thread", `UPDATE conversation_projections SET thread_id='other' WHERE state='building'`},
		{"shadow-state", `UPDATE conversation_projections SET state='abandoned' WHERE state='building'`},
		{"search-versions", `UPDATE conversation_search_state SET tokenizer_version='changed'`},
		{"search-start", `UPDATE conversation_search_state SET started_at='2026-01-02T00:00:00Z'`},
		{"search-modes", `UPDATE conversation_search_state SET state='failed'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRebuildFixture(t)
			ctx := context.Background()
			buildSearchModes(t, ctx, f.store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 10)
			build := f.complete(t)
			if c.name == "shadow-thread" {
				other := testSource(f.source.Source.Root, "other.jsonl", "other", ThreadMain, nil)
				if _, err := f.store.ApplySource(ctx, other); err != nil {
					t.Fatal(err)
				}
				var id string
				f.store.db.QueryRow(`SELECT id FROM threads WHERE native_thread_id='other'`).Scan(&id)
				c.statement = `UPDATE conversation_projections SET thread_id='` + id + `' WHERE state='building'`
			}
			if _, err := f.store.db.Exec(c.statement); err != nil {
				t.Fatal(err)
			}
			if c.name == "active-id" {
				_, err := f.store.db.Exec(`INSERT INTO conversation_projections(
projection_key,thread_id,source_file_id,source_generation,provider,parser_version,
completion_rule_version,schema_version,taxonomy_version,state,complete_offset,
complete_lines,parser_state_json,item_count,latest_seq,last_indexed_at,error_summary,read_offset)
SELECT ?,thread_id,source_file_id,source_generation,provider,parser_version,
completion_rule_version,schema_version,taxonomy_version,'active',complete_offset,
complete_lines,parser_state_json,item_count,latest_seq,last_indexed_at,error_summary,read_offset
FROM conversation_projections WHERE id=?`, f.base.ProjectionKey, build.activeID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.store.SwitchTranscriptRebuild(ctx, build); !errors.Is(err, ErrTranscriptChanged) {
				t.Fatalf("invalid switch accepted: %v", err)
			}
			if err := f.store.AbandonTranscriptRebuild(ctx, build); err != nil {
				t.Fatal(err)
			}
			assertCount(t, f.store.db, `SELECT COUNT(*) FROM conversation_projections WHERE state='building'`, 0)
			assertCount(t, f.store.db, `SELECT COUNT(*) FROM conversation_projections WHERE state='active'`, 1)
		})
	}
}

// Finishing a search build changes its checkpoint/state but not the shadow's documents (L2).
func TestTranscriptRebuildSurvivesSearchBuildCompletion(t *testing.T) {
	f := newRebuildFixture(t)
	ctx := context.Background()
	modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
	if _, err := f.store.StartConversationSearchBackfill(ctx, ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	build := f.complete(t)
	for {
		progress, err := f.store.RunConversationSearchBackfillBatch(ctx, ConversationSearchBackfillBatch{Modes: modes, BatchSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
	}
	if _, err := f.store.SwitchTranscriptRebuild(ctx, build); err != nil {
		t.Fatalf("search completion abandoned a valid shadow: %v", err)
	}
}

// Kind-changing ApplySource must not scan or remove A/B's documents at the switch (N2).
func TestTranscriptRebuildSourceSwitchSkipsThreadSearchMaintenance(t *testing.T) {
	f := newRebuildFixture(t)
	ctx := context.Background()
	buildSearchModes(t, ctx, f.store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 10)
	build := f.begin(t)
	if err := f.store.WriteTranscriptRebuild(ctx, build, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.IndexTranscriptRebuild(ctx, build, ThreadGuardian, nil); err != nil {
		t.Fatal(err)
	}
	f.parsed.Metadata.Projection.Kind = ThreadGuardian
	// A maintenance pass under the new kind would delete A's documents and hit this guard.
	if _, err := f.store.db.Exec(`CREATE TRIGGER no_search_delete BEFORE DELETE ON conversation_search_documents BEGIN SELECT RAISE(ABORT,'thread search maintenance ran'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApplySource(ctx, SourceProjection{Source: f.source.Source, Result: &f.parsed.Metadata, ParserVersion: f.parsed.ParserVersion, Status: SourcePresent}, ApplySourceOptions{SkipThreadSearch: f.source.ThreadID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SwitchTranscriptRebuild(ctx, build); err != nil {
		t.Fatal(err)
	}
	if err := reconcileConversationSearch(ctx, f.store.db, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}); err != nil {
		t.Fatal(err)
	}
}

// Crash/cancel at durable edges must retain A, roll back the current chunk, and collect only abandoned B.
func TestTranscriptRebuildCrashEdgesRecoverWithoutPartialVisibility(t *testing.T) {
	for _, point := range []string{"parse", "create", "items-rollback", "items-cancel", "between-items", "search-rollback", "search-cancel", "search", "after-source", "switch-rollback", "after-switch", "collection"} {
		t.Run(point, func(t *testing.T) {
			f := newRebuildFixture(t)
			ctx := context.Background()
			var build *TranscriptRebuildHandle
			buildSearchModes(t, ctx, f.store, []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}, true, 10)
			if point != "parse" {
				build = f.begin(t)
			}
			if point == "items-cancel" {
				canceled, cancel := context.WithCancel(ctx)
				defer cancel()
				visit := build.input.Result.parsedBatches
				build.input.Result.parsedBatches = func(yield func(model.Batch) error) error {
					return visit(func(batch model.Batch) error {
						err := yield(batch)
						cancel()
						return err
					})
				}
				if err := f.store.WriteTranscriptRebuild(canceled, build, nil); !errors.Is(err, context.Canceled) {
					t.Fatal("item cancellation did not stop the chunk", err)
				}
			} else if point == "items-rollback" {
				if _, err := f.store.db.Exec(`CREATE TRIGGER fail_items BEFORE INSERT ON conversation_items BEGIN SELECT RAISE(ABORT,'item crash'); END`); err != nil {
					t.Fatal(err)
				}
				if err := f.store.WriteTranscriptRebuild(ctx, build, nil); err == nil {
					t.Fatal("item crash did not fail")
				}
				assertCount(t, f.store.db, fmt.Sprintf(`SELECT COUNT(*) FROM conversation_items WHERE projection_id=%d`, build.id), 0)
			} else if build != nil && point != "create" {
				if err := f.store.WriteTranscriptRebuild(ctx, build, nil); err != nil {
					t.Fatal(err)
				}
				if point == "search-cancel" {
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					if err := f.store.IndexTranscriptRebuild(canceled, build, ThreadMain, nil); !errors.Is(err, context.Canceled) {
						t.Fatal("search cancellation did not stop the pass", err)
					}
				} else if point == "search-rollback" {
					if _, err := f.store.db.Exec(`CREATE TRIGGER fail_search BEFORE INSERT ON conversation_search_documents WHEN NEW.projection_id=` + fmt.Sprint(build.id) + ` BEGIN SELECT RAISE(ABORT,'search crash'); END`); err != nil {
						t.Fatal(err)
					}
					if err := f.store.IndexTranscriptRebuild(ctx, build, ThreadMain, nil); err == nil {
						t.Fatal("search crash did not fail")
					}
				} else if point != "between-items" {
					if err := f.store.IndexTranscriptRebuild(ctx, build, ThreadMain, nil); err != nil {
						t.Fatal(err)
					}
				}
			}
			if point == "after-source" || point == "switch-rollback" || point == "after-switch" {
				if _, err := f.store.ApplySource(ctx, SourceProjection{Source: f.source.Source, Result: &f.parsed.Metadata, ParserVersion: f.parsed.ParserVersion, Status: SourcePresent}, ApplySourceOptions{SkipThreadSearch: f.source.ThreadID}); err != nil {
					t.Fatal(err)
				}
			}
			if point == "switch-rollback" {
				if _, err := f.store.db.Exec(`CREATE TRIGGER fail_switch BEFORE UPDATE OF state ON conversation_projections WHEN NEW.state='active' BEGIN SELECT RAISE(ABORT,'switch crash'); END`); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.SwitchTranscriptRebuild(ctx, build); err == nil {
					t.Fatal("switch crash did not fail")
				}
			}
			if point == "after-switch" {
				if _, err := f.store.SwitchTranscriptRebuild(ctx, build); err != nil {
					t.Fatal(err)
				}
			}
			if point == "collection" {
				if err := f.store.AbandonTranscriptRebuild(ctx, build); err != nil {
					t.Fatal(err)
				}
				candidate := supersededProjectionCandidate{id: build.id, thread: f.source.ThreadID}
				if _, _, err := f.store.collectSupersededProjectionSearchBatch(ctx, candidate); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			f.store, err = Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.AbandonBuildingTranscripts(ctx); err != nil {
				t.Fatal(err)
			}
			expected := f.base.ProjectionKey
			if point == "after-switch" {
				expected = f.input.Result.ProjectionKey
			}
			if got := f.active(t); got != expected {
				t.Fatalf("crash exposed partial state: %q", got)
			}
			if _, err := f.store.CollectSupersededProjectionsBatch(ctx, 64); err != nil {
				t.Fatal(err)
			}
			assertCount(t, f.store.db, `SELECT COUNT(*) FROM conversation_projections WHERE state IN ('building','abandoned','superseded')`, 0)
			page, err := f.store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: f.source.ThreadID})
			if err != nil || len(page.Items) != 2 {
				t.Fatal("crash lost A's rows", err)
			}
		})
	}
}

// Bounded collection cannot delete a building/active projection or more than one statement's limit.
func TestTranscriptRebuildCollectorBatchLimitAndEligibility(t *testing.T) {
	f := newRebuildFixture(t)
	ctx := context.Background()
	build := f.begin(t)
	if err := f.store.WriteTranscriptRebuild(ctx, build, nil); err != nil {
		t.Fatal(err)
	}
	candidate := supersededProjectionCandidate{id: build.id, thread: f.source.ThreadID}
	if removed, eligible, err := f.store.collectSupersededProjectionTableBatch(ctx, candidate, "conversation_items"); err != nil || eligible || removed != 0 {
		t.Fatal("collector touched building B", removed, eligible, err)
	}
	if err := f.store.AbandonTranscriptRebuild(ctx, build); err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1003; i++ {
		if _, err := tx.Exec(`INSERT INTO conversation_parse_diagnostics(projection_id,diagnostic_key,diagnostic_json) VALUES (?,?,?)`, build.id, fmt.Sprint(i), `{}`); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	removed, eligible, err := f.store.collectSupersededProjectionTableBatch(ctx, candidate, "conversation_parse_diagnostics")
	if err != nil || !eligible || removed != 1000 {
		t.Fatal("collector exceeded statement bound", removed, eligible, err)
	}
	assertCount(t, f.store.db, fmt.Sprintf(`SELECT COUNT(*) FROM conversation_parse_diagnostics WHERE projection_id=%d`, build.id), 3)
}

// A backlog build yields at a committed chunk edge; cancellation prevents a new build.
func TestTranscriptRebuildChunkEdgePreemptionAndCancel(t *testing.T) {
	f := newRebuildFixture(t)
	ctx := context.Background()
	visit := f.input.Result.parsedBatches
	callbacks := 0
	f.input.Result.parsedBatches = func(yield func(model.Batch) error) error {
		return visit(func(batch model.Batch) error {
			err := yield(batch)
			callbacks++
			if callbacks == 1 {
				time.Sleep(backgroundWriteTarget + time.Millisecond)
			}
			return err
		})
	}
	build := f.begin(t)
	checks := 0
	err := f.store.WriteTranscriptRebuild(ctx, build, func() bool {
		checks++
		if checks == 2 {
			if _, err := f.store.EnsureRoot(ctx, f.source.Source.Root); err != nil {
				t.Fatal(err)
			}
			return true
		}
		return false
	})
	if !errors.Is(err, ErrTranscriptRebuildPreempted) || callbacks < 2 {
		t.Fatalf("backlog did not yield at its chunk edge: callbacks=%d checks=%d err=%v", callbacks, checks, err)
	}
	if f.active(t) != f.base.ProjectionKey {
		t.Fatal("preemption changed A")
	}
	if err := f.store.AbandonTranscriptRebuild(ctx, build); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.store.BeginTranscriptRebuild(canceled, f.input, f.base); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled create was accepted", err)
	}
}

// A failed startup UPDATE cannot advertise success or silently retain building rows.
func TestTranscriptRebuildStartupAbandonFailsClosed(t *testing.T) {
	f := newRebuildFixture(t)
	ctx := context.Background()
	f.begin(t)
	if _, err := f.store.db.Exec(`CREATE TRIGGER fail_abandon BEFORE UPDATE OF state ON conversation_projections WHEN OLD.state='building' BEGIN SELECT RAISE(ABORT,'startup crash'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbandonBuildingTranscripts(ctx); err == nil {
		t.Fatal("startup abandon hid its failure")
	}
	assertCount(t, f.store.db, `SELECT COUNT(*) FROM conversation_projections WHERE state='building'`, 1)
	if _, err := f.store.db.Exec(`DROP TRIGGER fail_abandon`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AbandonBuildingTranscripts(ctx); err != nil {
		t.Fatal(err)
	}
	assertCount(t, f.store.db, `SELECT COUNT(*) FROM conversation_projections WHERE state='building'`, 0)
}
