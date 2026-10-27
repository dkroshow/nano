package history

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexMetadataProjection(t *testing.T) {
	path := filepath.Join("testdata", "codex", "rollout-2026-07-09T12-00-00-11111111-1111-4111-8111-111111111111.jsonl")
	result := projectFixture(t, (metadataFixtureReader{ProviderCodex}), SourceCandidate{
		Root: SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions},
		Path: path, Kind: ThreadMain,
	})
	projection := result.Projection
	if projection.NativeThreadID != "11111111-1111-4111-8111-111111111111" || projection.IdentityEvidence != EvidenceNativeDurable {
		t.Fatalf("identity = %q (%q)", projection.NativeThreadID, projection.IdentityEvidence)
	}
	if projection.Kind != ThreadSubagent || projection.ParentNativeID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("subagent linkage = kind %q parent %q", projection.Kind, projection.ParentNativeID)
	}
	assertKnownField(t, "cwd", projection.Metadata.CWD, "/workspace/catalog-next")
	assertKnownField(t, "model", projection.Metadata.Model, "gpt-test")
	assertKnownField(t, "effort", projection.Metadata.Effort, "high")
	assertKnownField(t, "priority", projection.Metadata.Priority, "priority")
	assertKnownField(t, "permission", projection.Metadata.Permission, "on-request")
	assertKnownField(t, "branch", projection.Metadata.HistoricalBranch, "feature/catalog")
	assertKnownField(t, "origin", projection.Metadata.Origin, "codex_exec")
	if got := ClassifyInteraction(projection.Provider, projection.Metadata.Origin); got != InteractionProgrammatic {
		t.Fatalf("codex_exec interaction = %q, want programmatic", got)
	}
	if projection.Preview == nil || *projection.Preview != "Build the read-only thread catalog." {
		t.Fatalf("preview = %#v", projection.Preview)
	}
	if projection.ProviderVersion == nil || *projection.ProviderVersion != "0.150.0" {
		t.Fatalf("provider version = %#v", projection.ProviderVersion)
	}
	// Unknown and malformed rows cannot contribute trusted timestamps.
	if projection.CreatedAt == nil || projection.CreatedAt.Format("15:04") != "19:00" || projection.UpdatedAt == nil || projection.UpdatedAt.Format("15:04") != "19:03" {
		t.Fatalf("timestamps = created %v updated %v", projection.CreatedAt, projection.UpdatedAt)
	}
	if result.Summary.UnknownRows != 1 || result.Summary.Malformed != 1 || len(result.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %#v details %#v", result.Summary, result.Diagnostics)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.CompleteOffset != info.Size() || result.BytesRead != info.Size() {
		t.Fatalf("offset/read = %d/%d, want %d", result.CompleteOffset, result.BytesRead, info.Size())
	}
}

// The multi-agent collaboration rows codex started writing alongside its
// worker-orchestration feature: the transcript renderer already displays
// both (transcript_codex.go), but the metadata scanner did not recognize
// them and flagged every thread using the feature with a spurious
// "unknown_row" diagnostic (observed 2026-08-07 on five real worker threads).
func TestCodexCollaborationRowsAreNotUnknown(t *testing.T) {
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout.jsonl", Kind: ThreadMain}
	input := `{"type":"session_meta","payload":{"id":"collab-thread","cwd":"/workspace"}}` + "\n" +
		`{"type":"world_state","payload":{}}` + "\n" +
		`{"type":"inter_agent_communication_metadata","payload":{}}` + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.UnknownRows != 0 || len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v details %#v, want none", result.Summary, result.Diagnostics)
	}
}

func TestCodexGuardianSourceClassificationUsesStructuredNativeEvidence(t *testing.T) {
	for _, test := range []struct {
		name, fixture, wantParent string
	}{
		{"recent parented guardian", "guardian-recent.jsonl", "parent-thread"},
		{"older parentless guardian", "guardian-parentless.jsonl", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join("testdata", "codex", test.fixture)
			result := projectFixture(t, (metadataFixtureReader{ProviderCodex}), SourceCandidate{
				Root: SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions}, Path: path, Kind: ThreadMain,
			})
			if result.Projection.Kind != ThreadGuardian || result.Projection.ParentNativeID != test.wantParent {
				t.Fatalf("guardian projection = kind %q parent %q, want guardian/%q", result.Projection.Kind, result.Projection.ParentNativeID, test.wantParent)
			}
		})
	}
}

func TestCodexNonGuardianSubagentSourcesRemainSubagents(t *testing.T) {
	for _, source := range []string{
		`{"type":"subagent"}`,
		`{"subagent":{"other":"explorer"}}`,
		`{"subagent":{"other":""}}`,
	} {
		input := `{"type":"session_meta","payload":{"id":"ordinary-worker","source":` + source + `}}` + "\n"
		result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
			Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/worker.jsonl", Kind: ThreadMain},
			Reader: strings.NewReader(input),
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Projection.Kind != ThreadSubagent {
			t.Fatalf("source %s kind = %q, want subagent", source, result.Projection.Kind)
		}
	}
}

func TestCodexMetadataIgnoresInjectedResponseUserContent(t *testing.T) {
	input := strings.Join([]string{
		`{"timestamp":"2026-08-08T09:00:00Z","type":"session_meta","payload":{"id":"metadata-injected"}}`,
		`{"timestamp":"2026-08-08T09:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>\\nAvailable plugins\\n</recommended_plugins>"},{"type":"input_text","text":"# AGENTS.md instructions for /workspace\\nProvider-owned instruction block."},{"type":"input_text","text":"<environment_context>provider-owned context</environment_context>"}]}}`,
		`{"timestamp":"2026-08-08T09:02:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Real operator prompt"}]}}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/metadata-injected.jsonl", Kind: ThreadMain},
		Reader: strings.NewReader(input),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.Preview == nil || *result.Projection.Preview != "Real operator prompt" ||
		result.Projection.UpdatedAt == nil || result.Projection.UpdatedAt.Format("15:04") != "09:02" {
		t.Fatalf("metadata injected-content classification = %#v", result.Projection)
	}
}

func TestClaudeMetadataProjection(t *testing.T) {
	path := filepath.Join("testdata", "claude", "33333333-3333-4333-8333-333333333333.jsonl")
	result := projectFixture(t, (metadataFixtureReader{ProviderClaude}), SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: path, Kind: ThreadMain,
	})
	projection := result.Projection
	if projection.NativeThreadID != "33333333-3333-4333-8333-333333333333" || projection.IdentityEvidence != EvidenceNativeDurable {
		t.Fatalf("identity = %q (%q)", projection.NativeThreadID, projection.IdentityEvidence)
	}
	assertKnownField(t, "cwd", projection.Metadata.CWD, "/workspace/claude-next")
	assertKnownField(t, "model", projection.Metadata.Model, "claude-test")
	assertKnownField(t, "effort", projection.Metadata.Effort, "high")
	assertKnownField(t, "permission", projection.Metadata.Permission, "bypassPermissions")
	assertKnownField(t, "branch", projection.Metadata.HistoricalBranch, "feature/explorer")
	assertKnownField(t, "origin", projection.Metadata.Origin, "cli")
	if got := ClassifyInteraction(projection.Provider, projection.Metadata.Origin); got != InteractionInteractive {
		t.Fatalf("claude cli interaction = %q, want interactive", got)
	}
	if projection.Metadata.Priority.State != MetadataUnsupported || projection.Metadata.Priority.Value != nil {
		t.Fatalf("Claude priority = %#v, want unsupported", projection.Metadata.Priority)
	}
	if projection.Title == nil || *projection.Title != "Explorer design" {
		t.Fatalf("title = %#v", projection.Title)
	}
	if projection.Preview == nil || *projection.Preview != "Show the latest catalog state." {
		t.Fatalf("preview = %#v", projection.Preview)
	}
	if result.Summary.UnknownRows != 1 || result.Summary.Malformed != 1 {
		t.Fatalf("diagnostics = %#v", result.Summary)
	}
}

func TestClaudeSubagentIdentityDoesNotUseParentSession(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: "/sanitized/project/session/subagents/agent-worker-7.jsonl", Kind: ThreadSubagent,
	}
	input := `{"type":"assistant","sessionId":"parent-session","agentId":"worker-7","isSidechain":true,"cwd":"/workspace","message":{"role":"assistant","model":"claude-subagent","content":"done"}}` + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.NativeThreadID != "worker-7" || result.Projection.ParentNativeID != "parent-session" {
		t.Fatalf("subagent identity = native %q parent %q", result.Projection.NativeThreadID, result.Projection.ParentNativeID)
	}
	assertKnownField(t, "subagent model", result.Projection.Metadata.Model, "claude-subagent")
}

func TestClaudeSubagentRejectsAgentIDThatDisagreesWithPath(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: "/sanitized/project/session/subagents/agent-worker-7.jsonl", Kind: ThreadSubagent,
	}
	input := `{"type":"assistant","sessionId":"parent-session","agentId":"imposter","isSidechain":true,"timestamp":"2026-07-09T23:00:00Z","cwd":"/wrong","message":{"role":"assistant","model":"wrong-model","content":"done"}}` + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.NativeThreadID != "worker-7" || result.Projection.IdentityEvidence != EvidenceFilenameFallback {
		t.Fatalf("subagent fallback identity = %q (%q)", result.Projection.NativeThreadID, result.Projection.IdentityEvidence)
	}
	if result.Summary.IdentityConflicts != 1 || result.Projection.ParentNativeID != "" {
		t.Fatalf("mismatch diagnostics/linkage = %#v parent %q", result.Summary, result.Projection.ParentNativeID)
	}
	if result.Projection.Metadata.Model.State != MetadataUnknown || result.Projection.Metadata.CWD.State != MetadataUnknown || result.Projection.UpdatedAt != nil {
		t.Fatalf("rejected row contaminated projection: %#v", result.Projection)
	}
}

func TestClaudeMainProjectionIgnoresAllSidechainEvidence(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: "/sanitized/project/session-id.jsonl", Kind: ThreadMain,
	}
	input := strings.Join([]string{
		`{"type":"user","sessionId":"session-id","timestamp":"2026-07-09T20:00:00Z","cwd":"/main","message":{"role":"user","content":"Main prompt"}}`,
		`{"type":"assistant","sessionId":"session-id","timestamp":"2026-07-09T20:01:00Z","message":{"role":"assistant","model":"main-model","content":"Main answer"}}`,
		`{"type":"permission-mode","sessionId":"session-id","timestamp":"2026-07-09T20:02:00Z","permissionMode":"default"}`,
		`{"type":"user","sessionId":"other-session","isSidechain":true,"timestamp":"2026-07-09T21:00:00Z","cwd":"/sidechain","message":{"role":"user","content":"Sidechain prompt"}}`,
		`{"type":"assistant","sessionId":"other-session","isSidechain":true,"timestamp":"2026-07-09T21:01:00Z","message":{"role":"assistant","model":"sidechain-model","content":"Sidechain answer"}}`,
		`{"type":"permission-mode","sessionId":"other-session","isSidechain":true,"timestamp":"2026-07-09T21:02:00Z","permissionMode":"bypassPermissions"}`,
		`{"type":"custom-title","sessionId":"other-session","isSidechain":true,"timestamp":"2026-07-09T21:03:00Z","customTitle":"Sidechain title"}`,
		`{"type":"last-prompt","sessionId":"other-session","isSidechain":true,"timestamp":"2026-07-09T21:04:00Z","lastPrompt":"Sidechain latest prompt"}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	assertKnownField(t, "main cwd", result.Projection.Metadata.CWD, "/main")
	assertKnownField(t, "main model", result.Projection.Metadata.Model, "main-model")
	assertKnownField(t, "main permission", result.Projection.Metadata.Permission, "default")
	if result.Projection.Preview == nil || *result.Projection.Preview != "Main prompt" || result.Projection.Title != nil {
		t.Fatalf("sidechain changed title/preview: title %#v preview %#v", result.Projection.Title, result.Projection.Preview)
	}
	// 20:01 is the main thread's last message. The permission-mode row at 20:02
	// is bookkeeping and no longer advances UpdatedAt; the sidechain's own
	// answer at 21:01 must not touch it either.
	if result.Projection.UpdatedAt == nil || result.Projection.UpdatedAt.Format("15:04") != "20:01" {
		t.Fatalf("sidechain changed updated timestamp: %v", result.Projection.UpdatedAt)
	}
	if result.Summary.IdentityConflicts != 0 {
		t.Fatalf("sidechain identity affected main projection: %#v", result.Summary)
	}
}

func TestConflictingClaudeIdentityCannotContaminateWinnerMetadata(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: "/sanitized/project/winner.jsonl", Kind: ThreadMain,
	}
	input := strings.Join([]string{
		`{"type":"user","sessionId":"winner","timestamp":"2026-07-09T20:00:00Z","cwd":"/winner","message":{"role":"user","content":"Winner prompt"}}`,
		`{"type":"permission-mode","sessionId":"winner","timestamp":"2026-07-09T20:01:00Z","permissionMode":"default"}`,
		`{"type":"custom-title","sessionId":"winner","timestamp":"2026-07-09T20:02:00Z","customTitle":"Winner title"}`,
		`{"type":"custom-title","sessionId":"loser","timestamp":"2026-07-09T23:00:00Z","cwd":"/loser","permissionMode":"bypassPermissions","customTitle":"Loser title"}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.NativeThreadID != "winner" || result.Summary.IdentityConflicts != 1 {
		t.Fatalf("identity winner/conflict = %q %#v", result.Projection.NativeThreadID, result.Summary)
	}
	assertKnownField(t, "winner cwd", result.Projection.Metadata.CWD, "/winner")
	assertKnownField(t, "winner permission", result.Projection.Metadata.Permission, "default")
	// The winner's only message is its 20:00 prompt; its later title row is
	// bookkeeping, and the loser's 23:00 row must not leak in at all.
	if result.Projection.Title == nil || *result.Projection.Title != "Winner title" || result.Projection.UpdatedAt == nil || result.Projection.UpdatedAt.Format("15:04") != "20:00" {
		t.Fatalf("loser metadata contaminated winner: %#v", result.Projection)
	}
}

func TestFilenameFallbackAndMissingIdentity(t *testing.T) {
	withFallback, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout-2026-07-09-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl", Kind: ThreadMain},
		Reader: strings.NewReader(`{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if withFallback.Projection.NativeThreadID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || withFallback.Projection.IdentityEvidence != EvidenceFilenameFallback || withFallback.Summary.MissingIdentity != 0 {
		t.Fatalf("filename fallback result = %#v", withFallback)
	}

	missing, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout-no-id.jsonl", Kind: ThreadMain},
		Reader: strings.NewReader(`{"type":"event_msg","payload":{"type":"task_started"}}` + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Projection.NativeThreadID != "" || missing.Summary.MissingIdentity != 1 {
		t.Fatalf("missing identity result = %#v", missing)
	}
}

func TestProjectionCheckpointsCompleteLinesAndAppends(t *testing.T) {
	first := `{"timestamp":"2026-07-09T19:00:00Z","type":"session_meta","payload":{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","cwd":"/first"}}` + "\n"
	incomplete := `{"timestamp":"2026-07-09T19:01:00Z","type":"turn_context","payload":{"cwd":"/second","model":"gpt-next"}}`
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl", Kind: ThreadMain}

	initial, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(first + incomplete)})
	if err != nil {
		t.Fatal(err)
	}
	if initial.CompleteOffset != int64(len(first)) || initial.BytesRead != int64(len(first+incomplete)) {
		t.Fatalf("initial offsets = complete %d read %d", initial.CompleteOffset, initial.BytesRead)
	}
	assertKnownField(t, "initial cwd", initial.Projection.Metadata.CWD, "/first")
	if initial.Projection.Metadata.Model.State != MetadataUnknown {
		t.Fatalf("incomplete line changed model: %#v", initial.Projection.Metadata.Model)
	}

	appended, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: source, Reader: strings.NewReader(incomplete + "\n"), StartOffset: initial.CompleteOffset,
		PriorCompleteLines: initial.CompleteLines, Previous: &initial.Projection,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertKnownField(t, "appended cwd", appended.Projection.Metadata.CWD, "/second")
	assertKnownField(t, "appended model", appended.Projection.Metadata.Model, "gpt-next")
	if appended.CompleteOffset != int64(len(first+incomplete)+1) {
		t.Fatalf("append complete offset = %d", appended.CompleteOffset)
	}
}

func TestAppendDiagnosticsUseAbsoluteLineNumbers(t *testing.T) {
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl", Kind: ThreadMain}
	initialInput := `{"type":"event_msg","payload":{"type":"task_started"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"task_complete"}}` + "\n" +
		`{"type":"compacted","payload":{}}` + "\n"
	initial, err := ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source, Reader: strings.NewReader(initialInput), SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	checkpoint := TranscriptCheckpoint{Provider: ProviderCodex, ProjectionKey: initial.Transcript.ProjectionKey,
		ParserVersion: initial.ParserVersion, CompletionRuleVersion: initial.CompletionRuleVersion,
		CompleteOffset: initial.Transcript.CompleteOffset, ReadOffset: initial.Transcript.ReadOffset,
		CompleteLines: initial.Transcript.CompleteLines, ParserState: initial.Transcript.ParserState}
	parsed, err := ProjectParsedFile(context.Background(), ParsedFileRequest{
		Source: source, Reader: strings.NewReader("not-json\n"), Checkpoint: &checkpoint, SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	appended := parsed.Metadata
	if appended.CompleteLines != 4 || len(appended.Diagnostics) != 1 || appended.Diagnostics[0].Line != 4 {
		t.Fatalf("append diagnostics/line count = lines %d diagnostics %#v", appended.CompleteLines, appended.Diagnostics)
	}
}

func TestProjectionSkipsOversizedCompleteLineWithinMemoryBudget(t *testing.T) {
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl", Kind: ThreadMain}
	huge := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 2048) + `"}}` + "\n"
	metadata := `{"type":"turn_context","payload":{"model":"gpt-after-large-row"}}` + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: source, Reader: strings.NewReader(huge + metadata), MaxLineBytes: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.OversizedLines != 1 || result.Summary.Malformed != 0 || result.CompleteLines != 2 {
		t.Fatalf("oversized diagnostics = %#v, complete lines %d", result.Summary, result.CompleteLines)
	}
	if result.CompleteOffset != int64(len(huge+metadata)) || result.BytesRead != int64(len(huge+metadata)) {
		t.Fatalf("oversized checkpoint = offset %d read %d", result.CompleteOffset, result.BytesRead)
	}
	assertKnownField(t, "metadata after oversized row", result.Projection.Metadata.Model, "gpt-after-large-row")
}

func TestConflictingCodexIdentityCannotContaminateWinnerMetadata(t *testing.T) {
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout.jsonl", Kind: ThreadMain}
	input := `{"timestamp":"2026-07-09T20:00:00Z","type":"session_meta","payload":{"id":"winner","cwd":"/winner","title":"Winner","git":{"branch":"main"}}}` + "\n" +
		`{"timestamp":"2026-07-09T20:01:00Z","type":"turn_context","payload":{"model":"winner-model"}}` + "\n" +
		`{"timestamp":"2026-07-09T23:00:00Z","type":"session_meta","payload":{"id":"loser","cwd":"/loser","title":"Loser","git":{"branch":"contaminated"}}}` + "\n" +
		`{"timestamp":"2026-07-09T23:01:00Z","type":"turn_context","payload":{"model":"loser-model","cwd":"/loser-after-conflict"}}` + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.NativeThreadID != "winner" || result.Summary.IdentityConflicts != 1 {
		t.Fatalf("identity winner/conflict = %q %#v", result.Projection.NativeThreadID, result.Summary)
	}
	assertKnownField(t, "winner cwd", result.Projection.Metadata.CWD, "/winner")
	assertKnownField(t, "winner branch", result.Projection.Metadata.HistoricalBranch, "main")
	assertKnownField(t, "winner model", result.Projection.Metadata.Model, "winner-model")
	if result.Projection.Title == nil || *result.Projection.Title != "Winner" || result.Projection.UpdatedAt == nil || result.Projection.UpdatedAt.Format("15:04") != "20:01" {
		t.Fatalf("loser metadata contaminated winner: %#v", result.Projection)
	}
}

func TestCodexIdentityConflictQuarantineSurvivesAppendProjection(t *testing.T) {
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout.jsonl", Kind: ThreadMain}
	initialInput := `{"type":"session_meta","payload":{"id":"winner","cwd":"/winner"}}` + "\n" +
		`{"type":"turn_context","payload":{"model":"winner-model"}}` + "\n" +
		`{"type":"session_meta","payload":{"id":"loser","cwd":"/loser"}}` + "\n"
	initial, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: source, Reader: strings.NewReader(initialInput),
	})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: source,
		Reader: strings.NewReader(
			`{"type":"turn_context","payload":{"model":"loser-model","cwd":"/loser"}}` + "\n"),
		StartOffset: initial.CompleteOffset, PriorCompleteLines: initial.CompleteLines,
		PriorIdentityConflicted: initial.Summary.IdentityConflicts > 0, Previous: &initial.Projection,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertKnownField(t, "quarantined winner model", appended.Projection.Metadata.Model, "winner-model")
	assertKnownField(t, "quarantined winner cwd", appended.Projection.Metadata.CWD, "/winner")
}

func TestCodexSyntheticPromptsDoNotBecomePreview(t *testing.T) {
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl", Kind: ThreadMain}
	input := `{"type":"response_item","payload":{"type":"message","role":"user","content":"<environment_context>hidden</environment_context>"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"Actual operator request"}}` + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.Preview == nil || *result.Projection.Preview != "Actual operator request" {
		t.Fatalf("synthetic prompt became preview: %#v", result.Projection.Preview)
	}
}

func TestProjectionBoundsDiagnosticsAndPreservesFirstIdentityOnConflict(t *testing.T) {
	var input strings.Builder
	input.WriteString(`{"type":"session_meta","payload":{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}}` + "\n")
	for i := 0; i < 40; i++ {
		input.WriteString(`{"type":"future-row"}` + "\n")
	}
	input.WriteString(`{"type":"session_meta","payload":{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}}` + "\n")
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{
		Source: SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/history/rollout.jsonl", Kind: ThreadMain},
		Reader: strings.NewReader(input.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.NativeThreadID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || result.Summary.IdentityConflicts != 1 {
		t.Fatalf("identity conflict result = %#v", result)
	}
	if len(result.Diagnostics) != maxDiagnosticDetails || result.Summary.Dropped != 16 || result.Summary.Total != 41 {
		t.Fatalf("bounded diagnostics = len %d summary %#v", len(result.Diagnostics), result.Summary)
	}
}

func projectFixture(t *testing.T, adapter metadataFixtureReader, source SourceCandidate) ProjectionResult {
	t.Helper()
	file, err := os.Open(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := adapter.Project(context.Background(), ProjectionRequest{Source: source, Reader: file})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertKnownField(t *testing.T, name string, field MetadataField, want string) {
	t.Helper()
	if field.State != MetadataKnown || field.Value == nil || *field.Value != want || field.Evidence == nil || *field.Evidence != EvidenceNativeDurable {
		t.Fatalf("%s = %#v, want known %q native durable", name, field, want)
	}
}

// The bug this pins: a thread whose last message was hours old read as active
// because Claude Code appended bookkeeping — a background task reporting in,
// with its attachments and queue rows (the operator 2026-08-07).
func TestClaudeUpdatedAtTracksConversationNotFileActivity(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: "/sanitized/project/session-id.jsonl", Kind: ThreadMain,
	}
	input := strings.Join([]string{
		`{"type":"user","sessionId":"session-id","timestamp":"2026-08-07T15:00:00Z","message":{"role":"user","content":[{"type":"text","text":"Do the thing"}]}}`,
		`{"type":"assistant","sessionId":"session-id","timestamp":"2026-08-07T15:47:31Z","message":{"role":"assistant","model":"claude-opus-5","content":"Done"}}`,
		`{"type":"user","sessionId":"session-id","timestamp":"2026-08-07T15:47:35Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}`,
		`{"type":"queue-operation","sessionId":"session-id","timestamp":"2026-08-07T17:17:26Z","operation":"enqueue"}`,
		`{"type":"user","sessionId":"session-id","timestamp":"2026-08-07T17:17:26Z","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>stopped</task-notification>"}}`,
		`{"type":"attachment","sessionId":"session-id","timestamp":"2026-08-07T17:17:26Z","attachment":{"type":"skill_listing"}}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.UpdatedAt == nil || !result.Projection.UpdatedAt.Equal(time.Date(2026, 8, 7, 15, 47, 31, 0, time.UTC)) {
		t.Fatalf("updated at = %v, want the 15:47:31 assistant message", result.Projection.UpdatedAt)
	}
	// The span still opens at the first row, and the file's later activity is
	// not lost — it simply does not decide how recent the thread looks.
	if result.Projection.CreatedAt == nil || !result.Projection.CreatedAt.Equal(time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("created at = %v, want the first row", result.Projection.CreatedAt)
	}
}

// A thread can hold no conversation at all — a session that died during setup.
// It still needs a time to sort and display by.
func TestClaudeUpdatedAtFallsBackToActivityWithoutMessages(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects},
		Path: "/sanitized/project/session-id.jsonl", Kind: ThreadMain,
	}
	input := strings.Join([]string{
		`{"type":"permission-mode","sessionId":"session-id","timestamp":"2026-08-07T15:00:00Z","permissionMode":"default"}`,
		`{"type":"attachment","sessionId":"session-id","timestamp":"2026-08-07T15:00:05Z","attachment":{"type":"skill_listing"}}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderClaude}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.UpdatedAt == nil || !result.Projection.UpdatedAt.Equal(time.Date(2026, 8, 7, 15, 0, 5, 0, time.UTC)) {
		t.Fatalf("updated at = %v, want the last activity fallback", result.Projection.UpdatedAt)
	}
}

func TestCodexUpdatedAtTracksConversationNotTelemetry(t *testing.T) {
	source := SourceCandidate{
		Root: SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions},
		Path: "/history/rollout-2026-08-07-4cb13627-a7e6-5fad-892d-e5c187d3659d.jsonl", Kind: ThreadMain,
	}
	input := strings.Join([]string{
		`{"type":"session_meta","timestamp":"2026-08-07T15:00:00Z","payload":{"id":"4cb13627-a7e6-5fad-892d-e5c187d3659d","cwd":"/w"}}`,
		`{"type":"response_item","timestamp":"2026-08-07T15:00:10Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Do the thing"}]}}`,
		`{"type":"response_item","timestamp":"2026-08-07T15:05:00Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done"}]}}`,
		`{"type":"response_item","timestamp":"2026-08-07T15:05:30Z","payload":{"type":"function_call","name":"shell"}}`,
		`{"type":"event_msg","timestamp":"2026-08-07T16:00:00Z","payload":{"type":"token_count","info":{}}}`,
		`{"type":"turn_context","timestamp":"2026-08-07T16:00:01Z","payload":{"cwd":"/w","model":"gpt-5"}}`,
		`{"type":"response_item","timestamp":"2026-08-07T16:00:02Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd changed</environment_context>"}]}}`,
	}, "\n") + "\n"
	result, err := (metadataFixtureReader{ProviderCodex}).Project(context.Background(), ProjectionRequest{Source: source, Reader: strings.NewReader(input)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Projection.UpdatedAt == nil || !result.Projection.UpdatedAt.Equal(time.Date(2026, 8, 7, 15, 5, 0, 0, time.UTC)) {
		t.Fatalf("updated at = %v, want the 15:05 assistant message", result.Projection.UpdatedAt)
	}
}
