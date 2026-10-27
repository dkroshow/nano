package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompletedTranscriptTurnStoreBoundsAndIdentity(t *testing.T) {
	for _, name := range []string{"valid", "missing", "duplicate-turn", "in-progress", "missing-final", "duplicate-message", "truncated", "oversize", "too-many", "cancelled", "store-error"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
			metadata := testSource(root, "thread.jsonl", "native-thread", ThreadMain, nil)
			applied, err := store.ApplySource(ctx, metadata)
			if err != nil {
				t.Fatal(err)
			}
			stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
			if err != nil {
				t.Fatal(err)
			}
			adapter := newTranscriptFixture(t, ProviderCodex)
			result, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: strings.NewReader(codexTurnsJSONL(1, 2))})
			if err != nil {
				t.Fatal(err)
			}
			result.Turns[0].NativeTurnID = "exact-turn"
			result.Turns[1].NativeTurnID = "other-turn"
			if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: ProviderCodex, ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: result}); err != nil {
				t.Fatal(err)
			}
			turnID := "exact-turn"
			statement := ""
			switch name {
			case "missing":
				turnID = "missing"
			case "duplicate-turn":
				statement = "UPDATE conversation_turns SET native_turn_id='exact-turn'"
			case "in-progress":
				statement = "UPDATE conversation_turns SET status='in_progress'"
			case "missing-final":
				statement = "UPDATE conversation_turns SET final_item_id='missing'"
			case "duplicate-message":
				statement = "UPDATE conversation_items SET native_item_id='duplicate' WHERE item_type IN ('user_message','agent_message')"
			case "truncated":
				statement = "UPDATE conversation_items SET content_truncated=1 WHERE item_type='agent_message'"
			case "oversize":
				statement = "UPDATE conversation_items SET display_text=hex(zeroblob(150000)) WHERE item_type='agent_message'"
			case "too-many":
				for index := 0; index < 101; index++ {
					_, err := store.db.Exec(`INSERT INTO conversation_items(projection_id,item_id,projected_seq,source_key,turn_key,turn_ordinal,item_type,item_status,source_line,source_row_type) SELECT projection_id,?,1000+?, ?,turn_key,ordinal,'agent_message','completed',1,'test' FROM conversation_turns WHERE native_turn_id='exact-turn'`, fmt.Sprintf("extra-%d", index), index, fmt.Sprintf("extra-source-%d", index))
					if err != nil {
						t.Fatal(err)
					}
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "store-error":
				store.Close()
			}
			if statement != "" {
				if _, err := store.db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			page, err := store.ReadCompletedTranscriptTurn(ctx, applied.ThreadID, turnID)
			if name == "valid" {
				if err != nil || len(page.Items) != 2 || len(page.Turns) != 1 || page.Turns[0].NativeTurnID != "exact-turn" || !page.Items[1].IsFinalAnswer {
					t.Fatalf("page=%+v err=%v", page, err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s: %+v", name, page)
			}
		})
	}
}

func TestCodexTranscriptFixtureNormalizesCollaborationAndCanonicalFinal(t *testing.T) {
	data := readTranscriptFixture(t, "codex-v144-collaboration.jsonl")
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDenseTranscript(t, result.Items, 0)
	if len(result.Turns) != 2 || result.Turns[0].NativeTurnID != "turn-a" || result.Turns[1].NativeTurnID != "" {
		t.Fatalf("CODEX-SCOPE-1: native turn and unbound activity groups = %#v", result.Turns)
	}
	turn := result.Turns[0]
	if turn.Status != TurnCompleted || turn.CompletionEvidence != CompletionTaskComplete || turn.FinalitySource != "native" {
		t.Fatalf("completion = %#v", turn)
	}
	var userCount, agentCount int
	var final TranscriptItem
	seenTypes := map[TranscriptItemType]bool{}
	for _, item := range result.Items {
		seenTypes[item.Type] = true
		if item.Type == ItemUserMessage {
			userCount++
		}
		if item.Type == ItemAgentMessage {
			agentCount++
		}
		if item.IsFinalAnswer {
			final = item
		}
	}
	if userCount != 1 || agentCount != 1 {
		t.Fatalf("deduped message counts user=%d agent=%d", userCount, agentCount)
	}
	if final.NativeItemID != "canonical-final" || final.Phase != "final_answer" || final.Text != "The boundary is sound." {
		t.Fatalf("canonical final promotion = %#v", final)
	}
	for _, want := range []TranscriptItemType{ItemReasoning, ItemCommand, ItemToolResult, ItemNotice, ItemProviderEvent} {
		if !seenTypes[want] {
			t.Fatalf("missing normalized type %q", want)
		}
	}
	// CODEX-PART-1 retains native arguments and provider detail. Those facts
	// must not become operator messages or overwrite the canonical answer.
	for _, item := range result.Items {
		if item.Type == ItemUserMessage && strings.Contains(item.Text, "PRIVATE_SENTINEL") {
			t.Fatal("provider detail became an operator message")
		}
	}
	if result.Summary.UnknownRows != 1 {
		t.Fatalf("diagnostics = %#v", result.Summary)
	}
}

func TestClaudeTranscriptFixtureDerivesFinalFromTerminalEvidence(t *testing.T) {
	data := readTranscriptFixture(t, "claude-completion.jsonl")
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDenseTranscript(t, result.Items, 0)
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v", result.Turns)
	}
	turn := result.Turns[0]
	if turn.Status != TurnCompleted || turn.CompletionEvidence != CompletionStopReason || turn.DurationMS != 6000 {
		t.Fatalf("completion = %#v", turn)
	}
	var final TranscriptItem
	var sidechainFinal bool
	seenToolCall, seenToolResult, seenReasoning := false, false, false
	for _, item := range result.Items {
		if item.IsFinalAnswer {
			final = item
			sidechainFinal = item.Sidechain
		}
		seenToolCall = seenToolCall || item.Type == ItemToolCall
		seenToolResult = seenToolResult || item.Type == ItemToolResult
		seenReasoning = seenReasoning || item.Type == ItemReasoning
	}
	if final.Text != "The session is valid." || sidechainFinal {
		t.Fatalf("derived final = %#v", final)
	}
	if !seenToolCall || !seenToolResult || !seenReasoning {
		t.Fatalf("normalized Claude items missing: call=%v result=%v reasoning=%v", seenToolCall, seenToolResult, seenReasoning)
	}
	// CLAUDE-ORIGIN-1 keeps injected context in its native notice, separate
	// from operator prompts and the final answer.
	for _, item := range result.Items {
		if item.Type == ItemUserMessage && strings.Contains(item.Text, "PRIVATE_SENTINEL") {
			t.Fatal("injected context became an operator prompt")
		}
	}
}

func TestClaudeFable51SplitRowsKeepStreamBlockIdentity(t *testing.T) {
	data := readTranscriptFixture(t, "claude-fable-5-1-split.jsonl")
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	var agentTexts []string
	for _, item := range result.Items {
		switch item.Type {
		case ItemReasoning, ItemAgentMessage, ItemCommand:
			got = append(got, fmt.Sprintf("%s=%s", item.Type, item.NativeItemID))
		}
		if item.Type == ItemAgentMessage {
			agentTexts = append(agentTexts, item.Text)
		}
	}
	want := []string{
		"reasoning=msg_fable_main:0",
		"agent_message=msg_fable_main:1",
		"command=msg_fable_main:2",
		"reasoning=msg_fable_final:0",
		"agent_message=msg_fable_final:1",
		"agent_message=msg_fable_next:0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split Fable native identities = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(agentTexts, []string{
		"The main response is complete.",
		"The final response is complete.",
		"Next: owner — continue with the verified correction.",
	}) {
		t.Fatalf("split Fable agent messages = %#v", agentTexts)
	}
	if len(result.Turns) != 1 || result.Turns[0].Model != "claude-fable-5-1" ||
		result.Turns[0].Status != TurnCompleted || result.Turns[0].FinalItemID == "" {
		t.Fatalf("split Fable turn = %#v", result.Turns)
	}
	var final TranscriptItem
	for _, item := range result.Items {
		if item.IsFinalAnswer {
			final = item
		}
	}
	if final.Text != "Next: owner — continue with the verified correction." || final.NativeItemID != "msg_fable_next:0" {
		t.Fatalf("split Fable final = %#v", final)
	}
}

func TestClaudeFable51SplitIdentitySurvivesAppendCheckpoint(t *testing.T) {
	first := strings.Join([]string{
		`{"type":"user","uuid":"user-1","timestamp":"2026-09-01T21:29:19Z","message":{"role":"user","content":[{"type":"text","text":"Inspect"}]}}`,
		`{"type":"assistant","uuid":"row-thinking","timestamp":"2026-09-01T21:29:20.001Z","message":{"id":"msg_append","role":"assistant","model":"claude-fable-5-1","content":[{"type":"thinking","thinking":"Checking"}],"stop_reason":"end_turn"}}`,
	}, "\n") + "\n"
	adapter := newTranscriptFixture(t, ProviderClaude)
	initial, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(first),
	})
	if err != nil {
		t.Fatal(err)
	}
	appended := `{"type":"assistant","uuid":"row-text","timestamp":"2026-09-01T21:29:20.002Z","message":{"id":"msg_append","role":"assistant","model":"claude-fable-5-1","content":[{"type":"text","text":"Done"}],"stop_reason":"end_turn"}}` + "\n"
	continued, err := adapter.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(appended),
		ProjectionKey: initial.ProjectionKey, StartOffset: initial.CompleteOffset,
		PriorCompleteLines: initial.CompleteLines, PriorProjectedSeq: int64(len(initial.Items)),
		PreviousState: initial.ParserState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(continued.Items) < 1 {
		t.Fatalf("append emitted no items: %#v", continued)
	}
	var agent TranscriptItem
	for _, item := range continued.Items {
		if item.Type == ItemAgentMessage {
			agent = item
		}
	}
	if agent.NativeItemID != "msg_append:1" || agent.Text != "Done" {
		t.Fatalf("append split identity = %#v", agent)
	}
}

func TestClaudeExistingMultiBlockMessageKeepsBlockIdentity(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","uuid":"user-old","timestamp":"2026-08-01T20:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Inspect"}]}}`,
		`{"type":"assistant","uuid":"row-old","timestamp":"2026-08-01T20:00:01Z","message":{"id":"msg_old","role":"assistant","model":"claude-fable-5","content":[{"type":"thinking","thinking":"Checking"},{"type":"text","text":"One answer"},{"type":"tool_use","id":"toolu_old","name":"Read","input":{}}],"stop_reason":"tool_use"}}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range result.Items {
		if item.Type == ItemReasoning || item.Type == ItemAgentMessage || item.Type == ItemToolCall {
			got = append(got, item.NativeItemID)
		}
	}
	if !reflect.DeepEqual(got, []string{"msg_old:0", "msg_old:1", "msg_old:2"}) {
		t.Fatalf("existing Claude block identities = %#v", got)
	}
}

func TestClaudeSubagentSidechainEndTurnCompletesItsOwnTurn(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","isSidechain":true,"agentId":"worker-1","timestamp":"2026-08-04T22:11:47Z","message":{"role":"user","content":[{"type":"text","text":"Research the housing plan"}]}}`,
		`{"type":"assistant","isSidechain":true,"agentId":"worker-1","timestamp":"2026-08-04T22:18:43.020Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Preparing the final answer"}],"stop_reason":"end_turn"}}`,
		`{"type":"assistant","isSidechain":true,"agentId":"worker-1","timestamp":"2026-08-04T22:18:43.065Z","message":{"role":"assistant","content":[{"type":"text","text":"Deliverable written."}],"stop_reason":"end_turn"}}`,
	}, "\n") + "\n"
	source := transcriptCandidate(t, ProviderClaude)
	source.Kind = ThreadSubagent
	source.Path = filepath.Join(filepath.Dir(source.Path), "parent", "subagents", "agent-worker-1.jsonl")
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 {
		t.Fatalf("turns = %#v", result.Turns)
	}
	turn := result.Turns[0]
	if turn.Status != TurnCompleted || turn.CompletionEvidence != CompletionStopReason || turn.FinalItemID == "" {
		t.Fatalf("subagent completion = %#v", turn)
	}
	var final TranscriptItem
	for _, item := range result.Items {
		if item.IsFinalAnswer {
			final = item
		}
	}
	if final.Text != "Deliverable written." || !final.Sidechain {
		t.Fatalf("subagent final = %#v", final)
	}
}

func TestClaudeSubagentInterruptionIsTerminalNotANewPrompt(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","isSidechain":true,"agentId":"worker-1","timestamp":"2026-08-04T22:11:47Z","message":{"role":"user","content":[{"type":"text","text":"Research renovation cases"}]}}`,
		`{"type":"assistant","isSidechain":true,"agentId":"worker-1","timestamp":"2026-08-04T22:19:31Z","message":{"role":"assistant","content":[{"type":"text","text":"Writing the deliverable now."}]}}`,
		`{"type":"user","isSidechain":true,"agentId":"worker-1","timestamp":"2026-08-04T22:19:31.775Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
	}, "\n") + "\n"
	source := transcriptCandidate(t, ProviderClaude)
	source.Kind = ThreadSubagent
	source.Path = filepath.Join(filepath.Dir(source.Path), "parent", "subagents", "agent-worker-1.jsonl")
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].Status != TurnInterrupted ||
		result.Turns[0].CompletionEvidence != CompletionStopReason {
		t.Fatalf("interrupted subagent = %#v", result.Turns)
	}
	for _, item := range result.Items {
		if item.Type == ItemUserMessage && strings.Contains(item.Text, "interrupted") {
			t.Fatalf("provider interruption became a user prompt: %#v", item)
		}
	}
}

func TestClaudeSubsequentPromptPreservesIncompleteTurnWithoutFinalAnswer(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-13T19:10:00Z","message":{"role":"user","content":[{"type":"text","text":"First prompt"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-13T19:10:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Still working"}],"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2026-07-13T19:10:02Z","message":{"role":"user","content":[{"type":"text","text":"Second prompt"}]}}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 2 {
		t.Fatalf("turns = %#v", result.Turns)
	}
	first := result.Turns[0]
	if first.Status != TurnUnknown || first.CompletionEvidence != CompletionNone || first.CompletionSeq != 0 || first.CompletedAt != nil || first.FinalItemID != "" {
		t.Fatalf("incomplete prior turn = %#v", first)
	}
	for _, item := range result.Items {
		if item.TurnKey == first.Key && item.IsFinalAnswer {
			t.Fatalf("subsequent prompt fabricated final answer: %#v", item)
		}
	}
}

func TestClaudeInteractivePromptRemainsConversationVisible(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-13T19:10:00Z","message":{"role":"user","content":[{"type":"text","text":"Choose a target"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-13T19:10:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_question","name":"AskUserQuestion","input":{"questions":[{"header":"Target","question":"Where should this live?","options":[{"label":"Nano"},{"label":"v4"}]}]}}],"stop_reason":"tool_use"}}`,
		`{"type":"user","timestamp":"2026-07-13T19:10:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_question","content":"Where should this live?=Nano"}]}}`,
		`{"type":"system","subtype":"turn_duration","durationMs":2000,"timestamp":"2026-07-13T19:10:03Z"}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var interactions []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemInteraction || item.Type == ItemToolResult {
			interactions = append(interactions, item)
		}
	}
	if len(interactions) != 2 || interactions[0].Role != RoleAssistant || interactions[1].Role != RoleTool ||
		!strings.Contains(interactions[0].Text, "Where should this live?") ||
		transcriptItemCategory(interactions[0]) != CategoryMessages || transcriptItemCategory(interactions[1]) != CategoryToolResult || interactions[0].CallID != interactions[1].CallID {
		t.Fatalf("interactive conversation = %#v", interactions)
	}
}

func TestClaudeBashCommandRetainsCommandText(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-08-06T12:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Run the checks"}]}}`,
		`{"type":"assistant","timestamp":"2026-08-06T12:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"npm --prefix web test -- --runInBand","description":"Run the web tests"}}],"stop_reason":"tool_use"}}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.ToolName != "Bash" {
			continue
		}
		if item.Type != ItemCommand || item.Text != "npm --prefix web test -- --runInBand" {
			t.Fatalf("Bash command = %#v", item)
		}
		return
	}
	t.Fatalf("Bash command missing from %#v", result.Items)
}

func TestCodexInteractivePromptPairsCallAndOutput(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Authenticate"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"request_user_input","arguments":"{\"questions\":[{\"question\":\"Paste the callback URL\",\"options\":[{\"label\":\"Ready\"}]}]}","call_id":"call_question"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_question","output":"https://localhost/callback"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var interactions []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemInteraction || item.Type == ItemToolResult {
			interactions = append(interactions, item)
		}
	}
	if len(interactions) != 2 || interactions[0].CallID != "call_question" || interactions[1].Role != RoleTool {
		t.Fatalf("Codex interactive conversation = %#v", interactions)
	}
}

func TestCodexResponseSyntheticContextDoesNotBecomeOperatorMessage(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<permissions instructions>internal runtime context</permissions instructions>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Real operator prompt"}]}}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var users []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemUserMessage {
			users = append(users, item)
		}
	}
	if len(users) != 1 || users[0].Text != "Real operator prompt" {
		t.Fatalf("operator messages = %#v", users)
	}
}

func TestCodexRecommendedPluginsOpenerIsNotAnOperatorMessage(t *testing.T) {
	data := readTranscriptFixture(t, "codex-recommended-plugins-opener.jsonl")
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	var users []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemUserMessage {
			users = append(users, item)
		}
	}
	if len(users) != 1 || users[0].NativeItemID != "operator-prompt" || users[0].Text != "Implement the bounded Guardian classification slice." {
		t.Fatalf("operator messages = %#v", users)
	}
}

func TestCodexResponseUserContentDropsSeparableInjectedBlocks(t *testing.T) {
	input := `{"type":"response_item","payload":{"type":"message","id":"mixed","role":"user","content":[` +
		`{"type":"input_text","text":"<recommended_plugins>\\nAvailable plugins\\n</recommended_plugins>"},` +
		`{"type":"input_text","text":"# AGENTS.md instructions for /workspace"},` +
		`{"type":"input_text","text":"<environment_context>provider-owned context</environment_context>"},` +
		`{"type":"input_text","text":"Inspect the parser boundary."}]}}` + "\n"
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var users []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemUserMessage {
			users = append(users, item)
		}
	}
	if len(users) != 1 || users[0].NativeItemID != "mixed" || users[0].Text != "Inspect the parser boundary." {
		t.Fatalf("mixed user content = %#v", users)
	}
}

func TestCodexResponseUserContentPreservesSingleCombinedHarnessBlock(t *testing.T) {
	text := "<recommended_plugins>\nAvailable plugins\n</recommended_plugins>\n# AGENTS.md instructions for /workspace\n<environment_context>provider-owned context</environment_context>"
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	input := `{"type":"response_item","payload":{"type":"message","id":"ambiguous","role":"user","content":[{"type":"input_text","text":` + string(encoded) + `}]}}` + "\n"
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var users []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemUserMessage {
			users = append(users, item)
		}
	}
	if len(users) != 1 || users[0].NativeItemID != "ambiguous" || users[0].Text != text {
		t.Fatalf("ambiguous user content = %#v", users)
	}
}

func TestCodexResponseUserContentRetainsProgrammaticJudgePrompt(t *testing.T) {
	text := "Security judge: assess this security-grading result and return a structured verdict."
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	input := `{"type":"response_item","payload":{"type":"message","id":"judge","role":"user","content":[{"type":"input_text","text":` + string(encoded) + `}]}}` + "\n"
	result, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	var users []TranscriptItem
	for _, item := range result.Items {
		if item.Type == ItemUserMessage {
			users = append(users, item)
		}
	}
	if len(users) != 1 || users[0].NativeItemID != "judge" || users[0].Text != text {
		t.Fatalf("judge user content = %#v", users)
	}
}

func TestClaudeHousekeepingDoesNotCompleteGhostTurn(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"permission-mode","permissionMode":"default"}`,
		`{"type":"system","subtype":"stop_hook_summary"}`,
		`{"type":"system","subtype":"turn_duration","durationMs":4}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range result.Turns {
		if turn.Status == TurnCompleted {
			t.Fatalf("housekeeping completed a semantic turn: %#v", turn)
		}
	}
}

func TestAuditKnownProviderShapesStayClassified(t *testing.T) {
	codexInput := strings.Join([]string{
		`{"type":"event_msg","payload":{"type":"user_message","message":"Inspect shapes"}}`,
		`{"type":"event_msg","payload":{"type":"guardian_assessment"}}`,
		`{"type":"event_msg","payload":{"type":"web_search_end","result":"done"}}`,
		`{"type":"event_msg","payload":{"type":"agent_reasoning","text":"considering"}}`,
		`{"type":"event_msg","payload":{"type":"thread_goal_updated"}}`,
		`{"type":"event_msg","payload":{"type":"mcp_tool_call_end","output":"ok"}}`,
		`{"type":"event_msg","payload":{"type":"collab_waiting_end"}}`,
		`{"type":"event_msg","payload":{"type":"error","message":"provider failed"}}`,
		`{"type":"response_item","payload":{"type":"tool_search_call","name":"tool_search","call_id":"search"}}`,
		`{"type":"response_item","payload":{"type":"tool_search_output","call_id":"search","output":"found"}}`,
	}, "\n") + "\n"
	codex, err := newTranscriptFixture(t, ProviderCodex).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(codexInput),
	})
	if err != nil || codex.Summary.UnknownRows != 0 {
		t.Fatalf("Codex known shapes = %#v, %v", codex.Summary, err)
	}

	claudeInput := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Inspect shapes"}]}}`,
		`{"type":"system","subtype":"stop_hook_summary"}`,
		`{"type":"system","subtype":"local_command"}`,
		`{"type":"system","subtype":"scheduled_task_fire"}`,
		`{"type":"system","subtype":"worktree-state"}`,
		`{"type":"system","subtype":"file-history-delta"}`,
		`{"type":"system","subtype":"model_refusal_fallback"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"fallback","from":"opus","to":"sonnet"}]}}`,
		`{"type":"system","subtype":"api_error"}`,
	}, "\n") + "\n"
	claude, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(claudeInput),
	})
	if err != nil || claude.Summary.UnknownRows != 0 {
		t.Fatalf("Claude known shapes = %#v, %v", claude.Summary, err)
	}
}

func TestEvidenceOnlyRowsPreserveCompletedTurnAndCoverMalformedRecords(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-13T19:20:00Z","message":{"role":"user","content":[{"type":"text","text":"Prompt"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-13T19:20:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn"}}`,
		`{"type":"custom-title","customTitle":"A title"}`,
		`{not valid json}`,
	}, "\n") + "\n"
	result, err := newTranscriptFixture(t, ProviderClaude).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 1 || result.Turns[0].Status != TurnCompleted ||
		result.Turns[0].CompletionEvidence != CompletionStopReason || result.Turns[0].FinalItemID == "" {
		t.Fatalf("evidence rows corrupted semantic completion: %#v", result.Turns)
	}
	seenLines := map[int]bool{}
	for _, item := range result.Items {
		seenLines[item.SourceLine] = true
	}
	for line := 1; line <= 4; line++ {
		if !seenLines[line] {
			t.Fatalf("native line %d has no Dev evidence handle: %#v", line, result.Items)
		}
	}
}

func TestTranscriptCategoryPrecedenceTreatsSidechainAsSubagent(t *testing.T) {
	item := TranscriptItem{Type: ItemAgentMessage, Status: ItemStatusCompleted, Sidechain: true}
	if got := transcriptItemCategory(item); got != CategorySubagent {
		t.Fatalf("sidechain category = %q", got)
	}
	item.Status = ItemStatusFailed
	if got := transcriptItemCategory(item); got != CategoryError {
		t.Fatalf("failed sidechain category = %q", got)
	}
}

func TestTranscriptSQLAndGoCategoryClassifiersAgree(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases := []TranscriptItem{
		{Type: ItemUserMessage, Status: ItemStatusCompleted, Role: RoleUser},
		{Type: ItemInteraction, Status: ItemStatusInterrupted, Role: RoleUser},
		{Type: ItemReasoning, Status: ItemStatusCompleted, Role: RoleAssistant},
		{Type: ItemCommand, Status: ItemStatusCompleted, Role: RoleAssistant},
		{Type: ItemToolResult, Status: ItemStatusCompleted, Role: RoleTool},
		{Type: ItemAgentMessage, Status: ItemStatusCompleted, Role: RoleAssistant, Sidechain: true},
		{Type: ItemNotice, Status: ItemStatusCompleted, Role: RoleSystem},
		{Type: ItemUserMessage, Status: ItemStatusFailed, Role: RoleUser},
		{Type: ItemToolResult, Status: ItemStatusFailed, Role: RoleTool},
		{Type: TranscriptItemType("future_item"), Status: ItemStatusUnknown, Role: RoleAssistant},
		// Enhanced hides every shell command and its result, failed or not,
		// a subagent's included.
		{Type: ItemCommand, Status: ItemStatusFailed, Role: RoleAssistant},
		{Type: ItemCommand, Status: ItemStatusInterrupted, Role: RoleAssistant},
		{Type: ItemToolResult, Status: ItemStatusFailed, Role: RoleTool, FilterTags: []string{"command"}},
		{Type: ItemToolResult, Status: ItemStatusCompleted, Role: RoleTool, Sidechain: true, FilterTags: []string{"command"}},
	}
	want := []TranscriptCategory{
		CategoryMessages, CategoryMessages, CategoryReasoning, CategoryToolCall, CategoryToolResult,
		CategorySubagent, CategorySystem, CategoryError, CategoryError, CategoryUnclassified,
		CategoryToolCall, CategoryToolCall, CategoryToolResult, CategoryToolResult,
	}
	query := `WITH conversation_items(
  projection_id, turn_key, projected_seq, item_status, sidechain, item_type, filter_tags_json, role
) AS (VALUES (1, 'turn', 1, ?, ?, ?, ?, ?))
SELECT ` + transcriptCategorySQL + ` FROM conversation_items`
	for index, item := range cases {
		tags, marshalErr := json.Marshal(item.FilterTags)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var sqlCategory TranscriptCategory
		if err := store.db.QueryRowContext(context.Background(), query,
			item.Status, item.Sidechain, item.Type, string(tags), item.Role).Scan(&sqlCategory); err != nil {
			t.Fatal(err)
		}
		goCategory := transcriptItemCategory(item)
		if goCategory != want[index] || sqlCategory != want[index] || sqlCategory != goCategory {
			t.Fatalf("case %d: Go=%q SQL=%q want=%q", index, goCategory, sqlCategory, want[index])
		}
	}
}

// A failed tool call is part of the agent's work while its turn runs and once
// the turn completes with an answer, so it stays with the tool results the
// Enhanced view hides. It is an error only when its turn ends without one: a
// file-only pane reads saved rows mid-turn, and a failure the agent is still
// working through must not flash into Enhanced. A failed shell command's
// result is never an error: Enhanced hides every shell command.
func TestResolvedToolFailureClassifiesAsHiddenToolResult(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Stored turn shapes: a running Claude turn is saved as "unknown" and a
	// running Codex turn as "in_progress", both with no completion sequence.
	for _, test := range []struct {
		status     string
		completion int
		final      string
		tags       string
		want       TranscriptCategory
	}{
		{"completed", 5, "answer", "[]", CategoryToolResult},
		{"unknown", 0, "", "[]", CategoryToolResult},
		{"in_progress", 0, "", "[]", CategoryToolResult},
		{"completed", 5, "", "[]", CategoryError},
		{"interrupted", 5, "", "[]", CategoryError},
		{"truncated", 5, "", "[]", CategoryError},
		{"unknown", 5, "", "[]", CategoryError},
		{"interrupted", 5, "", `["command"]`, CategoryToolResult},
		{"completed", 5, "", `["command"]`, CategoryToolResult},
	} {
		query := `WITH
conversation_items(
  projection_id, turn_key, projected_seq, item_status, sidechain, item_type, filter_tags_json, role
) AS (VALUES (1, 'turn', 1, 'failed', 0, 'tool_result', ?, 'tool')),
conversation_turns(projection_id, turn_key, status, completion_seq, final_item_id) AS (
  VALUES (1, 'turn', ?, ?, ?)
)
SELECT ` + transcriptCategorySQL + ` FROM conversation_items`
		var category TranscriptCategory
		if err := store.db.QueryRowContext(context.Background(), query, test.tags, test.status, test.completion, test.final).Scan(&category); err != nil {
			t.Fatal(err)
		}
		if category != test.want {
			t.Errorf("failed result tagged %s in a %s turn (completion %d, final %q) = %q, want %q", test.tags, test.status, test.completion, test.final, category, test.want)
		}
	}
}

func TestClaudeUnknownSystemAndContentShapesAreDiagnosed(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-13T19:20:00Z","message":{"role":"user","content":[{"type":"future_user_block","private":"PRIVATE_SENTINEL"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-13T19:20:01Z","message":{"role":"assistant","content":[{"type":"future_assistant_block","private":"PRIVATE_SENTINEL"}]}}`,
		`{"type":"system","timestamp":"2026-07-13T19:20:02Z","subtype":"future_system_subtype","private":"PRIVATE_SENTINEL"}`,
	}, "\n") + "\n"
	reader := newTranscriptFixture(t, ProviderClaude)
	result, err := reader.ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderClaude), Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := reader.nativeDiagnostics(result.ProjectionKey)
	if len(diagnostics) != 2 || diagnostics[0].Code != "unknown_content_block" || diagnostics[1].Code != "unknown_content_block" {
		t.Fatalf("unknown-content diagnostics = %+v", diagnostics)
	}
	if len(result.Items) != 3 {
		t.Fatalf("unknown rows must each have a Dev evidence handle: %#v", result.Items)
	}
	for _, item := range result.Items[:2] {
		if item.Type != ItemProviderEvent || item.Status != ItemStatusUnknown || !strings.HasPrefix(item.SourcePayloadType, "future_") {
			t.Fatalf("unknown Claude block lost its evidence handle: %#v", item)
		}
	}
	encoded, _ := json.Marshal(result.Items)
	if strings.Contains(string(encoded), "PRIVATE_SENTINEL") || strings.Contains(string(encoded), "future_") {
		t.Fatalf("unknown Claude JSON leaked into transcript: %s", encoded)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTranscriptProjectionWaitsForCompleteLineAndAppendsDensely(t *testing.T) {
	ctx := context.Background()
	adapter := newTranscriptFixture(t, ProviderCodex)
	firstLine := `{"timestamp":"2026-07-13T20:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"Hello"}}` + "\n"
	partial := `{"timestamp":"2026-07-13T20:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Hi"}]}}`
	first, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(firstLine + partial),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.CompleteLines != 1 || first.CompleteOffset != int64(len(firstLine)) || len(first.Items) != 1 {
		t.Fatalf("incomplete checkpoint = %#v", first)
	}
	appendBytes := partial + "\n" + `{"timestamp":"2026-07-13T20:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
	second, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: transcriptCandidate(t, ProviderCodex), Reader: strings.NewReader(appendBytes),
		ProjectionKey: first.ProjectionKey, StartOffset: first.CompleteOffset,
		PriorCompleteLines: first.CompleteLines, PriorProjectedSeq: 1,
		PreviousState: first.ParserState,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDenseTranscript(t, second.Items, 1)
	if len(second.Items) != 2 || second.CompleteLines != 3 || second.Turns[0].Status != TurnCompleted {
		t.Fatalf("append projection = %#v", second)
	}
}

func TestTranscriptStoreAppendPagingRebuildAndRetention(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(rootSpec, "thread.jsonl", "native-transcript", ThreadMain, nil)
	metadata.Source.Size = 4096
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok || !stored.CanonicalTranscriptSource || stored.ThreadID != applied.ThreadID {
		t.Fatalf("stored canonical source = %#v, %v, %v", stored, ok, err)
	}

	initial := []byte(codexTurnsJSONL(1, 2))
	adapter := newTranscriptFixture(t, ProviderCodex)
	first, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: bytes.NewReader(initial)})
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		Result: first,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldHead := written.LatestSeq
	checkpoint, ok, err := store.LoadTranscriptCheckpoint(ctx, applied.ThreadID)
	if err != nil || !ok || checkpoint.CompleteOffset != int64(len(initial)) || checkpoint.LatestSeq != oldHead {
		t.Fatalf("checkpoint = %#v, %v, %v", checkpoint, ok, err)
	}

	appendData := []byte(codexTurnsJSONL(3, 3))
	appendResult, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: metadata.Source, Reader: bytes.NewReader(appendData), ProjectionKey: checkpoint.ProjectionKey,
		StartOffset: checkpoint.CompleteOffset, PriorCompleteLines: checkpoint.CompleteLines,
		PriorProjectedSeq: checkpoint.LatestSeq, PreviousState: checkpoint.ParserState,
	})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptAppend, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: appendResult,
	})
	if err != nil {
		t.Fatal(err)
	}
	if appended.ProjectionKey != checkpoint.ProjectionKey || appended.LatestSeq <= oldHead {
		t.Fatalf("append = %#v", appended)
	}

	tail, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Items) != 3 || tail.StartSeq != oldHead+1 || !tail.HasMoreBefore || tail.StartsMidTurn {
		t.Fatalf("turn-aware tail = %#v", tail)
	}
	turnWindow, err := store.ReadTranscript(ctx, TranscriptReadRequest{
		ThreadID: applied.ThreadID, BeforeTurnOrdinal: 3, TurnLimit: 1, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turnWindow.Turns) != 1 || turnWindow.Turns[0].Ordinal != 2 || len(turnWindow.Items) != 3 {
		t.Fatalf("turn window = %#v", turnWindow)
	}
	older, err := store.ReadTranscript(ctx, TranscriptReadRequest{
		ThreadID: applied.ThreadID, ExpectedProjectionKey: tail.Projection.Key,
		SnapshotHeadSeq: tail.SnapshotHeadSeq, BeforeSeq: tail.StartSeq, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Items) != 3 || older.EndSeq+1 != tail.StartSeq {
		t.Fatalf("older page = %#v", older)
	}
	pinned, err := store.ReadTranscript(ctx, TranscriptReadRequest{
		ThreadID: applied.ThreadID, ExpectedProjectionKey: checkpoint.ProjectionKey,
		SnapshotHeadSeq: oldHead, Limit: 100,
	})
	if err != nil || pinned.EndSeq != oldHead || pinned.LatestSeq != appended.LatestSeq {
		t.Fatalf("pinned snapshot = %#v, %v", pinned, err)
	}

	if err := store.MarkTranscriptError(ctx, applied.ThreadID, stored.ID, "fixture parser failed", time.Now()); err != nil {
		t.Fatal(err)
	}
	failedCheckpoint, ok, err := store.LoadTranscriptCheckpoint(ctx, applied.ThreadID)
	if err != nil || !ok || failedCheckpoint.ErrorSummary == nil || *failedCheckpoint.ErrorSummary != "fixture parser failed" {
		t.Fatalf("failed checkpoint = %#v, %v, %v", failedCheckpoint, ok, err)
	}
	degraded, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	if err != nil || degraded.Projection.State != TranscriptDegraded || degraded.Projection.ItemCount != appended.ItemCount {
		t.Fatalf("retained failure = %#v, %v", degraded, err)
	}

	rebuiltResult, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: metadata.Source, Reader: bytes.NewReader(append(initial, appendData...)),
	})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: rebuiltResult,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ProjectionKey == checkpoint.ProjectionKey {
		t.Fatal("rebuild retained projection key")
	}
	_, err = store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, ExpectedProjectionKey: checkpoint.ProjectionKey})
	if !errors.Is(err, ErrTranscriptChanged) {
		t.Fatalf("old projection read error = %v", err)
	}
	current, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, Limit: 1})
	if err != nil || len(current.Items) == 0 {
		t.Fatalf("current projection = %#v, %v", current, err)
	}
	if _, err := store.LoadTranscriptEvidenceRef(ctx, applied.ThreadID, current.Items[0].ID); err != nil {
		t.Fatalf("current evidence ref = %v", err)
	}
	if _, err := store.LoadTranscriptEvidenceRef(ctx, applied.ThreadID, "nit_stale_after_rebuild"); !errors.Is(err, ErrTranscriptEvidenceUnavailable) {
		t.Fatalf("stale item evidence ref = %v", err)
	}
	all, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var expectedVisible []int64
	for _, item := range all.Items {
		if item.Category == CategoryMessages {
			expectedVisible = append(expectedVisible, item.ProjectedSeq)
		}
	}
	hiddenNonMessages := []TranscriptCategory{
		CategoryReasoning, CategoryToolCall, CategoryToolResult, CategorySubagent,
		CategorySystem, CategoryError, CategoryUnclassified,
	}
	var newestFirstPages [][]int64
	var snapshot, before int64
	for {
		page, pageErr := store.ReadTranscript(ctx, TranscriptReadRequest{
			ThreadID: applied.ThreadID, ExpectedProjectionKey: rebuilt.ProjectionKey,
			SnapshotHeadSeq: snapshot, BeforeSeq: before, Limit: 1,
			HiddenCategories: hiddenNonMessages,
		})
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		if snapshot == 0 {
			snapshot = page.SnapshotHeadSeq
		}
		seqs := make([]int64, len(page.Items))
		for index, item := range page.Items {
			seqs[index] = item.ProjectedSeq
			if index > 0 && seqs[index-1] >= seqs[index] {
				t.Fatalf("filtered page is not ascending: %v", seqs)
			}
		}
		newestFirstPages = append(newestFirstPages, seqs)
		if !page.HasMoreBefore {
			break
		}
		if page.StartSeq <= 0 {
			t.Fatalf("filtered page has more but no boundary: %#v", page)
		}
		before = page.StartSeq
	}
	var actualVisible []int64
	for pageIndex := len(newestFirstPages) - 1; pageIndex >= 0; pageIndex-- {
		actualVisible = append(actualVisible, newestFirstPages[pageIndex]...)
	}
	if !reflect.DeepEqual(actualVisible, expectedVisible) {
		t.Fatalf("filtered paging seqs = %v, want %v", actualVisible, expectedVisible)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE source_files SET generation = generation + 1 WHERE id = ?`, stored.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadTranscriptEvidenceRef(ctx, applied.ThreadID, current.Items[0].ID); !errors.Is(err, ErrTranscriptChanged) {
		t.Fatalf("generation-mismatched evidence ref = %v", err)
	}

	root, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileRoot(ctx, RootReconciliation{RootID: root.ID, State: IndexReady, CompletedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	retained, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	if err != nil || retained.Projection.State != TranscriptRetained || retained.Projection.EvidenceAvailable || retained.Projection.ItemCount == 0 {
		t.Fatalf("missing-source retention = %#v, %v", retained, err)
	}
}

func TestGrokTranscriptControlsCatalogRecency(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	root := SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	reconnectAt := time.Date(2026, 8, 17, 14, 6, 11, 0, time.UTC)
	metadata := testSource(root, "updates.jsonl", "grok-native", ThreadMain, &reconnectAt)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %v, %v", stored, ok, err)
	}
	versionBefore, err := store.CatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	userAt := time.Date(2026, 8, 13, 20, 17, 55, 312000000, time.UTC)
	agentAt := time.Date(2026, 8, 13, 20, 18, 2, 895000000, time.UTC)
	turn := TranscriptTurn{Key: "turn-1", Ordinal: 1, StartSeq: 1, EndSeq: 3, Status: TurnCompleted, CompletionEvidence: CompletionStopReason, FinalItemID: "agent"}
	items := []TranscriptItem{
		{ID: "user", ProjectedSeq: 1, SourceKey: "line:1:0", TurnKey: turn.Key, TurnOrdinal: 1, Type: ItemUserMessage, Status: ItemStatusCompleted, Role: RoleUser, Text: "done elsewhere", CreatedAt: &userAt, SourceLine: 1, SourceRowType: "session/update", SourceLineSHA256: strings.Repeat("1", sha256.Size*2)},
		{ID: "agent", ProjectedSeq: 2, SourceKey: "line:2:0", TurnKey: turn.Key, TurnOrdinal: 1, Type: ItemAgentMessage, Status: ItemStatusCompleted, Role: RoleAssistant, Text: "I will stand down", CreatedAt: &agentAt, IsFinalAnswer: true, SourceLine: 2, SourceRowType: "session/update", SourceLineSHA256: strings.Repeat("2", sha256.Size*2)},
		{ID: "housekeeping", ProjectedSeq: 3, SourceKey: "line:3:0", TurnKey: turn.Key, TurnOrdinal: 1, Type: ItemProviderEvent, Status: ItemStatusUnknown, Role: RoleSystem, Text: "session_end hook", CreatedAt: &reconnectAt, SourceLine: 3, SourceRowType: "session/update", SourceLineSHA256: strings.Repeat("3", sha256.Size*2)},
	}
	_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderGrok,
		ParserVersion: "grok-recency-test", CompletionRuleVersion: "grok-recency-test",
		Result: TranscriptProjectionResult{ProjectionKey: "grok-recency", CompleteOffset: metadata.Source.Size, CompleteLines: 3, ParserState: json.RawMessage(`{}`), Items: items, Turns: []TranscriptTurn{turn}},
	})
	if err != nil {
		t.Fatal(err)
	}

	detail, err := store.GetThread(ctx, applied.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Thread.UpdatedAt == nil || !detail.Thread.UpdatedAt.Equal(agentAt) {
		t.Fatalf("thread updatedAt = %v, want last conversation message %v", detail.Thread.UpdatedAt, agentAt)
	}
	versionAfter, err := store.CatalogVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if versionAfter <= versionBefore {
		t.Fatalf("catalog version did not advance for corrected recency: before=%d after=%d", versionBefore, versionAfter)
	}

	// A later metadata refresh may report another reconnect timestamp before
	// transcript repair runs. Once conversation evidence exists, metadata must
	// not temporarily overwrite it.
	laterReconnect := reconnectAt.Add(time.Hour)
	metadata.Result.Projection.UpdatedAt = &laterReconnect
	metadata.Source.Size++
	metadata.Result.CompleteOffset++
	if _, err := store.ApplySource(ctx, metadata); err != nil {
		t.Fatal(err)
	}
	detail, err = store.GetThread(ctx, applied.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Thread.UpdatedAt == nil || !detail.Thread.UpdatedAt.Equal(agentAt) {
		t.Fatalf("metadata refresh replaced conversation recency: got %v, want %v", detail.Thread.UpdatedAt, agentAt)
	}
}

func TestTranscriptProjectionMovesLosslesslyWhenSourceIdentityChanges(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(rootSpec, "reassigned.jsonl", "native-old", ThreadMain, nil)
	old, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, rootSpec)
	stored, ok, err := store.LoadSource(ctx, rootID, metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	data := []byte(codexLargeTurnJSONL())
	projected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: old.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: projected,
	})
	if err != nil {
		t.Fatal(err)
	}
	var itemsBefore, bodiesBefore int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_items`).Scan(&itemsBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_item_bodies`).Scan(&bodiesBefore); err != nil {
		t.Fatal(err)
	}
	if bodiesBefore != 1 {
		t.Fatalf("large-body fixture stored %d bodies", bodiesBefore)
	}

	reassigned := metadata
	reassigned.Result = &ProjectionResult{
		Projection:     metadata.Result.Projection,
		CompleteOffset: metadata.Result.CompleteOffset,
		CompleteLines:  metadata.Result.CompleteLines,
	}
	reassigned.Result.Projection.NativeThreadID = "native-new"
	newResult, err := store.ApplySource(ctx, reassigned)
	if err != nil {
		t.Fatalf("identity reassignment: %v", err)
	}
	if newResult.ThreadID == old.ThreadID {
		t.Fatal("identity reassignment reused old Nano thread")
	}
	if _, err := store.GetThread(ctx, old.ThreadID); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("old thread survived without a source: %v", err)
	}
	newPage, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: newResult.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	if newPage.Projection.State != TranscriptReady || newPage.Projection.Key != written.ProjectionKey || len(newPage.Items) != itemsBefore {
		t.Fatalf("transcript was not reassociated losslessly: %#v", newPage)
	}
	var retainedItemID string
	for _, item := range newPage.Items {
		if item.ContentRef != "" {
			retainedItemID = item.ID
			break
		}
	}
	body, err := store.LoadTranscriptBody(ctx, newResult.ThreadID, retainedItemID)
	if err != nil || body.ProjectionKey != written.ProjectionKey || body.StoredBytes <= maxTranscriptInlineBytes || body.Text == "" {
		t.Fatalf("retained body = %#v, %v", body, err)
	}
	var itemsAfter, bodiesAfter int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_items`).Scan(&itemsAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_item_bodies`).Scan(&bodiesAfter); err != nil {
		t.Fatal(err)
	}
	if itemsAfter != itemsBefore || bodiesAfter != bodiesBefore {
		t.Fatalf("reassignment changed evidence counts items %d->%d bodies %d->%d", itemsBefore, itemsAfter, bodiesBefore, bodiesAfter)
	}
	storedAfter, ok, err := store.LoadSource(ctx, rootID, metadata.Source.Path)
	if err != nil || !ok || !storedAfter.CanonicalTranscriptSource {
		t.Fatalf("moved active source is not canonical: %#v, %v, %v", storedAfter, ok, err)
	}
}

func TestLoadSourcesPrefersPresentDuplicateWhenActiveTranscriptSourceIsMissing(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	oldSource := testSource(rootSpec, "old.jsonl", "claude-moved", ThreadMain, nil)
	newSource := testSource(rootSpec, "worktree/new.jsonl", "claude-moved", ThreadMain, nil)
	oldThread, err := store.ApplySource(ctx, oldSource)
	if err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, rootSpec)
	storedOld, ok, err := store.LoadSource(ctx, rootID, oldSource.Source.Path)
	if err != nil || !ok {
		t.Fatalf("old source = %#v, %v, %v", storedOld, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderClaude)
	projected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: oldSource.Source,
		Reader: strings.NewReader(`{"type":"user","timestamp":"2026-08-06T12:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: oldThread.ThreadID, SourceID: storedOld.ID,
		SourceGeneration: storedOld.Generation, Provider: ProviderClaude,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: projected,
	}); err != nil {
		t.Fatal(err)
	}
	newThread, err := store.ApplySource(ctx, newSource)
	if err != nil {
		t.Fatal(err)
	}
	if newThread.ThreadID != oldThread.ThreadID {
		t.Fatalf("duplicate native thread split into %q and %q", oldThread.ThreadID, newThread.ThreadID)
	}
	if _, err := store.ReconcileRoot(ctx, RootReconciliation{
		RootID: rootID, ObservedPaths: []string{newSource.Source.Path}, State: IndexReady, CompletedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sources, err := store.LoadSourcesByRoot(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if sources[oldSource.Source.Path].CanonicalTranscriptSource || !sources[newSource.Source.Path].CanonicalTranscriptSource {
		t.Fatalf("canonical sources old=%#v new=%#v", sources[oldSource.Source.Path], sources[newSource.Source.Path])
	}
	newProjected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: newSource.Source,
		Reader: strings.NewReader(`{"type":"user","timestamp":"2026-08-06T12:01:00Z","message":{"role":"user","content":[{"type":"text","text":"from the moved source"}]}}` + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, ok, err := store.LoadTranscriptCheckpoint(ctx, oldThread.ThreadID)
	if err != nil || !ok {
		t.Fatalf("old checkpoint = %#v, %v, %v", checkpoint, ok, err)
	}
	storedNew := sources[newSource.Source.Path]
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: oldThread.ThreadID, SourceID: storedNew.ID,
		SourceGeneration: storedNew.Generation, Provider: ProviderClaude,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: newProjected,
	}); err != nil {
		t.Fatalf("rebuild from present duplicate: %v", err)
	}
	updated, ok, err := store.LoadTranscriptCheckpoint(ctx, oldThread.ThreadID)
	if err != nil || !ok || updated.SourceID != storedNew.ID {
		t.Fatalf("moved checkpoint = %#v, %v, %v", updated, ok, err)
	}
}

func TestTranscriptReassignmentKeepsExistingTargetActiveAndSupersedesMovedProjection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	oldSource := testSource(rootSpec, "old.jsonl", "native-old", ThreadMain, nil)
	targetSource := testSource(rootSpec, "target.jsonl", "native-target", ThreadMain, nil)
	oldThread, err := store.ApplySource(ctx, oldSource)
	if err != nil {
		t.Fatal(err)
	}
	targetThread, err := store.ApplySource(ctx, targetSource)
	if err != nil {
		t.Fatal(err)
	}
	rootID := mustRootID(t, store, rootSpec)
	storedOld, ok, err := store.LoadSource(ctx, rootID, oldSource.Source.Path)
	if err != nil || !ok {
		t.Fatalf("old source = %#v, %v, %v", storedOld, ok, err)
	}
	storedTarget, ok, err := store.LoadSource(ctx, rootID, targetSource.Source.Path)
	if err != nil || !ok {
		t.Fatalf("target source = %#v, %v, %v", storedTarget, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderCodex)
	oldProjection, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: oldSource.Source, Reader: strings.NewReader(codexLargeTurnJSONL())})
	if err != nil {
		t.Fatal(err)
	}
	oldWritten, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: oldThread.ThreadID, SourceID: storedOld.ID,
		SourceGeneration: storedOld.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: oldProjection,
	})
	if err != nil {
		t.Fatal(err)
	}
	targetProjection, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: targetSource.Source, Reader: strings.NewReader(codexTurnsJSONL(2, 2))})
	if err != nil {
		t.Fatal(err)
	}
	targetWritten, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: targetThread.ThreadID, SourceID: storedTarget.ID,
		SourceGeneration: storedTarget.Generation, Provider: ProviderCodex,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: targetProjection,
	})
	if err != nil {
		t.Fatal(err)
	}
	var itemsBefore, bodiesBefore int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_items`).Scan(&itemsBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_item_bodies`).Scan(&bodiesBefore); err != nil {
		t.Fatal(err)
	}
	if bodiesBefore == 0 {
		t.Fatal("existing-target fixture did not create a retained item body")
	}

	reassigned := oldSource
	reassigned.Result = &ProjectionResult{Projection: oldSource.Result.Projection,
		CompleteOffset: oldSource.Result.CompleteOffset, CompleteLines: oldSource.Result.CompleteLines}
	reassigned.Result.Projection.NativeThreadID = "native-target"
	moved, err := store.ApplySource(ctx, reassigned)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ThreadID != targetThread.ThreadID {
		t.Fatalf("reassignment target = %q, want %q", moved.ThreadID, targetThread.ThreadID)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: targetThread.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	if page.Projection.Key != targetWritten.ProjectionKey || page.Projection.Key == oldWritten.ProjectionKey {
		t.Fatalf("active projection selection = %#v", page.Projection)
	}
	var movedState string
	if err := store.db.QueryRowContext(ctx,
		`SELECT state FROM conversation_projections WHERE projection_key = ? AND thread_id = ?`,
		oldWritten.ProjectionKey, targetThread.ThreadID).Scan(&movedState); err != nil {
		t.Fatal(err)
	}
	if movedState != "superseded" {
		t.Fatalf("moved projection state = %q", movedState)
	}
	var itemsAfter, bodiesAfter int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_items`).Scan(&itemsAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_item_bodies`).Scan(&bodiesAfter); err != nil {
		t.Fatal(err)
	}
	if itemsAfter != itemsBefore || bodiesAfter != bodiesBefore {
		t.Fatalf("target reassignment changed evidence counts items %d->%d bodies %d->%d", itemsBefore, itemsAfter, bodiesBefore, bodiesAfter)
	}
	oldAfter, ok, err := store.LoadSource(ctx, rootID, oldSource.Source.Path)
	if err != nil || !ok || oldAfter.CanonicalTranscriptSource {
		t.Fatalf("superseded moved source remained canonical: %#v, %v, %v", oldAfter, ok, err)
	}
	targetAfter, ok, err := store.LoadSource(ctx, rootID, targetSource.Source.Path)
	if err != nil || !ok || !targetAfter.CanonicalTranscriptSource {
		t.Fatalf("existing active source lost canonical status: %#v, %v, %v", targetAfter, ok, err)
	}
}

func TestTranscriptTurnWindowKeepsSemanticBoundariesAndExcludesControlOnlyTurns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
	metadata := testSource(root, "absent.jsonl", "turn-window", ThreadMain, nil)
	thread, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	result := TranscriptProjectionResult{ProjectionKey: "turn-window", CompleteOffset: 7, CompleteLines: 7, ParserState: []byte(`{}`)}
	result.Turns = []TranscriptTurn{
		{Key: "outer", Ordinal: 1, StartSeq: 1, EndSeq: 5},
		{Key: "control", Ordinal: 2, StartSeq: 2, EndSeq: 4},
		{Key: "inner", Ordinal: 3, StartSeq: 3, EndSeq: 3},
		{Key: "trailing", Ordinal: 4, StartSeq: 6, EndSeq: 7},
	}
	for i, turn := range result.Turns {
		result.Turns[i].Status, result.Turns[i].CompletionEvidence = TurnInProgress, CompletionNone
		for seq := turn.StartSeq; seq <= turn.EndSeq; seq++ {
			if (turn.Key == "outer" || turn.Key == "control") && seq != turn.StartSeq && seq != turn.EndSeq {
				continue
			}
			kind := ItemAgentMessage
			if turn.Key == "control" || turn.Key == "trailing" {
				kind = ItemProviderEvent
			}
			id := fmt.Sprintf("item-%d", seq)
			result.Items = append(result.Items, TranscriptItem{ID: id, SourceKey: id, ProjectedSeq: seq,
				TurnKey: turn.Key, TurnOrdinal: turn.Ordinal, Type: kind, Status: ItemStatusCompleted,
				Text: id, SourceLine: int(seq), SourceRowType: "test", SourceLineSHA256: strings.Repeat("a", 64)})
		}
	}
	slices.SortFunc(result.Items, func(a, b TranscriptItem) int { return int(a.ProjectedSeq - b.ProjectedSeq) })
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: thread.ThreadID,
		SourceID: source.ID, SourceGeneration: source.Generation, Provider: ProviderCodex,
		ParserVersion: "window-test", CompletionRuleVersion: "window-test", Result: result}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: thread.ThreadID, Limit: 100})
	if err != nil || len(page.Items) != 7 || len(page.Turns) != 2 {
		t.Fatalf("retained window items=%d turns=%d err=%v", len(page.Items), len(page.Turns), err)
	}
	if page.Turns[0].Key != "outer" || page.Turns[1].Key != "inner" {
		t.Fatalf("overlapping turn windows borrowed another turn's semantic item: %+v", page.Turns)
	}
	if _, err := os.Stat(metadata.Source.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read must use retained rows without an evidence source: %v", err)
	}
}

func TestTranscriptPinnedSnapshotClampsTurnAfterSameTurnAppend(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(t.TempDir(), "projects")}
	metadata := testSource(rootSpec, "claude.jsonl", "claude-pinned", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("source = %#v, %v, %v", stored, ok, err)
	}
	adapter := newTranscriptFixture(t, ProviderClaude)
	initial := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-13T21:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Inspect"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-13T21:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Initial answer"}],"stop_reason":"end_turn"}}`,
	}, "\n") + "\n"
	first, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{Source: metadata.Source, Reader: strings.NewReader(initial)})
	if err != nil {
		t.Fatal(err)
	}
	written, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderClaude,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(), Result: first,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, ok, err := store.LoadTranscriptCheckpoint(ctx, applied.ThreadID)
	if err != nil || !ok {
		t.Fatalf("checkpoint = %#v, %v, %v", checkpoint, ok, err)
	}
	appendJSON := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-13T21:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-a","content":"continued"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-13T21:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"Revised answer"}],"stop_reason":"end_turn"}}`,
		`{"type":"system","timestamp":"2026-07-13T21:00:04Z","subtype":"turn_duration","durationMs":4000}`,
	}, "\n") + "\n"
	appendResult, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
		Source: metadata.Source, Reader: strings.NewReader(appendJSON), ProjectionKey: checkpoint.ProjectionKey,
		StartOffset: checkpoint.CompleteOffset, PriorCompleteLines: checkpoint.CompleteLines,
		PriorProjectedSeq: checkpoint.LatestSeq, PreviousState: checkpoint.ParserState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
		Mode: TranscriptAppend, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: ProviderClaude,
		ParserVersion: adapter.ParserVersion(), CompletionRuleVersion: adapter.CompletionRuleVersion(),
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: appendResult,
	}); err != nil {
		t.Fatal(err)
	}
	pinned, err := store.ReadTranscript(ctx, TranscriptReadRequest{
		ThreadID: applied.ThreadID, ExpectedProjectionKey: written.ProjectionKey,
		SnapshotHeadSeq: written.LatestSeq, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.Turns) != 1 {
		t.Fatalf("pinned turns = %#v", pinned.Turns)
	}
	turn := pinned.Turns[0]
	if turn.EndSeq != written.LatestSeq || turn.Status != TurnInProgress || turn.CompletionEvidence != CompletionNone ||
		turn.FinalItemID != "" || turn.CompletedAt != nil || turn.DurationMS != 0 {
		t.Fatalf("future turn state leaked into pinned snapshot: %#v", turn)
	}
	for _, item := range pinned.Items {
		if item.IsFinalAnswer {
			t.Fatalf("future finality leaked into pinned snapshot: %#v", item)
		}
	}
}

func readTranscriptFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "transcript", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func transcriptCandidate(t *testing.T, provider Provider) SourceCandidate {
	t.Helper()
	rootKind := RootCodexSessions
	if provider == ProviderClaude {
		rootKind = RootClaudeProjects
	}
	root := filepath.Join(t.TempDir(), "root")
	return SourceCandidate{Root: SourceRoot{Provider: provider, Kind: rootKind, Path: root}, Path: filepath.Join(root, "fixture.jsonl"), Kind: ThreadMain}
}

func assertDenseTranscript(t *testing.T, items []TranscriptItem, prior int64) {
	t.Helper()
	for index, item := range items {
		if item.ProjectedSeq != prior+int64(index)+1 {
			t.Fatalf("sequence[%d] = %d", index, item.ProjectedSeq)
		}
		if len(item.SourceLineSHA256) != sha256.Size*2 {
			t.Fatalf("item[%d] source hash = %q", index, item.SourceLineSHA256)
		}
	}
}

func mustRootID(t *testing.T, store *Store, root SourceRoot) int64 {
	t.Helper()
	persisted, err := store.EnsureRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return persisted.ID
}

func codexTurnsJSONL(first, last int) string {
	var builder strings.Builder
	for turn := first; turn <= last; turn++ {
		builder.WriteString(`{"timestamp":"2026-07-13T20:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"Prompt ` + string(rune('0'+turn)) + `"}}` + "\n")
		builder.WriteString(`{"timestamp":"2026-07-13T20:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Answer ` + string(rune('0'+turn)) + `"}]}}` + "\n")
		builder.WriteString(`{"timestamp":"2026-07-13T20:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n")
	}
	return builder.String()
}

func codexLargeTurnJSONL() string {
	largeText, _ := json.Marshal(strings.Repeat("retained body ", 900))
	return `{"timestamp":"2026-07-13T20:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"Prompt"}}` + "\n" +
		`{"timestamp":"2026-07-13T20:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":` + string(largeText) + `}]}}` + "\n" +
		`{"timestamp":"2026-07-13T20:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}` + "\n"
}

// Every rebuild mints a projection generation and supersedes the previous one.
// Collection is deliberately separate from ApplyTranscript so a rebuild never
// waits for historical cleanup. The bounded collector removes every
// superseded generation after the active replacement has committed.
func TestApplyTranscriptDefersOldSupersededGenerationCollection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(t.TempDir(), "sessions")}
	metadata := testSource(rootSpec, "thread.jsonl", "native-gc", ThreadMain, nil)
	metadata.Source.Size = 4096
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustRootID(t, store, rootSpec), metadata.Source.Path)
	if err != nil || !ok {
		t.Fatalf("LoadSource = %v, %v", ok, err)
	}

	adapter := newTranscriptFixture(t, ProviderCodex)
	body := []byte(codexTurnsJSONL(1, 2))
	// A parser-version change is what forces a fresh generation rather than an
	// append — the same trigger that produced the real accumulation.
	for _, parserVersion := range []string{"gc-parser-v1", "gc-parser-v2", "gc-parser-v3", "gc-parser-v4"} {
		projected, err := adapter.ProjectTranscript(ctx, TranscriptProjectionRequest{
			Source: metadata.Source, Reader: bytes.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
			Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
			SourceGeneration: stored.Generation, Provider: ProviderCodex,
			ParserVersion: parserVersion, CompletionRuleVersion: adapter.CompletionRuleVersion(),
			Result: projected,
		}); err != nil {
			t.Fatalf("rebuild %s: %v", parserVersion, err)
		}
	}
	var before int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_projections WHERE thread_id=?`, applied.ThreadID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 4 {
		t.Fatalf("ApplyTranscript collected inline: retained %d generations, want all 4 before sweep", before)
	}
	for {
		batch, err := store.CollectSupersededProjectionsBatch(ctx, 8)
		if err != nil {
			t.Fatal(err)
		}
		if !batch.Remaining {
			break
		}
	}

	type generation struct {
		id     int64
		state  string
		parser string
		items  int
	}
	rows, err := store.db.QueryContext(ctx, `
SELECT p.id, p.state, p.parser_version, (SELECT count(*) FROM conversation_items i WHERE i.projection_id = p.id)
FROM conversation_projections p WHERE p.thread_id = ? ORDER BY p.id`, applied.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var generations []generation
	for rows.Next() {
		var g generation
		if err := rows.Scan(&g.id, &g.state, &g.parser, &g.items); err != nil {
			t.Fatal(err)
		}
		generations = append(generations, g)
	}
	if len(generations) != 1 {
		t.Fatalf("after 4 rebuilds: %d generations retained, want active only: %#v", len(generations), generations)
	}
	if generations[0].state != "active" || generations[0].parser != "gc-parser-v4" {
		t.Errorf("live generation = %#v, want active gc-parser-v4", generations[0])
	}

	// Collected generations leave nothing behind in any child table.
	for _, table := range []string{"conversation_items", "conversation_turns", "conversation_item_bodies"} {
		var orphans int
		if err := store.db.QueryRowContext(ctx, fmt.Sprintf(`
SELECT count(*) FROM %s c
WHERE c.projection_id NOT IN (SELECT id FROM conversation_projections)`, table)).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if orphans != 0 {
			t.Errorf("%s: %d rows orphaned by collection, want 0", table, orphans)
		}
	}
}

// TestEnhancedPagesHoldOnlyUserAndAgentMessages catches an Enhanced page that
// skips a message, repeats one across a page boundary, or shows a hidden row.
// Boundary: ReadTranscript paging backward through a projection whose
// messages are sparse among tool, reasoning and error rows, and an append to
// its running turn. Enhanced (MessagesOnly) is user and agent messages, and
// questions and answers, in the main thread, whatever their status; a
// subagent's message and errors are not messages. Category profiles,
// including messages-only, keep classifying by category.
func TestEnhancedPagesHoldOnlyUserAndAgentMessages(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	metadata := testSource(root, "thread.jsonl", "enhanced-sparse", ThreadMain, nil)
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	var items []TranscriptItem
	add := func(turn string, ordinal int, itemType TranscriptItemType, status TranscriptItemStatus, sidechain bool) {
		seq := int64(len(items) + 1)
		id := fmt.Sprintf("item-%d", seq)
		items = append(items, TranscriptItem{
			ID: id, ProjectedSeq: seq, SourceKey: "source-" + id, TurnKey: turn, TurnOrdinal: ordinal,
			Type: itemType, Status: status, Role: RoleAssistant, Sidechain: sidechain,
			SourceLineSHA256: strings.Repeat("a", 64),
		})
	}
	// Turn 1, seqs 1-6: a subagent's message (5) between the prompt and answer.
	add("t1", 1, ItemUserMessage, ItemStatusCompleted, false)
	add("t1", 1, ItemReasoning, ItemStatusCompleted, false)
	add("t1", 1, ItemToolCall, ItemStatusCompleted, false)
	add("t1", 1, ItemToolResult, ItemStatusCompleted, false)
	add("t1", 1, ItemAgentMessage, ItemStatusCompleted, true)
	add("t1", 1, ItemAgentMessage, ItemStatusCompleted, false)
	// Turn 2, seqs 7-30, interrupted without an answer: hidden rows, a failed
	// tool result (27) and an error notice (28), both errors, an interrupted
	// agent message (29) and the agent's question (30).
	add("t2", 2, ItemUserMessage, ItemStatusCompleted, false)
	for range 9 {
		add("t2", 2, ItemReasoning, ItemStatusCompleted, false)
		add("t2", 2, ItemCommand, ItemStatusCompleted, false)
	}
	add("t2", 2, ItemReasoning, ItemStatusCompleted, false)
	add("t2", 2, ItemToolResult, ItemStatusFailed, false)
	add("t2", 2, ItemNotice, ItemStatusFailed, false)
	add("t2", 2, ItemAgentMessage, ItemStatusInterrupted, false)
	add("t2", 2, ItemInteraction, ItemStatusCompleted, false)
	// Turn 3, seqs 31-36, still running: a failed tool result the agent is
	// still working through (34) and its message so far (35).
	add("t3", 3, ItemUserMessage, ItemStatusCompleted, false)
	add("t3", 3, ItemToolCall, ItemStatusCompleted, false)
	add("t3", 3, ItemToolCall, ItemStatusCompleted, false)
	add("t3", 3, ItemToolResult, ItemStatusFailed, false)
	add("t3", 3, ItemAgentMessage, ItemStatusInProgress, false)
	add("t3", 3, ItemReasoning, ItemStatusCompleted, false)
	turns := func(end int64) []TranscriptTurn {
		return []TranscriptTurn{
			{Key: "t1", Ordinal: 1, StartSeq: 1, EndSeq: 6, CompletionSeq: 6, Status: TurnCompleted, CompletionEvidence: CompletionTaskComplete, FinalItemID: "item-6"},
			{Key: "t2", Ordinal: 2, StartSeq: 7, EndSeq: 30, CompletionSeq: 30, Status: TurnInterrupted, CompletionEvidence: CompletionTurnAborted},
			{Key: "t3", Ordinal: 3, StartSeq: 31, EndSeq: end, Status: TurnUnknown, CompletionEvidence: CompletionNone},
		}
	}
	request := TranscriptApplyRequest{
		Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: ProviderClaude, ParserVersion: "enhanced-test", CompletionRuleVersion: "enhanced-test",
		Result: TranscriptProjectionResult{
			ProjectionKey: "enhanced-sparse", CompleteOffset: 1, CompleteLines: 1, ParserState: json.RawMessage(`{}`),
			Items: items, Turns: turns(36),
		},
		IndexedAt: time.Now().UTC(),
	}
	if _, err := store.ApplyTranscript(ctx, request); err != nil {
		t.Fatal(err)
	}
	// pages reads a profile back to the start: Enhanced (MessagesOnly) when
	// hidden is nil, otherwise a category filter.
	pages := func(hidden []TranscriptCategory, limit int, head int64) [][]int64 {
		t.Helper()
		var out [][]int64
		var before int64
		for {
			page, err := store.ReadTranscript(ctx, TranscriptReadRequest{
				ThreadID: applied.ThreadID, Limit: limit, MessagesOnly: hidden == nil,
				HiddenCategories: hidden, SnapshotHeadSeq: head, BeforeSeq: before,
			})
			if err != nil {
				t.Fatal(err)
			}
			head = page.SnapshotHeadSeq
			seqs := []int64{}
			for _, item := range page.Items {
				seqs = append(seqs, item.ProjectedSeq)
				if item.ProjectedSeq == 29 && item.Category != CategoryError {
					t.Fatalf("interrupted message category = %q, want error", item.Category)
				}
			}
			out = append(out, seqs)
			if !page.HasMoreBefore {
				return out
			}
			before = page.StartSeq
		}
	}
	// The second page keeps its turn whole: message 7 joins 29 and 30.
	if got, want := pages(nil, 2, 0), [][]int64{{31, 35}, {7, 29, 30}, {1, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Enhanced pages = %v, want %v", got, want)
	}
	// Hiding every category but messages is a category filter: the
	// interrupted message is an error there.
	allButMessages := []TranscriptCategory{
		CategoryReasoning, CategoryToolCall, CategoryToolResult, CategorySubagent,
		CategorySystem, CategoryError, CategoryUnclassified,
	}
	if got, want := pages(allButMessages, 40, 0), [][]int64{{1, 6, 7, 30, 31, 35}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("messages-category pages = %v, want %v", got, want)
	}
	// The former Enhanced profile is now a custom one and keeps errors and
	// subagent activity.
	former := []TranscriptCategory{CategoryReasoning, CategoryToolCall, CategoryToolResult, CategorySystem, CategoryUnclassified}
	if got, want := pages(former, 40, 0), [][]int64{{1, 5, 6, 7, 27, 28, 29, 30, 31, 35}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("custom profile pages = %v, want %v", got, want)
	}
	if got := pages([]TranscriptCategory{}, 100, 0); len(got) != 1 || len(got[0]) != 36 {
		t.Fatalf("All pages = %v, want all 36 rows", got)
	}
	if _, err := store.ReadTranscript(ctx, TranscriptReadRequest{
		ThreadID: applied.ThreadID, MessagesOnly: true, HiddenCategories: []TranscriptCategory{CategoryReasoning},
	}); !errors.Is(err, ErrInvalidTranscriptPage) {
		t.Fatalf("MessagesOnly with categories = %v, want invalid page", err)
	}

	// The running turn saves the agent's next message and a question.
	appended := request
	appended.Mode = TranscriptAppend
	items = nil
	for range 36 {
		items = append(items, TranscriptItem{})
	}
	add("t3", 3, ItemToolResult, ItemStatusCompleted, false)
	add("t3", 3, ItemAgentMessage, ItemStatusCompleted, false)
	add("t3", 3, ItemInteraction, ItemStatusInProgress, false)
	appended.Result.Items = items[36:]
	appended.Result.Turns = turns(39)[2:]
	appended.Result.CompleteOffset, appended.Result.CompleteLines = 2, 2
	if _, err := store.ApplyTranscript(ctx, appended); err != nil {
		t.Fatal(err)
	}
	if got, want := pages(nil, 2, 36), [][]int64{{31, 35}, {7, 29, 30}, {1, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pages pinned before the append = %v, want %v", got, want)
	}
	if got, want := pages(nil, 2, 0), [][]int64{{31, 35, 38, 39}, {7, 29, 30}, {1, 6}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pages after the append = %v, want %v", got, want)
	}
}
