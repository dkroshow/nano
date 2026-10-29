package sessions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/wire"
)

func TestGrokCompletedPromptNamesItsAnswer(t *testing.T) {
	p := newLiveProjection("grok")
	p.setNativeThreadID("s1")
	at := time.UnixMilli(1000)
	for index, text := range []string{"hello ", "world"} {
		line := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":` + mustJSONText(text) + `}},"_meta":{"promptId":"p1","eventId":"chunk-` + fmt.Sprint(index) + `","agentTimestampMs":1000}}}`)
		p.applyWire(wire.Recv, line, at)
	}
	terminal := []byte(`{"method":"_x.ai/session_notification","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn"},"_meta":{"eventId":"s1-2"}}}`)
	p.applyWire(wire.Recv, terminal, at)

	if got := p.finalAgentItemID("p1"); got == "" {
		t.Fatal("the completed prompt has no final answer")
	}
}

func TestGrokAnswerAfterAToolIsTheFinalAnswer(t *testing.T) {
	p := newLiveProjection("grok")
	p.setNativeThreadID("s1")
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
		p.applyWire(wire.Recv, []byte(line), at)
	}
	// The answer is the message after the tool, not the one before it.
	if got := p.finalAgentItemID("p1"); got == "" || strings.Contains(got, "e1") {
		t.Fatalf("final item = %q, want the message after the tool", got)
	}
}

func mustJSONText(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
