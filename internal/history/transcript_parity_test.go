package history

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func projectClaude(t *testing.T, input string) TranscriptProjectionResult {
	t.Helper()
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func projectCodex(t *testing.T, input string) TranscriptProjectionResult {
	t.Helper()
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestClaudeCompactSummaryBecomesNoticeAndDoesNotOpenTurn(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","isCompactSummary":true,"message":{"role":"user","content":[{"type":"text","text":"COMPACT_BODY_SENTINEL machine authored recap"}]}}`,
		`{"type":"user","timestamp":"2026-07-22T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Real prompt"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn"}}`,
	}, "\n") + "\n"
	result := projectClaude(t, input)

	var userCount, compactionCount int
	for _, item := range result.Items {
		switch item.Type {
		case ItemUserMessage:
			userCount++
			if strings.Contains(item.Text, "COMPACT_BODY_SENTINEL") {
				t.Fatalf("compact summary rendered as a user message: %#v", item)
			}
		case ItemCompaction:
			compactionCount++
			if item.Role != RoleSystem {
				t.Fatalf("compaction notice role = %q", item.Role)
			}
		}
	}
	if userCount != 1 || compactionCount != 1 {
		t.Fatalf("user=%d compaction=%d; compact summary must not add an operator prompt", userCount, compactionCount)
	}
	if len(result.Turns) != 1 || result.Turns[0].Status != TurnCompleted {
		t.Fatalf("compact summary opened a spurious turn: %#v", result.Turns)
	}
	encoded, _ := json.Marshal(result.Items)
	if !strings.Contains(string(encoded), "COMPACT_BODY_SENTINEL") {
		t.Fatalf("CLAUDE-ORIGIN-1: compaction lost native evidence: %s", encoded)
	}
}

func TestCodexSubagentActivityReadsKind(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"sub_agent_activity","kind":"started","agent_path":"agents/reviewer","agent_thread_id":"th_42"}}`,
		`{"type":"event_msg","payload":{"type":"sub_agent_activity","kind":"interrupted"}}`,
	}, "\n") + "\n"
	result := projectCodex(t, input)

	var notices []TranscriptItem
	for _, item := range result.Items {
		if item.SourcePayloadType == "sub_agent_activity" {
			notices = append(notices, item)
		}
	}
	if len(notices) != 2 {
		t.Fatalf("sub_agent_activity notices = %#v", notices)
	}
	if notices[0].Type != ItemNotice || notices[1].Type != ItemNotice {
		t.Fatalf("kind labels = %q, %q", notices[0].Text, notices[1].Text)
	}
	var detail map[string]string
	if err := json.Unmarshal(notices[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["agent_path"] != "agents/reviewer" || detail["agent_thread_id"] != "th_42" || detail["kind"] != "started" {
		t.Fatalf("subagent detail = %#v", detail)
	}
}

func TestCodexExecCommandParsesArguments(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		wantText    string
		wantWorkdir string
	}{
		{"string arguments", `{"type":"function_call","name":"exec_command","call_id":"c1","arguments":"{\"cmd\":\"ls -la\",\"workdir\":\"/tmp\"}"}`, "ls -la", "/tmp"},
		{"object arguments", `{"type":"function_call","name":"exec_command","call_id":"c2","arguments":{"cmd":"pwd"}}`, "pwd", ""},
		{"shell wrapper", `{"type":"function_call","name":"shell","call_id":"c3","arguments":"{\"command\":\"bash -lc 'echo hi'\"}"}`, "echo hi", ""},
		{"argv array", `{"type":"local_shell_call","call_id":"c4","arguments":"{\"command\":[\"bash\",\"-lc\",\"echo hey\"]}"}`, "echo hey", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := projectCodex(t, `{"type":"response_item","payload":`+tc.payload+`}`+"\n")
			var command TranscriptItem
			for _, item := range result.Items {
				if item.Type == ItemCommand {
					command = item
				}
			}
			if command.Type != ItemCommand {
				t.Fatalf("no command item: %#v", result.Items)
			}
			if command.Text != tc.wantText {
				t.Fatalf("command text = %q, want %q", command.Text, tc.wantText)
			}
			if tc.wantWorkdir != "" {
				var detail map[string]string
				if err := json.Unmarshal(command.Detail, &detail); err != nil {
					t.Fatal(err)
				}
				var arguments map[string]string
				if err := json.Unmarshal([]byte(detail["arguments"]), &arguments); err != nil {
					t.Fatal(err)
				}
				if arguments["workdir"] != tc.wantWorkdir {
					t.Fatalf("workdir = %q, want %q", arguments["workdir"], tc.wantWorkdir)
				}
			}
		})
	}
}

func TestCodexObjectToolOutputSerializesToCompactJSON(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":{"stdout":"file.txt","exit_code":0}}}`,
		`{"type":"event_msg","payload":{"type":"mcp_tool_call_end","call_id":"m1","result":{"ok":true,"rows":[1,2]}}}`,
	}, "\n") + "\n"
	result := projectCodex(t, input)

	var outputs []string
	for _, item := range result.Items {
		if item.Type == ItemToolResult {
			outputs = append(outputs, item.Text)
		}
	}
	if len(outputs) != 2 {
		t.Fatalf("tool result outputs = %#v", outputs)
	}
	if outputs[0] != `{"stdout":"file.txt","exit_code":0}` {
		t.Fatalf("function_call_output re-serialization = %q", outputs[0])
	}
	if outputs[1] != `{"ok":true,"rows":[1,2]}` {
		t.Fatalf("mcp_tool_call_end re-serialization = %q", outputs[1])
	}
}

func TestCodexTurnUsageAndDurationCaptured(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"t1","message":"Prompt"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":14948,"cached_input_tokens":4992,"output_tokens":800,"reasoning_output_tokens":469,"total_tokens":15748},"model_context_window":258400}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","duration_ms":4200}}`,
	}, "\n") + "\n"
	result := projectCodex(t, input)
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v", result.Turns)
	}
	turn := result.Turns[0]
	if turn.DurationMS != 4200 {
		t.Fatalf("duration = %d, want 4200", turn.DurationMS)
	}
	assertTurnUsage(t, turn.Usage, 14948, 4992, 800, 469, 15748, 258400)
}

func TestClaudeTurnUsageCaptured(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:01Z","message":{"id":"usage-answer","role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"cache_read_input_tokens":19234,"cache_creation_input_tokens":11140,"output_tokens":371}}}`,
		`{"type":"system","timestamp":"2026-07-22T10:00:02Z","subtype":"turn_duration","durationMs":6000}`,
	}, "\n") + "\n"
	result := projectClaude(t, input)
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v", result.Turns)
	}
	usage := result.Turns[0].Usage
	if usage == nil {
		t.Fatal("claude turn usage not captured")
	}
	assertInt64Pointer(t, "input", usage.Input, 2)
	assertInt64Pointer(t, "cacheRead", usage.CacheRead, 19234)
	assertInt64Pointer(t, "cacheWrite", usage.CacheWrite, 11140)
	assertInt64Pointer(t, "output", usage.Output, 371)
	if usage.Total != nil || usage.ContextWindow != nil {
		t.Fatalf("claude usage fabricated total/context: %#v", usage)
	}
}

func TestTurnUsageRoundTripsThroughStore(t *testing.T) {
	codexInput := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Prompt"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":10,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":135},"model_context_window":200000}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","duration_ms":1500}}`,
	}, "\n") + "\n"
	turn := storeRoundTripTurn(t, ProviderCodex, RootCodexSessions, codexInput)
	if turn.DurationMS != 1500 {
		t.Fatalf("round-tripped duration = %d", turn.DurationMS)
	}
	assertTurnUsage(t, turn.Usage, 100, 10, 20, 5, 135, 200000)

	claudeInput := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:01Z","message":{"id":"usage-answer","role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":1,"output_tokens":9}}}`,
		`{"type":"system","timestamp":"2026-07-22T10:00:02Z","subtype":"turn_duration","durationMs":900}`,
	}, "\n") + "\n"
	claudeTurn := storeRoundTripTurn(t, ProviderClaude, RootClaudeProjects, claudeInput)
	assertInt64Pointer(t, "round-trip claude input", claudeTurn.Usage.Input, 7)
	assertInt64Pointer(t, "round-trip claude cacheRead", claudeTurn.Usage.CacheRead, 3)
	assertInt64Pointer(t, "round-trip claude cacheWrite", claudeTurn.Usage.CacheWrite, 1)
	if claudeTurn.Usage.Total != nil || claudeTurn.Usage.ContextWindow != nil {
		t.Fatalf("round-trip fabricated null usage: %#v", claudeTurn.Usage)
	}
}

func TestCodexCollaborationSettingsModelEffortFallback(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"11111111-1111-4111-8111-111111111111","cwd":"/w"}}`,
		`{"type":"turn_context","payload":{"collaboration_mode":{"settings":{"model":"gpt-collab","effort":"high","service_tier":"priority"}}}}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions}, Path: "x/rollout.jsonl", Kind: ThreadMain},
		Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertKnownField(t, "collab model", result.Projection.Metadata.Model, "gpt-collab")
	assertKnownField(t, "collab effort", result.Projection.Metadata.Effort, "high")
	assertKnownField(t, "collab priority", result.Projection.Metadata.Priority, "priority")
}

func TestClaudeAskUserQuestionReadsToolUseResultAnswers(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Route it"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_q","name":"AskUserQuestion","input":{"questions":[{"header":"Routing","question":"Home repo?","options":[{"label":"web-content"}]}]}}],"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2026-07-22T10:00:02Z","toolUseResult":{"questions":[{"question":"Home repo?"}],"answers":{"Home repo?":"web-content (Recommended)"}},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_q","content":"Your questions have been answered: \"Home repo?\"=\"web-content (Recommended)\"."}]}}`,
	}, "\n") + "\n"
	result := projectClaude(t, input)

	var answer TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemToolResult && item.Role == RoleTool {
			answer = item
		}
	}
	if answer.Type != ItemToolResult {
		t.Fatalf("no native answer result: %#v", result.Items)
	}
	if !strings.Contains(answer.Text, "web-content (Recommended)") || !strings.Contains(answer.Text, "Home repo?") {
		t.Fatalf("answer text = %q", answer.Text)
	}
}

func TestClaudeAskUserQuestionFallsBackToContentText(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Route it"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_q","name":"AskUserQuestion","input":{"questions":[{"question":"Home repo?","options":[{"label":"a"}]}]}}],"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2026-07-22T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_q","content":"FALLBACK_CONTENT_TEXT"}]}}`,
	}, "\n") + "\n"
	result := projectClaude(t, input)
	var answer TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemToolResult && item.Role == RoleTool {
			answer = item
		}
	}
	if answer.Text != "FALLBACK_CONTENT_TEXT" {
		t.Fatalf("fallback answer text = %q", answer.Text)
	}
}

func TestClaudeContentToleratesStringAndMixedArrays(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":"Bare string prompt"}}`,
		`{"type":"assistant","timestamp":"2026-07-22T10:00:01Z","message":{"role":"assistant","content":["Loose text",{"type":"text","text":"Block text"}],"stop_reason":"end_turn"}}`,
	}, "\n") + "\n"
	result := projectClaude(t, input)
	var userText, agentText []string
	for _, item := range result.Items {
		switch item.Type {
		case ItemUserMessage:
			userText = append(userText, item.Text)
		case ItemAgentMessage:
			agentText = append(agentText, item.Text)
		}
	}
	if len(userText) != 1 || userText[0] != "Bare string prompt" {
		t.Fatalf("bare string user content = %#v", userText)
	}
	if len(agentText) != 2 || agentText[0] != "Loose text" || agentText[1] != "Block text" {
		t.Fatalf("mixed assistant content = %#v", agentText)
	}
}

func TestClaudeWorktreeStateTopLevelIsKnown(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"worktree-state","timestamp":"2026-07-22T10:00:00Z","state":"clean"}`,
		`{"type":"file-history-delta","timestamp":"2026-07-22T10:00:01Z"}`,
	}, "\n") + "\n"
	result := projectClaude(t, input)
	if result.Summary.UnknownRows != 0 {
		t.Fatalf("top-level bookkeeping flagged unknown: %#v", result.Summary)
	}
}

func TestClaudeMetadataKnownTypeReconciliation(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","sessionId":"sess-1","timestamp":"2026-07-22T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"result","sessionId":"sess-1","timestamp":"2026-07-22T10:00:01Z"}`,
		`{"type":"file-history-delta","sessionId":"sess-1","timestamp":"2026-07-22T10:00:02Z"}`,
		`{"type":"worktree-state","sessionId":"sess-1","timestamp":"2026-07-22T10:00:03Z"}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects}, Path: "p/sess-1.jsonl", Kind: ThreadMain},
		Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.UnknownRows != 0 {
		t.Fatalf("metadata adapter flagged reconciled types unknown: %#v", result.Summary)
	}
}

func TestTurnUsageMigrationV7IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening re-runs initialize; the v7 ALTERs must not re-apply.
	store, err := Open(path)
	if err != nil {
		t.Fatalf("re-open after v7 migration: %v", err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(context.Background(),
		`SELECT usage_input, usage_cached, usage_output, usage_reasoning, usage_total, context_window FROM conversation_turns LIMIT 0`); err != nil {
		t.Fatalf("v7 usage columns absent: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations v7 rows = %d, want 1", count)
	}
}

func TestTurnUsageMigrationV11IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("re-open after v11 migration: %v", err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(context.Background(),
		`SELECT usage_cache_read, usage_cache_write, usage_native_json, usage_contract FROM conversation_turns LIMIT 0`); err != nil {
		t.Fatalf("v11 usage columns absent: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		`SELECT input_tokens, cache_read_tokens, cache_write_tokens FROM thread_turn_usage LIMIT 0`); err != nil {
		t.Fatalf("thread_turn_usage view absent: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 11`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations v11 rows = %d, want 1", count)
	}
}

func TestTurnUsageMigrationV13IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("re-open after v13 migration: %v", err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(context.Background(),
		`SELECT usage_model, usage_effort, usage_service_tier FROM conversation_turns LIMIT 0`); err != nil {
		t.Fatalf("v13 usage model/effort/service tier columns absent: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		`SELECT model, effort, service_tier FROM thread_turn_usage LIMIT 0`); err != nil {
		t.Fatalf("thread_turn_usage model/effort/service tier absent: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 13`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations v13 rows = %d, want 1", count)
	}
}

func storeRoundTripTurn(t *testing.T, provider Provider, rootKind RootKind, input string) TranscriptTurn {
	t.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rootSpec := SourceRoot{Provider: provider, Kind: rootKind, Path: filepath.Join(t.TempDir(), "root")}
	metadata := testSource(rootSpec, "thread.jsonl", "native-usage", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, provider)
	projected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: provider,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: projected,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Turns) != 1 {
		t.Fatalf("round-trip turns = %#v", page.Turns)
	}
	return page.Turns[0]
}

func assertTurnUsage(t *testing.T, usage *TranscriptTokenUsage, input, cached, output, reasoning, total, contextWindow int64) {
	t.Helper()
	if usage == nil {
		t.Fatal("turn usage not captured")
	}
	assertInt64Pointer(t, "input", usage.Input, input)
	assertInt64Pointer(t, "cached", usage.Cached, cached)
	assertInt64Pointer(t, "output", usage.Output, output)
	assertInt64Pointer(t, "reasoning", usage.Reasoning, reasoning)
	assertInt64Pointer(t, "total", usage.Total, total)
	assertInt64Pointer(t, "contextWindow", usage.ContextWindow, contextWindow)
}

func assertInt64Pointer(t *testing.T, label string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s usage is nil, want %d", label, want)
	}
	if *got != want {
		t.Fatalf("%s usage = %d, want %d", label, *got, want)
	}
}
