package history

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// grokSessionFixture writes a synthetic Grok session directory shaped
// <root>/<encoded-cwd>/<session-id>/summary.json (+ optional updates.jsonl)
// and returns the SourceCandidate discovery would have produced for it:
// updates.jsonl when present, otherwise the metadata-only summary.json.
func grokSessionFixture(t *testing.T, encodedCWD, sessionDir, summary string, updates *string) SourceCandidate {
	t.Helper()
	root := filepath.Join(t.TempDir(), "sessions")
	sessionPath := filepath.Join(root, encodedCWD, sessionDir)
	if err := os.MkdirAll(sessionPath, 0o755); err != nil {
		t.Fatal(err)
	}
	summaryPath := filepath.Join(sessionPath, "summary.json")
	if err := os.WriteFile(summaryPath, []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
	if updates != nil {
		if err := os.WriteFile(filepath.Join(sessionPath, "updates.jsonl"), []byte(*updates), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sourcePath := summaryPath
	if updates != nil {
		sourcePath = filepath.Join(sessionPath, "updates.jsonl")
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	return SourceCandidate{
		Root: SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: root},
		Path: sourcePath, Size: info.Size(), ModTime: info.ModTime(), Kind: ThreadMain,
	}
}

// grokCurrentSummary matches the verified 0.2.118 shape: only id and cwd
// nest under "info"; every other field is a top-level snake_case key.
const grokCurrentSummary = `{
  "info": {"id": "5f3a2b10-1111-4aaa-8bbb-000000000001", "cwd": "/home/user/example-project"},
  "created_at": "2026-08-01T12:00:00Z",
  "updated_at": "2026-08-01T12:14:32Z",
  "last_active_at": "2026-08-01T12:14:30Z",
  "current_model_id": "grok-4-fast",
  "reasoning_effort": "high",
  "sandbox_profile": "off",
  "generated_title": "Fix flaky retry test",
  "session_summary": "Added bounded retry backoff to the flaky test."
}`

const grokUpdatesFixture = `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"5f3a2b10-1111-4aaa-8bbb-000000000001","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Fix the flaky retry test"}},"_meta":{"eventId":"e1","agentTimestampMs":1893456000123}}}
{"timestamp":1893456001,"method":"session/update","params":{"sessionId":"5f3a2b10-1111-4aaa-8bbb-000000000001","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Let me look at the test file."}},"_meta":{"eventId":"e2","promptId":"p1","agentTimestampMs":1893456001000}}}
{"timestamp":1893456002,"method":"session/update","params":{"sessionId":"5f3a2b10-1111-4aaa-8bbb-000000000001","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"run_shell","_meta":{"x.ai/tool":{"name":"run_shell","kind":"execute"}}},"_meta":{"eventId":"e3","promptId":"p1","updateParams":{"toolCallId":"call-1","status":"Pending"}}}}
{"timestamp":1893456003,"method":"session/update","params":{"sessionId":"5f3a2b10-1111-4aaa-8bbb-000000000001","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"3 passed"}}],"_meta":{"x.ai/tool":{"name":"run_shell","kind":"execute"}}},"_meta":{"eventId":"e4","promptId":"p1"}}}
{"timestamp":1893456004,"method":"session/update","params":{"sessionId":"5f3a2b10-1111-4aaa-8bbb-000000000001","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Fixed the flaky test by adding a retry backoff."}},"_meta":{"eventId":"e5","promptId":"p1","agentTimestampMs":1893456004000}}}
{"timestamp":1893456005,"method":"_x.ai/session/update","params":{"sessionId":"5f3a2b10-1111-4aaa-8bbb-000000000001","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":120,"outputTokens":42,"totalTokens":162,"cachedReadTokens":30,"reasoningTokens":15}},"_meta":{"eventId":"e6"}}}
`

func TestGrokMetadataProjection_CurrentSession(t *testing.T) {
	updates := grokUpdatesFixture
	source := grokSessionFixture(t, "-home-user-example-project", "5f3a2b10-1111-4aaa-8bbb-000000000001", grokCurrentSummary, &updates)

	result := projectFixture(t, (metadataFixtureReader{ProviderGrok}), source)
	projection := result.Projection

	if projection.NativeThreadID != "5f3a2b10-1111-4aaa-8bbb-000000000001" || projection.IdentityEvidence != EvidenceNativeDurable {
		t.Fatalf("identity = %q (%q), want native durable summary.info.id", projection.NativeThreadID, projection.IdentityEvidence)
	}
	if projection.Kind != ThreadMain {
		t.Fatalf("kind = %q, want main", projection.Kind)
	}
	assertKnownField(t, "cwd", projection.Metadata.CWD, "/home/user/example-project")
	assertKnownField(t, "model", projection.Metadata.Model, "grok-4-fast")
	assertKnownField(t, "effort", projection.Metadata.Effort, "high")
	assertKnownField(t, "permission", projection.Metadata.Permission, "off")
	if projection.Title == nil || *projection.Title != "Fix flaky retry test" {
		t.Fatalf("title = %#v", projection.Title)
	}
	if projection.Preview == nil || *projection.Preview != "Added bounded retry backoff to the flaky test." {
		t.Fatalf("preview = %#v", projection.Preview)
	}
	if projection.CreatedAt == nil || projection.CreatedAt.Format("15:04") != "12:00" {
		t.Fatalf("createdAt = %v", projection.CreatedAt)
	}
	if projection.UpdatedAt == nil || projection.UpdatedAt.Format("15:04") != "12:14" {
		t.Fatalf("updatedAt = %v", projection.UpdatedAt)
	}
	// Fields Grok's summary carries no evidence for stay honestly unknown
	// rather than guessed.
	for name, field := range map[string]MetadataField{
		"priority": projection.Metadata.Priority, "branch": projection.Metadata.HistoricalBranch,
		"origin": projection.Metadata.Origin,
	} {
		if field.State != MetadataUnknown {
			t.Fatalf("%s = %#v, want unknown", name, field)
		}
	}
	if ClassifyInteraction(projection.Provider, projection.Metadata.Origin) != InteractionUnclassified {
		t.Fatalf("interaction should be unclassified without a recorded origin")
	}
	if len(result.Diagnostics) != 0 || result.Summary.Total != 0 {
		t.Fatalf("unexpected diagnostics: %#v", result)
	}
	if result.CompleteOffset != source.Size {
		t.Fatalf("metadata checkpoint = %d, want discovered updates size %d", result.CompleteOffset, source.Size)
	}
}

func TestGrokMetadataProjection_MetadataOnlyOldSessionFallsBackToDirectoryID(t *testing.T) {
	oldSummary := `{"info":{"cwd":"/home/user/legacy-project"},"current_model_id":"grok-3"}`
	source := grokSessionFixture(t, "-home-user-legacy-project", "22222222-2222-4222-8222-222222222222", oldSummary, nil)

	result := projectFixture(t, (metadataFixtureReader{ProviderGrok}), source)
	projection := result.Projection

	if projection.NativeThreadID != "22222222-2222-4222-8222-222222222222" || projection.IdentityEvidence != EvidenceFilenameFallback {
		t.Fatalf("identity = %q (%q), want directory-name fallback", projection.NativeThreadID, projection.IdentityEvidence)
	}
	assertKnownField(t, "cwd", projection.Metadata.CWD, "/home/user/legacy-project")
	assertKnownField(t, "model", projection.Metadata.Model, "grok-3")
	if projection.Title != nil {
		t.Fatalf("title = %#v, want none from a titleless summary", projection.Title)
	}
	if len(result.Diagnostics) != 0 {
		t.Fatalf("an old session missing updates.jsonl must not be a metadata diagnostic: %#v", result.Diagnostics)
	}
}

func TestGrokMetadataProjectionPreservesLastGoodLineageDuringSidecarGap(t *testing.T) {
	source := grokSessionFixture(t, "-home-user-example-project", "child-1", grokCurrentSummary, nil)
	previous := ThreadProjection{Provider: ProviderGrok, Kind: ThreadSubagent, ParentNativeID: "parent-1"}
	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := (metadataFixtureReader{ProviderGrok}).Project(context.Background(), ProjectionRequest{Source: source, Reader: file, Previous: &previous})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.Kind != ThreadSubagent || result.Projection.ParentNativeID != "parent-1" {
		t.Fatalf("lineage was lost during sidecar gap: %#v", result.Projection)
	}
}

func TestGrokMetadataProjection_RejectsAppendOffset(t *testing.T) {
	source := grokSessionFixture(t, "-home-user-example-project", "5f3a2b10-1111-4aaa-8bbb-000000000001", grokCurrentSummary, nil)
	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_, err = (metadataFixtureReader{ProviderGrok}).Project(context.Background(), ProjectionRequest{Source: source, Reader: file, StartOffset: 4})
	if err == nil {
		t.Fatal("expected an error resuming a partial read of a whole-document summary")
	}
}

func TestGrokTranscriptProjection_CurrentSummaryAndUpdates(t *testing.T) {
	updates := grokUpdatesFixture
	source := grokSessionFixture(t, "-home-user-example-project", "5f3a2b10-1111-4aaa-8bbb-000000000001", grokCurrentSummary, &updates)

	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: file,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDenseTranscript(t, result.Items, 0)
	if len(result.Items) != 5 {
		t.Fatalf("items = %d, want 5: %#v", len(result.Items), result.Items)
	}
	wantTypes := []TranscriptItemType{
		ItemUserMessage, ItemReasoning, ItemToolCall, ItemToolResult, ItemAgentMessage,
	}
	for index, item := range result.Items {
		if item.Type != wantTypes[index] {
			t.Fatalf("item[%d].Type = %q, want %q (%#v)", index, item.Type, wantTypes[index], item)
		}
	}
	if result.Items[2].CallID != "call-1" || result.Items[3].CallID != "call-1" {
		t.Fatalf("tool call/result must share a CallID: %#v / %#v", result.Items[2], result.Items[3])
	}
	seenNative := map[string]bool{}
	for _, item := range result.Items {
		if item.NativeItemID == "" {
			continue
		}
		if seenNative[item.NativeItemID] {
			t.Fatalf("duplicate Grok native item id %q: %#v", item.NativeItemID, result.Items)
		}
		seenNative[item.NativeItemID] = true
	}
	if result.Items[3].Role != RoleTool || result.Items[3].Status != ItemStatusCompleted {
		t.Fatalf("completed tool_call_update = %#v, want a completed tool role result", result.Items[3])
	}

	if len(result.Turns) != 1 {
		t.Fatalf("turns = %d, want 1: %#v", len(result.Turns), result.Turns)
	}
	turn := result.Turns[0]
	if turn.Status != TurnCompleted || turn.CompletionEvidence != CompletionStopReason {
		t.Fatalf("turn completion = %#v; items = %#v", turn, result.Items)
	}
	usage := turn.Usage
	if usage == nil || usage.Input == nil || *usage.Input != 120 || usage.Output == nil || *usage.Output != 42 ||
		usage.Total == nil || *usage.Total != 162 || usage.Cached == nil || *usage.Cached != 30 ||
		usage.Reasoning == nil || *usage.Reasoning != 15 {
		t.Fatalf("turn usage = %#v", usage)
	}
	if turn.Model != "grok-4-fast" || turn.Effort != "high" {
		t.Fatalf("session snapshot = %q / %q", turn.Model, turn.Effort)
	}
	agentMessage := result.Items[4]
	if !agentMessage.IsFinalAnswer || turn.FinalItemID != agentMessage.ID {
		t.Fatalf("final answer not attributed to the agent message: turn=%#v item=%#v", turn, agentMessage)
	}
}

func TestGrokTranscriptProjection_ToleratesMissingUpdatesFile(t *testing.T) {
	source := grokSessionFixture(t, "-home-user-legacy-project", "22222222-2222-4222-8222-222222222222", grokCurrentSummary, nil)

	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
		Source: source, Reader: file,
	})
	if err != nil {
		t.Fatalf("a session with no updates.jsonl yet must project as empty, not fail: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Type != ItemProviderEvent || len(result.Turns) != 0 {
		t.Fatalf("expected retained summary metadata without conversation turns, got %d items and %d turns", len(result.Items), len(result.Turns))
	}
	if result.CompleteOffset != source.Size {
		t.Fatalf("expected a source-aligned checkpoint, got offset %d lines %d", result.CompleteOffset, result.CompleteLines)
	}
	if result.ProjectionKey == "" {
		t.Fatal("expected a minted projection key even for an empty transcript")
	}
}

func TestGrokTranscriptProjectionKeepsOnlyCompactSubagentActivities(t *testing.T) {
	updates := `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"visible"}},"_meta":{"eventId":"user-1"}}}
{"timestamp":1893456001,"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hidden"},"_meta":{"hideFromScrollback":true}},"_meta":{"eventId":"hidden-1"}}}
{"timestamp":1893456002,"method":"_x.ai/session_notification","params":{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_spawned","child_session_id":"child-1","parent_session_id":"parent-1","description":"check tests"},"_meta":{"eventId":"spawn-1"}}}
{"timestamp":1893456003,"method":"_x.ai/session_notification","params":{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_finished","child_session_id":"child-1","parent_session_id":"parent-1","status":"completed","duration_ms":12,"tool_calls":2},"_meta":{"eventId":"finish-1"}}}` + "\n"
	source := grokSessionFixture(t, "-home-user-example", "parent-1", grokCurrentSummary, &updates)
	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{Source: source, Reader: file})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 4 {
		t.Fatalf("items = %#v", result.Items)
	}
	if result.Items[0].Text != "visible" || result.Items[1].Type != ItemProviderEvent || result.Items[2].Type != ItemNotice || result.Items[3].Type != ItemNotice {
		t.Fatalf("unexpected transcript items: %#v", result.Items)
	}
	if bytes.Contains(result.Items[2].Detail, []byte("output")) || bytes.Contains(result.Items[3].Detail, []byte("output")) {
		t.Fatalf("subagent card leaked child output: %#v", result.Items)
	}
}

func TestGrokTranscriptSyntheticContinuationCannotClaimOperatorTurn(t *testing.T) {
	updates := `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"operator request"}},"_meta":{"eventId":"user-1"}}}
{"timestamp":1893456001,"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"child finished"}},"_meta":{"eventId":"synthetic-1","promptId":"subagent-completed-child-1"}}}
{"timestamp":1893456002,"method":"_x.ai/session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"turn_completed","prompt_id":"subagent-completed-child-1","stop_reason":"end_turn"},"_meta":{"eventId":"synthetic-terminal"}}}
{"timestamp":1893456003,"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"operator answer"}},"_meta":{"eventId":"answer-1","promptId":"operator-prompt"}}}
{"timestamp":1893456004,"method":"_x.ai/session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"turn_completed","prompt_id":"operator-prompt","stop_reason":"end_turn"},"_meta":{"eventId":"operator-terminal"}}}
`
	source := grokSessionFixture(t, "-home-user-example", "parent-1", grokCurrentSummary, &updates)
	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{Source: source, Reader: file})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Turns) != 2 || result.Turns[1].NativeTurnID != "subagent-completed-child-1" || result.Turns[0].NativeTurnID != "operator-prompt" || result.Turns[0].Status != TurnCompleted {
		t.Fatalf("synthetic continuation stole or closed operator turn: %#v", result.Turns)
	}
	if result.Items[len(result.Items)-1].Text != "operator answer" || !result.Items[len(result.Items)-1].IsFinalAnswer {
		t.Fatalf("operator terminal was not retained: %#v", result.Items)
	}
}

// TestGrokTranscriptProjection_TerminalStopReasonMapping locks in the
// integrator-specified mapping: end_turn completes the turn, a cancellation
// is an interrupted turn, and unknown reasons keep an unknown outcome.
// GROK-END-1 keeps the captured terminal record separate from turn outcome.
func TestGrokTranscriptProjection_TerminalStopReasonMapping(t *testing.T) {
	cases := []struct {
		name           string
		stopReason     string
		wantTurnStatus TranscriptTurnStatus
		wantEvidence   TranscriptCompletionEvidence
		wantItemStatus TranscriptItemStatus
	}{
		{"end_turn_completes", "end_turn", TurnCompleted, CompletionStopReason, ItemStatusCompleted},
		{"cancelled_interrupts", "cancelled", TurnInterrupted, CompletionTurnAborted, ItemStatusCompleted},
		{"other_nonempty_reason_fails", "content_filter", TurnUnknown, CompletionStopReason, ItemStatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updates := fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"go"}},"_meta":{"eventId":"e1"}}}
{"method":"_x.ai/session/update","params":{"sessionId":"s","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":%q},"_meta":{"eventId":"e2"}}}
`, tc.stopReason)
			source := grokSessionFixture(t, "-home-user-example", "session-"+tc.name, grokCurrentSummary, &updates)

			file, err := os.Open(source.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			result, err := newTranscriptFixture(t, ProviderGrok).ProjectTranscript(context.Background(), TranscriptProjectionRequest{
				Source: source, Reader: file,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Turns) != 1 {
				t.Fatalf("turns = %d, want 1: %#v", len(result.Turns), result.Turns)
			}
			if result.Turns[0].Status != tc.wantTurnStatus || result.Turns[0].CompletionEvidence != tc.wantEvidence {
				t.Fatalf("turn = %#v, want status %q evidence %q", result.Turns[0], tc.wantTurnStatus, tc.wantEvidence)
			}
			notice := result.Items[len(result.Items)-1]
			if notice.Status != tc.wantItemStatus {
				t.Fatalf("terminal notice status = %q, want %q (%#v)", notice.Status, tc.wantItemStatus, notice)
			}
		})
	}
}
