package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeUsageKeepsCacheReadAndWriteSeparate(t *testing.T) {
	result := projectClaude(t, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:01Z","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"cache_read_input_tokens":10,"cache_creation_input_tokens":4,"output_tokens":8,"output_tokens_details":{"thinking_tokens":3}}}}`,
	}, "\n")+"\n")
	usage := result.Turns[0].Usage
	assertInt64Pointer(t, "input", usage.Input, 2)
	assertInt64Pointer(t, "cacheRead", usage.CacheRead, 10)
	assertInt64Pointer(t, "cacheWrite", usage.CacheWrite, 4)
	assertInt64Pointer(t, "output", usage.Output, 8)
	assertInt64Pointer(t, "reasoning", usage.Reasoning, 3)
	if usage.Total != nil {
		t.Fatalf("claude fabricated total: %#v", usage.Total)
	}
}

func TestClaudeDuplicateMessageIDDoesNotDoubleCount(t *testing.T) {
	result := projectClaude(t, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","uuid":"a","message":{"id":"msg_1","content":[{"type":"thinking","thinking":"..."}],"usage":{"input_tokens":2,"output_tokens":10}}}`,
		`{"type":"assistant","uuid":"b","message":{"id":"msg_1","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":10}}}`,
	}, "\n")+"\n")
	assertInt64Pointer(t, "output", result.Turns[0].Usage.Output, 10)
}

func TestClaudeDistinctMessageIDsSumInOneTurn(t *testing.T) {
	result := projectClaude(t, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","name":"Bash","id":"c1"}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":4}}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"id":"msg_2","content":[{"type":"text","text":"Done"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}}`,
	}, "\n")+"\n")
	assertInt64Pointer(t, "input", result.Turns[0].Usage.Input, 4)
	assertInt64Pointer(t, "output", result.Turns[0].Usage.Output, 9)
}

func TestClaudeParentSidechainUsageIsNotMixedIntoEnvelope(t *testing.T) {
	result := projectClaude(t, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","message":{"id":"msg_main","content":[{"type":"text","text":"Main"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}`,
		`{"type":"assistant","isSidechain":true,"message":{"id":"msg_side","content":[{"type":"text","text":"Worker"}],"usage":{"input_tokens":90,"output_tokens":80}}}`,
	}, "\n")+"\n")
	assertInt64Pointer(t, "input", result.Turns[0].Usage.Input, 2)
	assertInt64Pointer(t, "output", result.Turns[0].Usage.Output, 3)
}

func TestCodexLastTokenUsageSumsInATurn(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Prompt"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":110},"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":110}}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":50,"cached_input_tokens":30,"output_tokens":7,"reasoning_output_tokens":1,"total_tokens":57},"total_token_usage":{"input_tokens":150,"cached_input_tokens":70,"output_tokens":17,"reasoning_output_tokens":3,"total_tokens":167}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n")+"\n")
	usage := result.Turns[0].Usage
	assertInt64Pointer(t, "input", usage.Input, 150)
	assertInt64Pointer(t, "cacheRead", usage.CacheRead, 70)
	assertInt64Pointer(t, "output", usage.Output, 17)
	if usage.CacheWrite != nil {
		t.Fatalf("codex invented cache write: %#v", usage.CacheWrite)
	}
}

func TestCodexUsageAppendDoesNotDoubleCount(t *testing.T) {
	first := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"append-tier","message":"Prompt"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12},"total_token_usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}}`,
	}, "\n") + "\n"
	second := strings.Join([]string{
		`{"type":"turn_context","payload":{"turn_id":"append-tier","service_tier":"standard"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8},"total_token_usage":{"input_tokens":15,"output_tokens":5,"total_tokens":20}}}}`,
	}, "\n") + "\n"
	adapter := newTranscriptFixture(t, ProviderCodex)
	firstResult, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(first),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(second),
		ProjectionKey: firstResult.ProjectionKey, StartOffset: firstResult.CompleteOffset,
		PriorCompleteLines: firstResult.CompleteLines, PriorProjectedSeq: int64(len(firstResult.Items)),
		PreviousState: firstResult.ParserState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondResult.Turns) != 1 {
		t.Fatalf("append turns = %#v", secondResult.Turns)
	}
	assertInt64Pointer(t, "appended input", secondResult.Turns[0].Usage.Input, 15)
	assertInt64Pointer(t, "appended output", secondResult.Turns[0].Usage.Output, 5)
	if secondResult.Turns[0].ServiceTier != "priority" {
		t.Fatalf("appended service tier = %q, want priority", secondResult.Turns[0].ServiceTier)
	}
}

func TestGrokUsageCapturesCacheWrite(t *testing.T) {
	updates := `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}
{"timestamp":1893456001,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":80,"outputTokens":9,"totalTokens":89,"cachedReadTokens":20,"cacheCreationTokens":6,"reasoningTokens":4}}}}
`
	source := grokSessionFixture(t, "-tmp", "s1", grokCurrentSummary, &updates)
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: strings.NewReader(updates),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].Usage == nil {
		t.Fatalf("turns = %#v", result.Turns)
	}
	usage := result.Turns[0].Usage
	assertInt64Pointer(t, "input", usage.Input, 80)
	assertInt64Pointer(t, "cacheRead", usage.CacheRead, 20)
	assertInt64Pointer(t, "cacheWrite", usage.CacheWrite, 6)
	assertInt64Pointer(t, "reasoning", usage.Reasoning, 4)
}

func TestThreadTurnUsageViewHidesOldGrain(t *testing.T) {
	store := turnTotalsFixture(t)
	if _, err := store.db.Exec(`UPDATE conversation_turns SET usage_input = 999, usage_cache_read = 1 WHERE projection_id = 1`); err != nil {
		t.Fatal(err)
	}
	var input sql.NullInt64
	err := store.db.QueryRow(`SELECT input_tokens FROM thread_turn_usage WHERE thread_id = 'nth_1' AND turn_ordinal = 1`).Scan(&input)
	if err != nil {
		t.Fatal(err)
	}
	if input.Valid {
		t.Fatalf("old-grain input leaked through view: %d", input.Int64)
	}
}

func TestTranscriptParserBackfillSkipsMissingSources(t *testing.T) {
	store := turnTotalsFixture(t)
	if _, err := store.db.Exec(`UPDATE conversation_projections SET parser_version = 'codex-transcript-v6' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE source_files SET status = 'source_missing' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("missing-source candidates = %#v", candidates)
	}
}

func TestTranscriptParserBackfillSelectsPresentParserBehind(t *testing.T) {
	store := turnTotalsFixture(t)
	if _, err := store.db.Exec(`UPDATE conversation_projections SET parser_version = 'codex-transcript-v6' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ThreadID != "nth_1" {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestTranscriptParserUnaffectedCheckIsExactToSourceSnapshot(t *testing.T) {
	store := turnTotalsFixture(t)
	current := CurrentTranscriptParserVersion(ProviderCodex)
	currentCompletion := CurrentTranscriptCompletionRuleVersion(ProviderCodex)
	old := strings.Replace(current, "codex-reader-v37", "codex-reader-v36", 1)
	if _, err := store.db.Exec(`UPDATE conversation_projections SET parser_version = ? WHERE id = 1`, old); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(context.Background(), 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("initial candidates=%#v err=%v", candidates, err)
	}
	originalCompletion := candidates[0].CompletionRuleVersion
	// Existing inspection receipts still suppress an unchanged source.
	if _, err := store.db.Exec(`
INSERT INTO conversation_parser_checks(
  source_file_id,source_generation,source_size,source_mtime,
  from_parser_version,to_parser_version,
  from_completion_rule_version,to_completion_rule_version,outcome,checked_at
)
SELECT id,generation,size,COALESCE(mtime,''),?,?,?,?, 'unaffected',?
FROM source_files WHERE id=?`, old, current, originalCompletion, currentCompletion,
		formatTime(time.Now().UTC()), candidates[0].Source.ID); err != nil {
		t.Fatal(err)
	}
	if candidates, err = store.ListTranscriptParserBackfillCandidates(context.Background(), 10); err != nil || len(candidates) != 0 {
		t.Fatalf("checked source remained queued=%#v err=%v", candidates, err)
	}
	if _, err := store.db.Exec(`UPDATE conversation_projections SET completion_rule_version = 'different-completion' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if candidates, err = store.ListTranscriptParserBackfillCandidates(context.Background(), 10); err != nil || len(candidates) != 1 {
		t.Fatalf("completion change did not invalidate check=%#v err=%v", candidates, err)
	}
	if _, err := store.db.Exec(`UPDATE conversation_projections SET completion_rule_version = ? WHERE id = 1`, originalCompletion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE source_files SET size=size+1, mtime='2026-09-16T04:00:00.000000000Z' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if candidates, err = store.ListTranscriptParserBackfillCandidates(context.Background(), 10); err != nil || len(candidates) != 1 {
		t.Fatalf("changed source did not invalidate check=%#v err=%v", candidates, err)
	}
}

func TestTranscriptParserBackfillSkipsErroredProjections(t *testing.T) {
	store := turnTotalsFixture(t)
	if _, err := store.db.Exec(`UPDATE conversation_projections SET parser_version = 'codex-transcript-v6', error_summary = 'usage backfill: unique content_ref' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("errored projection still a candidate: %#v", candidates)
	}
}

func TestUsageRebuildIsIdempotentByOrdinal(t *testing.T) {
	ctx := context.Background()
	input := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":21,"cached_input_tokens":4,"output_tokens":6,"total_tokens":27}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n") + "\n"
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "root")}
	metadata := testSource(rootSpec, "thread.jsonl", "native-idem", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatal(err)
	}
	projectAndApply := func() {
		t.Helper()
		projected, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: metadata.Source, Reader: strings.NewReader(input), SpoolDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		defer projected.Close()
		if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
			Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
			SourceGeneration: stored.Generation, Provider: ProviderCodex,
			ParserVersion: projected.ParserVersion, CompletionRuleVersion: projected.CompletionRuleVersion, Result: projected.Transcript,
		}); err != nil {
			t.Fatal(err)
		}
	}
	projectAndApply()
	first, err := store.ThreadTurnUsage(ctx, applied.ThreadID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	projectAndApply()
	second, err := store.ThreadTurnUsage(ctx, applied.ThreadID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Turns) != len(second.Turns) {
		t.Fatalf("turn count %d vs %d", len(first.Turns), len(second.Turns))
	}
	for i := range first.Turns {
		if first.Turns[i].Ordinal != second.Turns[i].Ordinal {
			t.Fatalf("ordinal drift at %d", i)
		}
		a, b := first.Turns[i].Usage, second.Turns[i].Usage
		if (a == nil) != (b == nil) {
			t.Fatalf("usage presence drift at ordinal %d", first.Turns[i].Ordinal)
		}
		if a != nil && (*a.Input != *b.Input || *a.Output != *b.Output || *a.CacheRead != *b.CacheRead) {
			t.Fatalf("envelope drift at ordinal %d: %#v vs %#v", first.Turns[i].Ordinal, a, b)
		}
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("current-parser candidates after rebuild = %#v", candidates)
	}
}

func TestThreadTurnUsageAPIJoinsMessages(t *testing.T) {
	ctx := context.Background()
	codexInput := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"usage-tier","message":"Prompt"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"turn_context","payload":{"turn_id":"usage-tier","service_tier":"standard"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":11,"cached_input_tokens":2,"output_tokens":4,"total_tokens":15}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n") + "\n"
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "root")}
	metadata := testSource(rootSpec, "thread.jsonl", "native-usage", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	projected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: strings.NewReader(codexInput)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: projected,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ThreadTurnUsage(ctx, applied.ThreadID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 {
		t.Fatalf("usage turns = %#v", page.Turns)
	}
	got := page.Turns[0]
	if got.ServiceTier != "priority" {
		t.Fatalf("api service tier = %q, want priority", got.ServiceTier)
	}
	var usageTier, messageTier sql.NullString
	if err := store.db.QueryRow(`SELECT service_tier FROM thread_turn_usage WHERE thread_id = ?`, applied.ThreadID).Scan(&usageTier); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT service_tier FROM thread_turn_usage_messages WHERE thread_id = ? LIMIT 1`, applied.ThreadID).Scan(&messageTier); err != nil {
		t.Fatal(err)
	}
	if !usageTier.Valid || usageTier.String != "priority" || !messageTier.Valid || messageTier.String != "priority" {
		t.Fatalf("view service tiers = %q / %q", usageTier.String, messageTier.String)
	}
	assertInt64Pointer(t, "api input", got.Usage.Input, 11)
	assertInt64Pointer(t, "api cacheRead", got.Usage.CacheRead, 2)
	if len(got.Messages) < 2 {
		t.Fatalf("messages = %#v", got.Messages)
	}
	totals, err := store.ListThreadUsageTotals(ctx, ThreadUsageListQuery{Provider: ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals.Items) != 1 || totals.Items[0].TurnsWithUsage != 1 {
		t.Fatalf("fleet totals = %#v", totals.Items)
	}
	assertInt64Pointer(t, "fleet input", totals.Items[0].InputTokens, 11)

	if _, err := store.db.Exec(`UPDATE conversation_turns SET usage_service_tier = NULL WHERE projection_id = (SELECT id FROM conversation_projections WHERE thread_id = ? AND state = 'active')`, applied.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT service_tier FROM thread_turn_usage WHERE thread_id = ?`, applied.ThreadID).Scan(&usageTier); err != nil {
		t.Fatal(err)
	}
	if usageTier.Valid {
		t.Fatalf("unknown service tier leaked through view: %q", usageTier.String)
	}
	page, err = store.ThreadTurnUsage(ctx, applied.ThreadID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page.Turns[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"serviceTier"`)) {
		t.Fatalf("unknown service tier was not omitted from JSON: %s", encoded)
	}
}

func TestClaudeTurnCapturesModelAndEffort(t *testing.T) {
	result := projectClaude(t, strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","effort":"high","message":{"id":"msg_1","model":"claude-fable-5","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}`,
	}, "\n")+"\n")
	turn := result.Turns[0]
	if turn.Model != "claude-fable-5" || turn.Effort != "high" {
		t.Fatalf("claude model/effort = %q / %q", turn.Model, turn.Effort)
	}
}

func TestCodexTurnContextCapturesModelAndEffort(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"t1","message":"Prompt"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t1","model":"gpt-5.6-terra","effort":"high"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n")+"\n")
	turn := result.Turns[0]
	if turn.Model != "gpt-5.6-terra" || turn.Effort != "high" || turn.NativeTurnID != "t1" {
		t.Fatalf("codex turn = model %q effort %q native %q", turn.Model, turn.Effort, turn.NativeTurnID)
	}
	assertInt64Pointer(t, "input", turn.Usage.Input, 11)
}

func TestCodexThreadSettingsServiceTierUsesExplicitPerTurnPrecedence(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"t1","message":"First prompt"}}`,
		// A turn_context fallback may be overwritten by an explicit setting later
		// in that same still-open turn.
		`{"type":"turn_context","payload":{"turn_id":"t1","service_tier":"default"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		// Observed rollout order: the nested settings notice immediately precedes
		// the matching turn_context. The explicit tier must win over the context.
		`{"type":"event_msg","timestamp":"2026-08-25T22:29:54.540Z","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"turn_context","timestamp":"2026-08-25T22:29:54.568Z","payload":{"turn_id":"t2","service_tier":"default"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t2","service_tier":"default"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		// Legacy direct fields still work, and a later explicit setting wins.
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","service_tier":"default"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"turn_context","payload":{"turn_id":"t3","service_tier":"default"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		// Nested default is Standard's real Codex wire value.
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"default"}}}`,
		`{"type":"turn_context","payload":{"turn_id":"t4"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		// A direct legacy setting remains a supported explicit source.
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","service_tier":"default"}}`,
		`{"type":"turn_context","payload":{"turn_id":"t5"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		// Missing settings/context remain unknown rather than inherit t5.
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{}}}`,
		`{"type":"turn_context","payload":{"turn_id":"t6"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
		// Explicit null is unknown too, not an inferred prior tier.
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":null}}}`,
		`{"type":"turn_context","payload":{"turn_id":"t7"}}`,
	}, "\n")+"\n")
	if len(result.Turns) != 7 {
		t.Fatalf("turns = %#v", result.Turns)
	}
	got := []string{result.Turns[0].ServiceTier, result.Turns[1].ServiceTier, result.Turns[2].ServiceTier, result.Turns[3].ServiceTier, result.Turns[4].ServiceTier, result.Turns[5].ServiceTier, result.Turns[6].ServiceTier}
	if got[0] != "priority" || got[1] != "priority" || got[2] != "priority" || got[3] != "default" || got[4] != "default" || got[5] != "" || got[6] != "" {
		t.Fatalf("service tiers = %#v, want [priority priority priority default default empty empty]", got)
	}
	if result.Turns[1].NativeTurnID != "t2" || result.Turns[2].NativeTurnID != "t3" || result.Turns[3].NativeTurnID != "t4" || result.Turns[4].NativeTurnID != "t5" || result.Turns[5].NativeTurnID != "t6" || result.Turns[6].NativeTurnID != "t7" {
		t.Fatalf("native turn ids = %#v", result.Turns)
	}
	var settingsNotice *TranscriptItem
	for index := range result.Items {
		item := &result.Items[index]
		if item.SourceLine == 6 && item.TurnOrdinal == result.Turns[1].Ordinal {
			settingsNotice = item
			break
		}
	}
	if settingsNotice == nil || settingsNotice.TurnKey != result.Turns[1].Key {
		t.Fatalf("settings notice did not share the following turn_context key/ordinal: notice=%#v turn=%#v", settingsNotice, result.Turns[1])
	}
}

func TestCodexTurnContextCollaborationServiceTierFallback(t *testing.T) {
	result := projectCodex(t, `{"type":"turn_context","payload":{"turn_id":"collab-tier","collaboration_mode":{"settings":{"service_tier":"priority"}}}}`+"\n")
	if len(result.Turns) != 1 || result.Turns[0].ServiceTier != "priority" {
		t.Fatalf("collaboration service tier = %#v", result.Turns)
	}
}

func TestGrokTurnCopiesSessionModelAndEffort(t *testing.T) {
	result := projectGrokUpdates(t, grokCurrentSummary, grokOneTurnUpdates)
	if result.Turns[0].Model != "grok-4-fast" || result.Turns[0].Effort != "high" {
		t.Fatalf("grok session snapshot = %q / %q", result.Turns[0].Model, result.Turns[0].Effort)
	}
}

func TestGrokTurnLeavesModelAndEffortEmptyWhenSummaryOmitsThem(t *testing.T) {
	result := projectGrokUpdates(t, `{"info":{"id":"s1","cwd":"/tmp"}}`, grokOneTurnUpdates)
	if result.Turns[0].Model != "" || result.Turns[0].Effort != "" {
		t.Fatalf("grok invented model/effort: %#v", result.Turns[0])
	}
}

func TestGrokTurnLeavesModelAndEffortEmptyWhenSummaryIsMissing(t *testing.T) {
	dir := t.TempDir()
	updatesPath := filepath.Join(dir, "updates.jsonl")
	if err := os.WriteFile(updatesPath, []byte(grokOneTurnUpdates), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: SourceCandidate{
			Root: SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: dir},
			Path: updatesPath, Kind: ThreadMain,
		},
		Reader: strings.NewReader(grokOneTurnUpdates),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].Model != "" || result.Turns[0].Effort != "" {
		t.Fatalf("missing summary invented model/effort: %#v", result.Turns)
	}
}

func TestGrokRebuildRestampsLatestSessionSnapshot(t *testing.T) {
	updates := grokTwoTurnUpdates
	source := grokSessionFixture(t, "-tmp", "s1", grokCurrentSummary, &updates)
	first, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: strings.NewReader(updates),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Turns) != 2 || first.Turns[0].Effort != "high" || first.Turns[1].Effort != "high" {
		t.Fatalf("initial snapshot = %#v", first.Turns)
	}
	later := `{
  "info": {"id": "s1", "cwd": "/tmp"},
  "current_model_id": "grok-4.6",
  "reasoning_effort": "xhigh"
}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(source.Path), "summary.json"), []byte(later), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: strings.NewReader(updates),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Turns) != 2 {
		t.Fatalf("rebuild turns = %#v", second.Turns)
	}
	for i, turn := range second.Turns {
		if turn.Model != "grok-4.6" || turn.Effort != "xhigh" {
			t.Fatalf("turn %d restamp = %q / %q", i, turn.Model, turn.Effort)
		}
	}
}

func TestUsageModelEffortRoundTripsThroughView(t *testing.T) {
	ctx := context.Background()
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","effort":"xhigh","message":{"id":"msg_1","model":"claude-opus-4-6","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}}`,
	}, "\n") + "\n"
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "root")}
	metadata := testSource(rootSpec, "thread.jsonl", "native-model", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatal(err)
	}
	adapter := newTranscriptFixture(t, ProviderClaude)
	projected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderClaude,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: projected,
	}); err != nil {
		t.Fatal(err)
	}
	var model, effort sql.NullString
	if err := store.db.QueryRow(`SELECT model, effort FROM thread_turn_usage WHERE thread_id = ?`, applied.ThreadID).Scan(&model, &effort); err != nil {
		t.Fatal(err)
	}
	if model.String != "claude-opus-4-6" || effort.String != "xhigh" {
		t.Fatalf("view model/effort = %q / %q", model.String, effort.String)
	}
	page, err := store.ThreadTurnUsage(ctx, applied.ThreadID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].Model != "claude-opus-4-6" || page.Turns[0].Effort != "xhigh" {
		t.Fatalf("api turns = %#v", page.Turns)
	}
}

func TestGrokSessionModelEffortRoundTripsThroughView(t *testing.T) {
	ctx := context.Background()
	updates := grokOneTurnUpdates
	source := grokSessionFixture(t, "-tmp", "s1", grokCurrentSummary, &updates)
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	catalogSource := source
	catalogSource.Path = filepath.Join(filepath.Dir(source.Path), "summary.json")
	parsed, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: strings.NewReader(updates), SpoolDir: t.TempDir(), Catalog: &ParsedCatalogInput{Source: catalogSource, Reader: strings.NewReader(grokCurrentSummary), Format: "json"}})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	applied, err := store.ApplySource(ctx, SourceProjection{Source: source, Result: &parsed.Metadata, ParserVersion: parsed.ParserVersion, Status: SourcePresent})
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, source.Root), source.Path)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: ProviderGrok, ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion, Result: parsed.Transcript}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ThreadTurnUsage(ctx, applied.ThreadID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 || page.Turns[0].Model != "grok-4-fast" || page.Turns[0].Effort != "high" {
		t.Fatalf("grok view model/effort = %#v", page.Turns)
	}
	var nativeModel, nativeEffort sql.NullString
	if err := store.db.QueryRow(`SELECT json_extract(parsed_json,'$.model'),json_extract(parsed_json,'$.effort') FROM conversation_turns WHERE turn_key=?`, page.Turns[0].TurnKey).Scan(&nativeModel, &nativeEffort); err != nil {
		t.Fatal(err)
	}
	if nativeModel.Valid || nativeEffort.Valid {
		t.Fatal("session snapshot became native per-turn evidence")
	}
}

const grokOneTurnUpdates = `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}
{"timestamp":1893456001,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":80,"outputTokens":9,"totalTokens":89}}}}
`

const grokTwoTurnUpdates = `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}
{"timestamp":1893456001,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":80,"outputTokens":9,"totalTokens":89}}}}
{"timestamp":1893456002,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Again"}}}}
{"timestamp":1893456003,"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p2","stop_reason":"end_turn","usage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}}}
`

func projectGrokUpdates(t *testing.T, summary, updates string) TranscriptProjectionResult {
	t.Helper()
	source := grokSessionFixture(t, "-tmp", "s1", summary, &updates)
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: strings.NewReader(updates),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) == 0 {
		t.Fatalf("expected grok turns, got %#v", result.Turns)
	}
	return result
}

func TestCodexDualUserSerializationKeepsOneNativeTurn(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-a","model":"codex-auto-review","effort":"low"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Assess this."},{"type":"input_text","text":"Second block."}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Assess this.Second block."}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-a"}}`,
	}, "\n")+"\n")
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v, want 1", result.Turns)
	}
	turn := result.Turns[0]
	if turn.NativeTurnID != "turn-a" || turn.Model != "codex-auto-review" || turn.Effort != "low" {
		t.Fatalf("identity = %#v", turn)
	}
	assertInt64Pointer(t, "input", turn.Usage.Input, 10)
	assertInt64Pointer(t, "output", turn.Usage.Output, 2)
}

func TestCodexAgentsMdThenPromptKeepsOneNativeTurn(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n\nDo the work"}]}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-a","model":"gpt-5.6-sol","effort":"high"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Real prompt"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Real prompt"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-a"}}`,
	}, "\n")+"\n")
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v, want 1", result.Turns)
	}
	turn := result.Turns[0]
	if turn.NativeTurnID != "turn-a" || turn.Model != "gpt-5.6-sol" || turn.Effort != "high" {
		t.Fatalf("identity = %#v", turn)
	}
	assertInt64Pointer(t, "input", turn.Usage.Input, 8)
}

func TestCodexSequentialNativeTurnsStayTwoRows(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-a","model":"gpt-5.6-terra","effort":"high"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"First"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-a"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-b"}}`,
		`{"type":"turn_context","payload":{"turn_id":"turn-b","model":"gpt-5.6-terra","effort":"high"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Second"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-b"}}`,
	}, "\n")+"\n")
	if len(result.Turns) != 2 {
		t.Fatalf("turns = %#v, want 2", result.Turns)
	}
	if result.Turns[0].NativeTurnID != "turn-a" || result.Turns[1].NativeTurnID != "turn-b" {
		t.Fatalf("native ids = %#v", result.Turns)
	}
	assertInt64Pointer(t, "first input", result.Turns[0].Usage.Input, 5)
	assertInt64Pointer(t, "second input", result.Turns[1].Usage.Input, 7)
}

func TestCodexEventOnlyUserMessageStillOpensTurn(t *testing.T) {
	result := projectCodex(t, strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Only event"}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n")+"\n")
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v, want 1", result.Turns)
	}
	if result.Turns[0].NativeTurnID != "" {
		t.Fatalf("invented native id: %#v", result.Turns[0])
	}
	assertInt64Pointer(t, "input", result.Turns[0].Usage.Input, 4)
}

// A marker split across two read chunks must still be found: missing it would
// mark an affected source current and leave its merged prompts unsettled.
func TestSourceContainsMarkerAcrossChunkBoundary(t *testing.T) {
	marker := []byte(`"mergedSendUuids"`)
	path := filepath.Join(t.TempDir(), "source.jsonl")
	for _, offset := range []int{0, 1<<20 - 1, 1<<20 - len(marker)/2, 1 << 20, 3<<20 + 17} {
		content := make([]byte, offset+len(marker)+5)
		copy(content[offset:], marker)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		found, err := SourceContainsMarker(path, marker)
		if err != nil || !found {
			t.Fatalf("marker at offset %d: found=%v err=%v", offset, found, err)
		}
	}
	if err := os.WriteFile(path, make([]byte, 2<<20+3), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := SourceContainsMarker(path, marker); err != nil || found {
		t.Fatalf("absent marker: found=%v err=%v", found, err)
	}
	v35 := strings.Replace(CurrentTranscriptParserVersion(ProviderClaude), "claude-reader-v36", "claude-reader-v35", 1)
	v34 := strings.Replace(v35, "claude-reader-v35", "claude-reader-v34", 1)
	if upgrade, ok := TranscriptUpgradeMarker(strings.Replace(v34, "claude-reader-v34", "claude-reader-v33", 1), v34); !ok || string(upgrade) != string(marker) {
		t.Fatalf("v33 to v34 upgrade marker = %q, %v", upgrade, ok)
	}
	// v35 groups saved turns by Claude's promptId: a file without one cannot change.
	if upgrade, ok := TranscriptUpgradeMarker(v34, v35); !ok || string(upgrade) != `"promptId"` {
		t.Fatalf("v34 to v35 upgrade marker = %q, %v", upgrade, ok)
	}
	// v36 reads a sent slash command from a JSON string that opens with a command tag.
	if upgrade, ok := TranscriptUpgradeMarker(v35, CurrentTranscriptParserVersion(ProviderClaude)); !ok || string(upgrade) != `"<command-` {
		t.Fatalf("v35 to v36 upgrade marker = %q, %v", upgrade, ok)
	}
	if _, ok := TranscriptUpgradeMarker("codex-reader-v36", "codex-reader-v37"); ok {
		t.Fatal("an unlisted upgrade must re-read every source")
	}
}
