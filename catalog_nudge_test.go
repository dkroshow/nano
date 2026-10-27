package main

import (
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// Synthetic wire examples following internal/wire/codex/captures/first-turn.jsonl (a synthetic
// codex app-server session) and testdata/live_projection/claude.jsonl, so the
// filter is pinned against shapes the providers actually emit rather than
// shapes a test invented.
const (
	codexThreadStartedWire    = `{"method":"thread/started","params":{"thread":{"id":"c681a046-47a5-5fd8-92c7-9982f3468d5a","sessionId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","preview":"","ephemeral":false,"modelProvider":"openai","createdAt":1783299757,"status":{"type":"idle"},"path":"/home/user/.codex/sessions/2026/07/05/rollout-2026-07-05T18-02-36-c681a046-47a5-5fd8-92c7-9982f3468d5a.jsonl","cwd":"/home/user/Code/nano","turns":[]}}}`
	codexTurnStartedWire      = `{"method":"turn/started","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","turn":{"id":"e6c5c93d-7cc5-5ec9-b350-6ca225766381","items":[],"itemsView":"notLoaded","status":"inProgress","error":null,"startedAt":1783299757,"completedAt":null,"durationMs":null}}}`
	codexTurnCompletedWire    = `{"method":"turn/completed","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","turn":{"id":"e6c5c93d-7cc5-5ec9-b350-6ca225766381","items":[],"itemsView":"notLoaded","status":"completed","error":null,"startedAt":1783299757,"completedAt":1783299760,"durationMs":2940}}}`
	codexTurnFailedWire       = `{"method":"turn/completed","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","turn":{"id":"e6c5c93d-7cc5-5ec9-b350-6ca225766381","status":"failed","error":{"message":"stream disconnected"},"startedAt":1783299757,"completedAt":1783299760}}}`
	codexItemCompletedWire    = `{"method":"item/completed","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","item":{"type":"userMessage","id":"ec8ec6cf-eda7-5caf-bb3d-945e230a3c6d","content":[{"type":"text","text":"Reply with exactly: PROTOCOL_SPIKE_OK"}]}}}`
	codexItemStartedWire      = `{"method":"item/started","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","item":{"type":"agentMessage","id":"msg_f932e2e7e35a5c869e095262ab68ed0f"}}}`
	codexTokenUsageWire       = `{"method":"thread/tokenUsage/updated","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","turnId":"e6c5c93d-7cc5-5ec9-b350-6ca225766381","tokenUsage":{"total":{"totalTokens":12185,"inputTokens":12165}}}}`
	codexAgentDeltaWire       = `{"method":"item/agentMessage/delta","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","turnId":"e6c5c93d-7cc5-5ec9-b350-6ca225766381","itemId":"msg_f932e2e7e35a5c869e095262ab68ed0f","delta":"PROTOCOL"}}`
	codexThreadStatusWire     = `{"method":"thread/status/changed","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","status":{"type":"active","activeFlags":[]}}}`
	codexMcpStatusWire        = `{"method":"mcpServer/startupStatus/updated","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","name":"codex_apps","status":"starting","error":null}}`
	codexThreadStartRespWire  = `{"id":2,"result":{"thread":{"id":"c681a046-47a5-5fd8-92c7-9982f3468d5a"}}}`
	codexApprovalRequestWire  = `{"id":9,"method":"execCommandApproval","params":{"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a","command":["rm","-rf","x"]}}`
	codexTurnStartRequestWire = `{"id":3,"method":"turn/start","params":{"input":[{"text":"Reply with exactly: PROTOCOL_SPIKE_OK","type":"text"}],"threadId":"c681a046-47a5-5fd8-92c7-9982f3468d5a"}}`

	claudeInitWire         = `{"type":"system","subtype":"init","session_id":"native-1","model":"sonnet"}`
	claudeSystemStatusWire = `{"type":"system","subtype":"status"}`
	claudeResultWire       = `{"type":"result","subtype":"success","duration_ms":10}`
	claudeUserTurnWire     = `{"type":"user","message":{"id":"u-1","content":[{"type":"text","text":"continue"}]}}`
	claudeToolResultWire   = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"text","text":"README"}],"is_error":false}]}}`
	claudeAssistantWire    = `{"type":"assistant","message":{"id":"m-1","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"path":"README.md"}}]}}`
	claudeStreamEventWire  = `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"text":"Hello"}}}`
	claudeUnknownTypeWire  = `{"type":"unknown_provider_shape","payload":{"preserve":"raw feed"}}`
)

// TestWireNudgesCatalogFiresOnTurnEdgesOnly pins the nudge contract for both
// providers: turn edges (start and completion) and the identity-binding line
// nudge, everything mid-turn does not. The turn-START edge matters as much as
// completion — the fleet feed reads `working` off catalog version bumps.
func TestWireNudgesCatalogFiresOnTurnEdgesOnly(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		dir      protocol.Direction
		line     string
		want     bool
	}{
		// codex turn edges + identity.
		{"codex turn started", "codex", protocol.Recv, codexTurnStartedWire, true},
		{"codex turn completed", "codex", protocol.Recv, codexTurnCompletedWire, true},
		{"codex turn completed failed status", "codex", protocol.Recv, codexTurnFailedWire, true},
		{"codex thread started", "codex", protocol.Recv, codexThreadStartedWire, true},

		// codex mid-turn traffic: durable, but nothing consumes the freshness.
		{"codex item completed", "codex", protocol.Recv, codexItemCompletedWire, false},
		{"codex item started", "codex", protocol.Recv, codexItemStartedWire, false},
		{"codex token usage", "codex", protocol.Recv, codexTokenUsageWire, false},
		{"codex agent message delta", "codex", protocol.Recv, codexAgentDeltaWire, false},
		{"codex thread status changed", "codex", protocol.Recv, codexThreadStatusWire, false},
		{"codex mcp startup status", "codex", protocol.Recv, codexMcpStatusWire, false},

		// codex non-notifications and wrong direction.
		{"codex response", "codex", protocol.Recv, codexThreadStartRespWire, false},
		{"codex server request", "codex", protocol.Recv, codexApprovalRequestWire, false},
		// Synthetic: codex has no request by this name. It pins the
		// notification-only guard, which an allowlist alone would not.
		{"codex request wearing an edge method", "codex", protocol.Recv, `{"id":11,"method":"turn/started","params":{"threadId":"t1"}}`, false},
		{"codex sent turn start request", "codex", protocol.Send, codexTurnStartRequestWire, false},
		{"codex turn started sent", "codex", protocol.Send, codexTurnStartedWire, false},
		{"codex malformed line", "codex", protocol.Recv, `{"jsonrpc":`, false},

		// claude turn edges + identity. The sent user line IS the start edge.
		{"claude sent user turn", "claude", protocol.Send, claudeUserTurnWire, true},
		{"claude result", "claude", protocol.Recv, claudeResultWire, true},
		{"claude system init", "claude", protocol.Recv, claudeInitWire, true},

		// claude mid-turn traffic and wrong-direction variants.
		{"claude received user tool result", "claude", protocol.Recv, claudeToolResultWire, false},
		{"claude received user turn", "claude", protocol.Recv, claudeUserTurnWire, false},
		{"claude assistant message", "claude", protocol.Recv, claudeAssistantWire, false},
		{"claude partial stream event", "claude", protocol.Recv, claudeStreamEventWire, false},
		{"claude other system line", "claude", protocol.Recv, claudeSystemStatusWire, false},
		{"claude sent result", "claude", protocol.Send, claudeResultWire, false},
		{"claude sent system init", "claude", protocol.Send, claudeInitWire, false},
		{"claude unknown type", "claude", protocol.Recv, claudeUnknownTypeWire, false},
		{"claude untyped line", "claude", protocol.Recv, `{"subtype":"noise"}`, false},
		{"claude malformed line", "claude", protocol.Recv, `{"type":`, false},

		{"unknown provider", "gemini", protocol.Recv, codexTurnStartedWire, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wireNudgesCatalog(tc.provider, tc.dir, []byte(tc.line)); got != tc.want {
				t.Fatalf("wireNudgesCatalog(%s, %s, %s) = %v, want %v", tc.provider, tc.dir, tc.line, got, tc.want)
			}
		})
	}
}

// TestRecordWireNudgesCatalogOnTurnEdgesOnly proves the recording path invokes
// the callback exactly when the filter passes: a mid-turn item/completed
// records without a nudge, the turn edge records with one.
func TestRecordWireNudgesCatalogOnTurnEdgesOnly(t *testing.T) {
	var nudges int
	sess := &session{provider: "codex",
		subs:              map[chan wireEvent]struct{}{},
		catalogRefresh:    func(string) { nudges++ },
		catalogSourcePath: "/sessions/rollout.jsonl",
		lifecycleState:    testLifecycleSnapshot("ready", false)}
	sess.recordWire(protocol.Recv, []byte(codexItemCompletedWire))
	if nudges != 0 {
		t.Fatalf("item/completed nudged the catalog %d times, want 0", nudges)
	}
	sess.recordWire(protocol.Recv, []byte(codexTurnCompletedWire))
	if nudges != 1 {
		t.Fatalf("turn/completed nudged the catalog %d times, want 1", nudges)
	}
	if len(sess.events) != 2 {
		t.Fatalf("recorded %d wire events, want 2", len(sess.events))
	}
}

func TestRecordWireTargetsKnownTranscriptPath(t *testing.T) {
	var codexPaths []string
	codexSession := &session{provider: "codex",
		subs:           map[chan wireEvent]struct{}{},
		catalogRefresh: func(path string) { codexPaths = append(codexPaths, path) },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	codexSession.recordWire(protocol.Recv, []byte(codexThreadStartedWire))
	codexSession.recordWire(protocol.Recv, []byte(codexTurnCompletedWire))
	wantCodex := "/home/user/.codex/sessions/2026/07/05/rollout-2026-07-05T18-02-36-c681a046-47a5-5fd8-92c7-9982f3468d5a.jsonl"
	if len(codexPaths) != 2 || codexPaths[0] != wantCodex || codexPaths[1] != wantCodex {
		t.Fatalf("codex refresh paths=%v, want cached %q twice", codexPaths, wantCodex)
	}

	var claudePath string
	claudeSession := &session{provider: "claude", threadID: "native-1", cwd: "/tmp/project",
		subs:           map[chan wireEvent]struct{}{},
		catalogRefresh: func(path string) { claudePath = path },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	claudeSession.recordWire(protocol.Send, []byte(claudeUserTurnWire))
	if want := claudeTranscriptPath("/tmp/project", "native-1"); claudePath != want {
		t.Fatalf("claude refresh path=%q, want %q", claudePath, want)
	}
}

// TestRecordWireNudgesCatalogOnClaudeTurnStart proves the one sent line that
// qualifies reaches the catalog through the recording path — the edge the
// fleet feed needs to report a claude session as working.
func TestRecordWireNudgesCatalogOnClaudeTurnStart(t *testing.T) {
	var nudges int
	sess := &session{provider: "claude", threadID: "native-1", cwd: "/tmp/project",
		subs:           map[chan wireEvent]struct{}{},
		catalogRefresh: func(string) { nudges++ },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.recordWire(protocol.Recv, []byte(claudeAssistantWire))
	if nudges != 0 {
		t.Fatalf("assistant line nudged the catalog %d times, want 0", nudges)
	}
	sess.recordWire(protocol.Send, []byte(claudeUserTurnWire))
	if nudges != 1 {
		t.Fatalf("sent user line nudged the catalog %d times, want 1", nudges)
	}
}

// TestRecordWireWithoutCatalogRefreshIsSafe proves a session spawned while the
// catalog is unavailable (nil callback) records nudging lines without panicking.
func TestRecordWireWithoutCatalogRefreshIsSafe(t *testing.T) {
	sess := &session{provider: "claude",
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.recordWire(protocol.Recv, []byte(claudeResultWire))
	if len(sess.events) != 1 {
		t.Fatalf("recorded %d wire events, want 1", len(sess.events))
	}
}

// Failure caught: raw traffic from a multiplexed child that has no attachment
// of its own requested a full root walk on every turn edge. Boundary: the
// parent's raw-wire recording path.
func TestRecordWireRawStartsNoCatalogWorkForUnattachedChild(t *testing.T) {
	nudges := 0
	sess := &session{provider: "grok",
		catalogRefresh: func(string) { nudges++ },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.recordWireRaw(protocol.Recv, []byte(`{"method":"_x.ai/session/prompt_complete","params":{"sessionId":"child","promptId":"child-prompt","stopReason":"end_turn"}}`))
	if nudges != 0 {
		t.Fatalf("raw child terminal nudges = %d, want 0", nudges)
	}
}

// TestRecordWireNudgeDoesNotBlockWhileTheWorkerIsBusy proves wire recording
// stays nonblocking when nothing drains the catalog queue (the steady state
// while a pass runs), and that repeated hints for one file coalesce.
func TestRecordWireNudgeDoesNotBlockWhileTheWorkerIsBusy(t *testing.T) {
	service := &threadCatalogService{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}}
	sess := &session{provider: "codex",
		subs:              map[chan wireEvent]struct{}{},
		catalogRefresh:    func(path string) { service.RequestPathRefresh([]string{path}) },
		catalogSourcePath: "/sessions/rollout.jsonl",
		lifecycleState:    testLifecycleSnapshot("ready", false)}
	line := []byte(codexTurnCompletedWire)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			sess.recordWire(protocol.Recv, line)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wire recording stalled behind a busy catalog worker")
	}
	if len(service.dirtyOrder) != 1 || len(service.work) != 1 {
		t.Fatalf("queued %d sources and %d wakes, want 1 and 1 (coalesced)", len(service.dirtyOrder), len(service.work))
	}
}
