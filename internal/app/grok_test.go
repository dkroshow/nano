package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	grokfixture "github.com/dkroshow/nano/internal/testsupport/grokserver"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestResolveGrokSpawnAcceptsSupportedSessionPermissionPosture(t *testing.T) {
	cwd := t.TempDir()
	s := &server{defaults: sessionDefaults{cwd: cwd}}
	for _, mode := range []string{"", "auto", "bypassPermissions"} {
		resolved, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "grok", Mode: "start", Cwd: cwd, PermissionMode: mode}})
		if err != nil || resolved.PermissionMode != mode {
			t.Fatalf("resolveSpawn(%q) = mode %q, error %v", mode, resolved.PermissionMode, err)
		}
	}
	_, err := s.resolveSpawn(spawnRequest{ProviderRequest: providerRequest{Provider: "grok", Mode: "start", Cwd: cwd, PermissionMode: "acceptEdits"}})
	if err == nil || !strings.Contains(err.Error(), "Grok permissionMode") {
		t.Fatalf("unsupported resolveSpawn error = %v", err)
	}
}

func TestGrokProviderSelectionSettlesCanonicalEffectiveEffort(t *testing.T) {
	sess, store := causationTestSessionForProvider(t, "nht_grok_effort", "grok")
	sess.RecordCreationPosture(providerRequest{Effort: "xhigh"})
	result := json.RawMessage(`{"sessionId":"native","_meta":{"x.ai/sessionConfig":{"options":[{"category":"mode","id":"xhigh","selected":true},{"category":"mode","id":"high","selected":false}]}}}`)
	provider := grokfixture.NewServer(t, func(c *grokfixture.Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case "initialize":
				c.Reply(call, map[string]any{"protocolVersion": 1})
			case "session/new":
				c.Reply(call, result)
			default:
				c.Reply(call, map[string]any{})
			}
		}
	})
	if err := sess.BeginProviderSpawn(providerRequest{Provider: "grok", Mode: "start", Effort: "xhigh", GrokBin: provider.Bin(), Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaudeStatus(t, sess, "ready")
	field, desired, effective := fieldStrings(t, store, sess.HostedThreadID(), postureFieldEffort)
	if desired != "xhigh" || effective != "xhigh" || field.DesiredDisposition != metadataDispositionConfirmed || field.EffectiveDisposition != metadataDispositionConfirmed {
		t.Fatalf("canonical effort = %+v desired=%q effective=%q", field, desired, effective)
	}
}

func TestGrokCompletedPromptNamesItsAnswer(t *testing.T) {
	p := newLiveProjection("grok")
	p.SetNativeThreadID("s1")
	at := time.UnixMilli(1000)
	for index, text := range []string{"hello ", "world"} {
		line := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":` + mustJSONText(text) + `}},"_meta":{"promptId":"p1","eventId":"chunk-` + fmt.Sprint(index) + `","agentTimestampMs":1000}}}`)
		p.applyWire(protocol.Recv, line, at)
	}
	terminal := []byte(`{"method":"_x.ai/session_notification","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn"},"_meta":{"eventId":"s1-2"}}}`)
	p.applyWire(protocol.Recv, terminal, at)

	if got := p.FinalAgentItemID("p1"); got == "" {
		t.Fatal("the completed prompt has no final answer")
	}
}

func TestGrokAnswerAfterAToolIsTheFinalAnswer(t *testing.T) {
	p := newLiveProjection("grok")
	p.SetNativeThreadID("s1")
	at := time.UnixMilli(1000)
	lines := []string{
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"I will run it."}},"_meta":{"promptId":"p1","eventId":"e1"}}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"Run command","rawInput":{"command":"true"},"_meta":{"x.ai/tool":{"name":"run_terminal_command","kind":"execute"}}},"_meta":{"promptId":"p1","eventId":"e2","updateParams":{"status":"Pending"}}}}`,
		// Real Grok progress updates can omit prompt and tool metadata. They must
		// retain the identity learned from the initial call.
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"ok"}}]},"_meta":{"eventId":"e3"}}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done."}},"_meta":{"promptId":"p1","eventId":"e4"}}}`,
		`{"method":"_x.ai/session_notification","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn"},"_meta":{"eventId":"e5"}}}`,
	}
	for _, line := range lines {
		p.applyWire(protocol.Recv, []byte(line), at)
	}
	// The answer is the message after the tool, not the one before it.
	if got := p.FinalAgentItemID("p1"); got == "" || strings.Contains(got, "e1") {
		t.Fatalf("final item = %q, want the message after the tool", got)
	}
}

func mustJSONText(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
