package grok

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// comparable is Update with the raw-bytes evidence fields zeroed out, so
// table tests can assert on the typed fields with reflect-free equality.
type comparable struct {
	Kind      updateKind
	SessionID string
	PromptID  string
	EventID   string
	NativeID  string
	Text      string
	Delta     string
	Done      bool
	Status    string
	Title     string
	ToolName  string
	ToolKind  string
	CallID    string
	AtMs      int64
}

func strip(u decodedUpdate) comparable {
	return comparable{
		Kind:      u.Kind,
		SessionID: u.SessionID,
		PromptID:  u.PromptID,
		EventID:   u.EventID,
		NativeID:  u.NativeID,
		Text:      u.Text,
		Delta:     u.Delta,
		Done:      u.Done,
		Status:    u.Status,
		Title:     u.Title,
		ToolName:  u.ToolName,
		ToolKind:  u.ToolKind,
		CallID:    u.CallID,
		AtMs:      u.AtMs,
	}
}

func projectOne(t *testing.T, raw string) decodedUpdate {
	t.Helper()
	got, err := decodeUpdate([]byte(raw))
	if err != nil {
		t.Fatalf("decodeUpdate: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("decodeUpdate: got %d updates, want 1", len(got))
	}
	return got[0]
}

func TestDecodeUpdate_Discriminants(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want comparable
	}{
		{
			name: "user_message_chunk",
			raw: `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hello agent"},` +
				`"_meta":{"modelId":"grok-test","promptIndex":0}},` +
				`"_meta":{"eventId":"sess-1-1","agentTimestampMs":1893456000123}}}`,
			want: comparable{
				Kind: kindUserMessage, SessionID: "sess-1", EventID: "sess-1-1",
				NativeID: "sess-1-1/userMessage", Text: "hello agent", Delta: "hello agent",
				AtMs: 1893456000123,
			},
		},
		{
			name: "agent_thought_chunk_live",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking it over"}},` +
				`"_meta":{"eventId":"sess-1-2","agentTimestampMs":1893456001000,"promptId":"prompt-1"}}}`,
			want: comparable{
				Kind: kindReasoning, SessionID: "sess-1", EventID: "sess-1-2", PromptID: "prompt-1",
				NativeID: "prompt-1/reasoning", Text: "thinking it over", Delta: "thinking it over",
				AtMs: 1893456001000,
			},
		},
		{
			name: "agent_message_chunk_durable_fallback_timestamp",
			raw: `{"timestamp":1893456002,"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}},` +
				`"_meta":{"eventId":"sess-1-3","promptId":"prompt-1"}}}`,
			want: comparable{
				Kind: kindAgentMessage, SessionID: "sess-1", EventID: "sess-1-3", PromptID: "prompt-1",
				NativeID: "prompt-1/agentMessage", Text: "done", Delta: "done",
				AtMs: 1893456002000,
			},
		},
		{
			name: "tool_call_pending",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"run_shell",` +
				`"rawInput":{"command":"echo hi"},` +
				`"_meta":{"x.ai/tool":{"version":1,"name":"run_shell","kind":"execute","namespace":"grok_build"}}},` +
				`"_meta":{"eventId":"sess-1-4","promptId":"prompt-1",` +
				`"updateParams":{"toolCallId":"call-1","title":"run_shell","kind":"Other","status":"Pending"}}}}`,
			want: comparable{
				Kind: kindToolCall, SessionID: "sess-1", EventID: "sess-1-4", PromptID: "prompt-1",
				NativeID: "call-1", CallID: "call-1", Title: "run_shell",
				ToolName: "run_shell", ToolKind: "execute", Status: "Pending",
			},
		},
		{
			name: "tool_call_update_in_progress",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","kind":"execute",` +
				`"title":"Execute ` + "`echo hi`" + `","status":"in_progress",` +
				`"content":[{"type":"content","content":{"type":"text","text":""}}],` +
				`"_meta":{"x.ai/tool":{"name":"run_shell","kind":"execute"}}},` +
				`"_meta":{"eventId":"sess-1-5","promptId":"prompt-1","updateParams":{"toolCallId":"call-1","status":null}}}}`,
			want: comparable{
				Kind: kindToolUpdate, SessionID: "sess-1", EventID: "sess-1-5", PromptID: "prompt-1",
				NativeID: "call-1", CallID: "call-1", Title: "Execute `echo hi`",
				ToolName: "run_shell", ToolKind: "execute", Status: "in_progress",
			},
		},
		{
			name: "tool_call_update_completed",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","kind":"execute",` +
				`"title":"Execute ` + "`echo hi`" + `","status":"completed",` +
				`"content":[{"type":"content","content":{"type":"text","text":"hi"}}],` +
				`"_meta":{"x.ai/tool":{"name":"run_shell","kind":"execute"}}},` +
				`"_meta":{"eventId":"sess-1-6","promptId":"prompt-1","updateParams":{"toolCallId":"call-1","status":"Completed"}}}}`,
			want: comparable{
				Kind: kindToolUpdate, SessionID: "sess-1", EventID: "sess-1-6", PromptID: "prompt-1",
				NativeID: "call-1", CallID: "call-1", Title: "Execute `echo hi`",
				ToolName: "run_shell", ToolKind: "execute", Status: "completed",
			},
		},
		{
			name: "plan",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"plan","title":"Implementation plan",` +
				`"content":{"type":"text","text":"Step 1: read the file"}},` +
				`"_meta":{"eventId":"sess-1-7","promptId":"prompt-1"}}}`,
			want: comparable{
				Kind: kindPlan, SessionID: "sess-1", EventID: "sess-1-7", PromptID: "prompt-1",
				NativeID: "prompt-1/plan", Title: "Implementation plan", Text: "Step 1: read the file",
			},
		},
		{
			name: "turn_completed",
			raw: `{"method":"_x.ai/session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1","stop_reason":"end_turn",` +
				`"usage":{"inputTokens":100,"outputTokens":10,"totalTokens":110}},` +
				`"_meta":{"eventId":"sess-1-8"}}}`,
			want: comparable{
				Kind: kindTerminal, SessionID: "sess-1", EventID: "sess-1-8", PromptID: "prompt-1",
				NativeID: "prompt-1/terminal", Status: "end_turn", Done: true,
			},
		},
		{
			name: "unknown_discriminant",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"available_commands_update","commands":["/help"]},` +
				`"_meta":{"eventId":"sess-1-9","promptId":"prompt-1"}}}`,
			want: comparable{
				Kind: kindUnclassified, SessionID: "sess-1", EventID: "sess-1-9", PromptID: "prompt-1",
				NativeID: "sess-1-9/unclassified",
			},
		},
		{
			name: "hook_execution",
			raw: `{"timestamp":1893456010,"method":"_x.ai/session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"hook_execution","event_name":"user_prompt_submit","prompt_id":"prompt-2",` +
				`"runs":[{"name":"global/settings:user_prompt_submit[0].hooks[0]","status":{"status":"success","elapsed_ms":5}}]},` +
				`"_meta":{"eventId":"sess-1-10"}}}`,
			want: comparable{
				Kind: kindHook, SessionID: "sess-1", EventID: "sess-1-10",
				NativeID: "sess-1-10/hook", AtMs: 1893456010000,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := projectOne(t, tc.raw)
			if got := strip(u); got != tc.want {
				t.Fatalf("strip(decodeUpdate) =\n  %+v\nwant\n  %+v", got, tc.want)
			}
			if len(u.Payload) == 0 {
				t.Fatalf("Payload not populated")
			}
			if len(u.Raw) == 0 {
				t.Fatalf("Raw not populated")
			}
			if tc.want.Kind == kindTerminal {
				if len(u.Usage) == 0 {
					t.Fatalf("Usage not populated for terminal update")
				}
			} else if u.Usage != nil {
				t.Fatalf("Usage unexpectedly populated for kind %s: %s", tc.want.Kind, u.Usage)
			}
		})
	}
}

func TestDecodeUpdate_LiveXAINotification(t *testing.T) {
	u := projectOne(t, `{"jsonrpc":"2.0","method":"_x.ai/session_notification","params":{`+
		`"sessionId":"sess-1","update":{"sessionUpdate":"turn_completed","prompt_id":"prompt-1",`+
		`"stop_reason":"end_turn","usage":{"inputTokens":3,"outputTokens":2}},`+
		`"_meta":{"eventId":"sess-1-10","agentTimestampMs":1893456003000}}}`)
	if u.Kind != kindTerminal || u.PromptID != "prompt-1" || u.Status != "end_turn" || !u.Done {
		t.Fatalf("live extension projection = %+v", strip(u))
	}
}

func TestDecodeUpdate_UnknownPreservesRawAndPayload(t *testing.T) {
	raw := `{"method":"session/update","params":{"sessionId":"sess-1",` +
		`"update":{"sessionUpdate":"future_thing","detail":"whatever the next Grok release adds"},` +
		`"_meta":{"eventId":"sess-1-99","promptId":"prompt-9"}}}`
	u := projectOne(t, raw)
	if u.Kind != kindUnclassified {
		t.Fatalf("Kind = %v, want unclassified", u.Kind)
	}
	var payload map[string]any
	if err := json.Unmarshal(u.Payload, &payload); err != nil {
		t.Fatalf("Payload is not valid JSON: %v", err)
	}
	if payload["sessionUpdate"] != "future_thing" {
		t.Fatalf("Payload lost the original discriminant: %v", payload)
	}
	var full map[string]any
	if err := json.Unmarshal(u.Raw, &full); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if full["method"] != "session/update" {
		t.Fatalf("Raw lost the envelope method: %v", full)
	}
}

func TestDecodeUpdate_UnrelatedMethodIgnored(t *testing.T) {
	raw := `{"method":"initialize","params":{"protocolVersion":1}}`
	got, err := decodeUpdate([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestDecodeUpdate_Malformed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"truncated_json", `{"method": "session/update", "params":`},
		{"not_an_object", `"just a string"`},
		{"missing_params", `{"method":"session/update"}`},
		{"missing_update", `{"method":"session/update","params":{"sessionId":"sess-1"}}`},
		{"update_not_an_object", `{"method":"session/update","params":{"sessionId":"sess-1","update":"oops"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeUpdate([]byte(tc.raw))
			if err == nil {
				t.Fatalf("got (%v, nil), want a descriptive error", got)
			}
			if got != nil {
				t.Fatalf("got %v updates alongside error, want nil", got)
			}
		})
	}
}

func TestDecodeUpdate_Oversized(t *testing.T) {
	padding := strings.Repeat("a", MaxUpdateBytes+1)
	raw := `{"method":"session/update","params":{"sessionId":"` + padding + `"}}`
	got, err := decodeUpdate([]byte(raw))
	if !errors.Is(err, ErrUpdateTooLarge) {
		t.Fatalf("err = %v, want ErrUpdateTooLarge", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestDecodeUpdate_TimestampPriority(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantMs int64
	}{
		{
			name: "agentTimestampMs_wins_over_durable_timestamp",
			raw: `{"timestamp":1000,"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}},` +
				`"_meta":{"eventId":"e1","agentTimestampMs":2500000}}}`,
			wantMs: 2500000,
		},
		{
			name: "durable_timestamp_converted_to_ms_when_agentTimestampMs_absent",
			raw: `{"timestamp":1700000000,"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}},` +
				`"_meta":{"eventId":"e2"}}}`,
			wantMs: 1700000000000,
		},
		{
			name: "neither_present_yields_zero",
			raw: `{"method":"session/update","params":{"sessionId":"sess-1",` +
				`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}},` +
				`"_meta":{"eventId":"e3"}}}`,
			wantMs: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := projectOne(t, tc.raw)
			if u.AtMs != tc.wantMs {
				t.Fatalf("AtMs = %d, want %d", u.AtMs, tc.wantMs)
			}
		})
	}
}

// TestDecodeUpdate_LiveDurableParity proves the same ACP update projects
// identically whether it arrives as a live JSON-RPC notification or as a
// durable updates.jsonl row (which only differs by the added top-level
// "timestamp" field). Only AtMs sourcing and Raw (which legitimately
// contains different envelope bytes) are allowed to differ.
func TestDecodeUpdate_LiveDurableParity(t *testing.T) {
	live := `{"method":"session/update","params":{"sessionId":"sess-1",` +
		`"update":{"sessionUpdate":"tool_call","toolCallId":"call-7","title":"write",` +
		`"_meta":{"x.ai/tool":{"name":"write","kind":"write"}}},` +
		`"_meta":{"eventId":"sess-1-50","promptId":"prompt-5","agentTimestampMs":42424242,` +
		`"updateParams":{"toolCallId":"call-7","status":"Pending"}}}}`
	durable := `{"timestamp":999999,"method":"session/update","params":{"sessionId":"sess-1",` +
		`"update":{"sessionUpdate":"tool_call","toolCallId":"call-7","title":"write",` +
		`"_meta":{"x.ai/tool":{"name":"write","kind":"write"}}},` +
		`"_meta":{"eventId":"sess-1-50","promptId":"prompt-5","agentTimestampMs":42424242,` +
		`"updateParams":{"toolCallId":"call-7","status":"Pending"}}}}`

	liveU := projectOne(t, live)
	durableU := projectOne(t, durable)

	if strip(liveU) != strip(durableU) {
		t.Fatalf("live/durable parity broke on typed fields:\n live=%+v\n durable=%+v", strip(liveU), strip(durableU))
	}
	if !bytes.Equal(liveU.Payload, durableU.Payload) {
		t.Fatalf("Payload differs between live and durable envelopes:\n live=%s\n durable=%s", liveU.Payload, durableU.Payload)
	}
	if bytes.Equal(liveU.Raw, durableU.Raw) {
		t.Fatalf("Raw unexpectedly identical: the durable envelope carries an extra timestamp field")
	}
}

func TestDecodeUpdate_StableNativeID(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "tool_call_id_wins_even_with_prompt_id",
			raw: `{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update",` +
				`"toolCallId":"call-42","status":"completed"},"_meta":{"eventId":"e1","promptId":"p1"}}}`,
			want: "call-42",
		},
		{
			name: "prompt_id_used_when_no_tool_call_id",
			raw: `{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk",` +
				`"content":{"type":"text","text":"hi"}},"_meta":{"eventId":"e2","promptId":"p2"}}}`,
			want: "p2/agentMessage",
		},
		{
			name: "event_id_used_when_neither_tool_call_id_nor_prompt_id",
			raw: `{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk",` +
				`"content":{"type":"text","text":"hi"}},"_meta":{"eventId":"e3"}}}`,
			want: "e3/agentMessage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := projectOne(t, tc.raw)
			if u.NativeID != tc.want {
				t.Fatalf("NativeID = %q, want %q", u.NativeID, tc.want)
			}
		})
	}
}

// TestDecodeUpdate_RawPayloadOwnership proves Update.Raw and Update.Payload
// are independent copies: mutating the caller's input buffer after
// decodeUpdate returns must not change the returned Update.
func TestDecodeUpdate_RawPayloadOwnership(t *testing.T) {
	raw := []byte(`{"method":"session/update","params":{"sessionId":"sess-1",` +
		`"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}},` +
		`"_meta":{"eventId":"e1","promptId":"p1"}}}`)
	rawCopy := append([]byte(nil), raw...)

	got, err := decodeUpdate(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u := got[0]
	payloadCopy := append([]byte(nil), u.Payload...)
	rawFieldCopy := append([]byte(nil), u.Raw...)

	// Mutate the caller's buffer in place after the call returns.
	for i := range raw {
		raw[i] = 'x'
	}

	if !bytes.Equal(u.Raw, rawFieldCopy) {
		t.Fatalf("Update.Raw changed after caller mutated its input buffer")
	}
	if !bytes.Equal(u.Payload, payloadCopy) {
		t.Fatalf("Update.Payload changed after caller mutated its input buffer")
	}
	if !bytes.Equal(u.Raw, rawCopy) {
		t.Fatalf("Update.Raw does not match the original input bytes")
	}
}

func TestDecodeUpdate_ToolStatusProgression(t *testing.T) {
	statuses := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "pending_from_tool_call",
			raw: `{"method":"session/update","params":{"sessionId":"s",` +
				`"update":{"sessionUpdate":"tool_call","toolCallId":"call-9"},` +
				`"_meta":{"eventId":"e1","updateParams":{"toolCallId":"call-9","status":"Pending"}}}}`,
			want: "Pending",
		},
		{
			name: "in_progress_from_tool_call_update",
			raw: `{"method":"session/update","params":{"sessionId":"s",` +
				`"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-9","status":"in_progress"},` +
				`"_meta":{"eventId":"e2"}}}`,
			want: "in_progress",
		},
		{
			name: "completed_from_tool_call_update",
			raw: `{"method":"session/update","params":{"sessionId":"s",` +
				`"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-9","status":"completed"},` +
				`"_meta":{"eventId":"e3"}}}`,
			want: "completed",
		},
	}
	for _, tc := range statuses {
		t.Run(tc.name, func(t *testing.T) {
			u := projectOne(t, tc.raw)
			if u.Status != tc.want {
				t.Fatalf("Status = %q, want %q", u.Status, tc.want)
			}
			if u.CallID != "call-9" || u.NativeID != "call-9" {
				t.Fatalf("tool identity not stable across progression: CallID=%q NativeID=%q", u.CallID, u.NativeID)
			}
		})
	}
}

func TestDecodeUpdate_SubagentCoordinationAndControls(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind updateKind
	}{
		{
			name: "spawn",
			raw:  `{"method":"_x.ai/session_notification","params":{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_spawned","child_session_id":"child-1","parent_session_id":"parent-1","parent_prompt_id":"prompt-1","description":"check tests","model":"grok-4","effective_context_source":"parent","capability_mode":"read_only"},"_meta":{"eventId":"spawn-1"}}}`,
			kind: kindSubagentSpawn,
		},
		{
			name: "progress",
			raw:  `{"method":"_x.ai/session_notification","params":{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_progress","child_session_id":"child-1","parent_session_id":"parent-1","description":"checking"},"_meta":{"eventId":"progress-1"}}}`,
			kind: kindSubagentProgress,
		},
		{
			name: "finish",
			raw:  `{"method":"_x.ai/session_notification","params":{"sessionId":"parent-1","update":{"sessionUpdate":"subagent_finished","child_session_id":"child-1","parent_session_id":"parent-1","status":"completed","duration_ms":12,"tool_calls":3,"turns":2,"tokens_used":99},"_meta":{"eventId":"finish-1"}}}`,
			kind: kindSubagentFinish,
		},
		{
			name: "hook",
			raw:  `{"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"hook_execution"},"_meta":{"eventId":"hook-1"}}}`,
			kind: kindHook,
		},
		{
			name: "hidden user",
			raw:  `{"method":"session/update","params":{"sessionId":"parent-1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"control"},"_meta":{"hideFromScrollback":true}},"_meta":{"eventId":"hidden-1"}}}`,
			kind: kindHiddenUser,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := projectOne(t, tc.raw)
			if u.Kind != tc.kind {
				t.Fatalf("Kind = %q, want %q", u.Kind, tc.kind)
			}
			if len(u.Raw) == 0 || len(u.Payload) == 0 {
				t.Fatal("typed projection dropped raw evidence")
			}
			if tc.kind == kindSubagentSpawn || tc.kind == kindSubagentProgress || tc.kind == kindSubagentFinish {
				if u.Subagent == nil || u.Subagent.ChildSessionID != "child-1" || u.Subagent.ParentSessionID != "parent-1" {
					t.Fatalf("subagent = %#v", u.Subagent)
				}
			}
		})
	}
}

func TestPhase0FixtureProjectsSameCoordinationLiveAndDurable(t *testing.T) {
	project := func(path string, unwrapWire bool) map[string]updateKind {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]updateKind{}
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			if unwrapWire {
				var fixture struct {
					Msg json.RawMessage `json:"msg"`
				}
				if err := json.Unmarshal(line, &fixture); err != nil {
					t.Fatal(err)
				}
				line = fixture.Msg
			}
			updates, err := decodeUpdate(line)
			if err != nil {
				t.Fatal(err)
			}
			for _, update := range updates {
				if update.EventID == "parent-1-200" || update.EventID == "parent-1-202" {
					got[update.EventID] = update.Kind
				}
			}
		}
		return got
	}
	live := project("testdata/phase0-parent-wire.jsonl", true)
	durable := project("testdata/phase0-parent-updates.jsonl", false)
	if len(live) != 2 || len(durable) != 2 || live["parent-1-200"] != kindSubagentSpawn ||
		live["parent-1-202"] != kindSubagentFinish || live["parent-1-200"] != durable["parent-1-200"] ||
		live["parent-1-202"] != durable["parent-1-202"] {
		t.Fatalf("fixture parity live=%#v durable=%#v", live, durable)
	}
}
