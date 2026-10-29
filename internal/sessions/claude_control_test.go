package sessions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// AskUserQuestion answers are keyed by the question's own TEXT. Getting this
// wrong is silent: the turn succeeds and the model is simply told the user did
// not answer, so only an assertion on the payload catches it.
func TestClaudeQuestionAnswersKeyByQuestionText(t *testing.T) {
	params := json.RawMessage(`{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[
		{"question":"Tabs or spaces?","header":"Indent","options":[{"label":"Tabs"},{"label":"Spaces"}]},
		{"question":"Which files?","multiSelect":true,"options":[{"label":"a.go"},{"label":"b.go"}]}
	]}}`)
	p := interact.PendingInteraction{ID: "r1", Kind: kindUserInput, Method: claudeSubtypeCanUseTool, Params: params}
	answers := json.RawMessage(`{"Tabs or spaces?":["Spaces"],"Which files?":["a.go","b.go"]}`)

	result, rpcErr := claudeInteractionResult(p, decisionAccept, answers)
	if rpcErr != nil {
		t.Fatalf("accept: %v", rpcErr)
	}
	body, _ := json.Marshal(result)
	var decoded struct {
		Behavior     string `json:"behavior"`
		UpdatedInput struct {
			Answers   map[string]json.RawMessage `json:"answers"`
			Questions []json.RawMessage          `json:"questions"`
		} `json:"updatedInput"`
	}
	json.Unmarshal(body, &decoded)
	if decoded.Behavior != "allow" {
		t.Fatalf("behavior = %q, want allow", decoded.Behavior)
	}
	// The whole tool input is echoed back, not just the answers: the CLI
	// round-trips fields nano has no business reinterpreting.
	if len(decoded.UpdatedInput.Questions) != 2 {
		t.Fatalf("questions not echoed back: %s", body)
	}
	if got := string(decoded.UpdatedInput.Answers["Tabs or spaces?"]); got != `"Spaces"` {
		t.Fatalf("single-select answer = %s, want the bare label", got)
	}
	if got := string(decoded.UpdatedInput.Answers["Which files?"]); got != `["a.go","b.go"]` {
		t.Fatalf("multi-select answer = %s, want the label list", got)
	}

	// Skip is "allow, with nothing answered" — the CLI's own no-answer path.
	// A denial would instead surface to the model as a tool error.
	skipped, rpcErr := claudeInteractionResult(p, decisionDecline, nil)
	if rpcErr != nil {
		t.Fatalf("decline: %v", rpcErr)
	}
	body, _ = json.Marshal(skipped)
	if !strings.Contains(string(body), `"answers":{}`) || !strings.Contains(string(body), `"behavior":"allow"`) {
		t.Fatalf("skip payload = %s, want allow with empty answers", body)
	}
}

// A tool permission is approve/deny, and declining has to speak the wire's own
// refusal shape rather than an empty allow.
func TestClaudeToolPermissionDecisions(t *testing.T) {
	params := json.RawMessage(`{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"}}`)
	p := interact.PendingInteraction{ID: "r1", Kind: kindToolPermission, Method: claudeSubtypeCanUseTool, Params: params}

	allow, _ := claudeInteractionResult(p, decisionAccept, nil)
	body, _ := json.Marshal(allow)
	if !strings.Contains(string(body), `"behavior":"allow"`) || !strings.Contains(string(body), `"rm -rf /"`) {
		t.Fatalf("approve = %s, want allow echoing the input", body)
	}
	deny, _ := claudeInteractionResult(p, decisionDecline, nil)
	body, _ = json.Marshal(deny)
	if !strings.Contains(string(body), `"behavior":"deny"`) {
		t.Fatalf("deny = %s, want behavior deny", body)
	}
}

// controlResponder scripts a shim whose child answers every control request
// with `payload`, so a test can exercise the whole request/response correlation
// without a real claude binary. Frames arrive back the way the daemon delivers
// them: as recv `session/line` notifications.
func controlResponder(t *testing.T, payload func(subtype string) any) *claudeshim.Server {
	t.Helper()
	var seq int64
	return newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			c.Reply(call, nil)
			if call.Method != shimMethodInput {
				continue
			}
			var p struct {
				Line string `json:"line"`
			}
			if json.Unmarshal(call.Params, &p) != nil {
				continue
			}
			var frame struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Request   struct {
					Subtype string `json:"subtype"`
				} `json:"request"`
			}
			if json.Unmarshal([]byte(p.Line), &frame) != nil || frame.Type != claudeControlRequestType {
				continue
			}
			body, _ := json.Marshal(map[string]any{
				"type": claudeControlResponseType,
				"response": map[string]any{
					"subtype": claudeControlSuccess, "request_id": frame.RequestID,
					"response": payload(frame.Request.Subtype),
				},
			})
			seq++
			c.Line(seq, "recv", string(body))
		}
	})
}

func startControlSession(t *testing.T, shim *claudeshim.Server) (*Session, *claudeSession) {
	t.Helper()
	sess, _ := claudeTestSession(t)
	claude, err := startClaudeSession(sess, claudeShimSpawnRequest(sess, shim.WSURL()))
	if err != nil {
		t.Fatalf("startClaudeSession: %v", err)
	}
	t.Cleanup(func() { claude.Close() })
	return sess, claude
}

func controlFramesOf(shim *claudeshim.Server, subtype string) []string {
	var out []string
	for _, line := range shim.InputLines() {
		if strings.Contains(line, `"type":"control_request"`) && strings.Contains(line, `"subtype":"`+subtype+`"`) {
			out = append(out, line)
		}
	}
	return out
}

func postJSON(t *testing.T, handler func(http.ResponseWriter, *http.Request), path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return w
}

// A claude interrupt is gated on nano's own turn state, because the CLI's ack
// cannot distinguish "stopped a turn" from "there was nothing to stop": it
// answers success either way and simply emits no result line when idle. Asking
// anyway would leave the operator with a Stop that always claims to have worked.
func TestClaudeInterruptIsGatedOnTurnState(t *testing.T) {
	shim := controlResponder(t, func(string) any { return map[string]any{"still_queued": []string{}} })
	sess, claude := startControlSession(t, shim)

	res := postJSON(t, sess.delivery.HandleInterrupt, "/api/sessions/x/interrupt", "")
	var idle struct {
		Interrupted bool   `json:"interrupted"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &idle); err != nil {
		t.Fatalf("decode idle interrupt: %v (%s)", err, res.Body.String())
	}
	if idle.Interrupted || idle.Reason == "" {
		t.Fatalf("idle interrupt = %+v, want a quiet interrupted:false with a reason", idle)
	}
	if frames := controlFramesOf(shim, claudeSubtypeInterrupt); len(frames) != 0 {
		t.Fatalf("idle interrupt still hit the wire: %#v", frames)
	}

	if err := claude.SendTurn("hello"); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	sess.wire.recordWireLive(protocol.Recv, []byte(`{"type":"assistant","message":{"role":"assistant"}}`), true)
	res = postJSON(t, sess.delivery.HandleInterrupt, "/api/sessions/x/interrupt", "")
	if res.Code != http.StatusOK {
		t.Fatalf("live interrupt = %d %s", res.Code, res.Body.String())
	}
	var live struct {
		Interrupted bool `json:"interrupted"`
	}
	json.Unmarshal(res.Body.Bytes(), &live)
	if !live.Interrupted {
		t.Fatalf("live interrupt = %s, want interrupted:true", res.Body.String())
	}
	frames := controlFramesOf(shim, claudeSubtypeInterrupt)
	if len(frames) != 1 {
		t.Fatalf("interrupt frames = %#v, want exactly one", frames)
	}
	// Wire evidence discipline: the control channel is protocol traffic and has
	// to show up in the Event Feed like every other line.
	if !claudeWireHas(sess, `"subtype":"interrupt"`) {
		t.Fatal("the interrupt frame never reached the session's wire buffer")
	}
}

func TestClaudeInteractionResponseUsesProviderAdapter(t *testing.T) {
	shim := controlResponder(t, func(string) any { return map[string]any{} })
	sess, _ := startControlSession(t, shim)
	sess.delivery.addPendingInteraction(interact.PendingInteraction{
		ID: "permission-1", Kind: kindToolPermission, ThreadID: sess.stateOwner.ThreadID(),
		Params: json.RawMessage(`{"input":{"command":"pwd"}}`), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})

	res := httptest.NewRecorder()
	sess.delivery.HandleInteraction(res, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interactions/permission-1", strings.NewReader(`{"decision":"accept"}`)), "permission-1")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"answered":true`) {
		t.Fatalf("interaction response = %d %s", res.Code, res.Body.String())
	}
	if !claudeWireHas(sess, `"type":"control_response"`) || !claudeWireHas(sess, `"behavior":"allow"`) {
		t.Fatal("provider adapter did not write the translated Claude control response")
	}
}

func claudeWireHas(sess *Session, needle string) bool {
	for _, evt := range claudeWireEvents(sess) {
		if strings.Contains(string(evt.Msg), needle) {
			return true
		}
	}
	return false
}

// Re-attach rehydration, and the discrepancy that forced its design.
//
// The plan called for the initialize response's pending_* arrays to be the
// authoritative source. Probed live 2026-07-31 they are simply absent in `-p`
// stdio mode even with a request genuinely open, so a dialog raised before a
// nano restart would be lost and its turn would hang. The spool is the
// substitute: it records both directions in full from seq 0 under a
// single-attached-client rule, so "replayed request with no replayed response"
// is complete evidence that nobody ever answered it.
//
// The two halves of the contract: an ANSWERED request stays gone (or a
// resolved dialog would resurrect on every re-attach), and an UNANSWERED one
// comes back.
func TestReattachRecoversOnlyUnansweredControlRequests(t *testing.T) {
	const answered = `{"type":"control_request","request_id":"old-answered","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`
	const answerLine = `{"type":"control_response","response":{"subtype":"success","request_id":"old-answered","response":{"behavior":"allow"}}}`
	const orphan = `{"type":"control_request","request_id":"old-orphan","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Go on?"}]}}}`

	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		attach, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(attach, map[string]any{"shimSessionId": "shim-1", "nativeSessionId": "native-1", "state": "running"})
		c.Line(1, "recv", answered)
		c.Line(2, "send", answerLine) // a previous nano's answer, spooled like any stdin line
		c.Line(3, "recv", orphan)
		c.Line(4, "recv", claudeInitLine)
		c.Notify(shimMethodReplayDone, map[string]any{"shimSessionId": "shim-1", "lastSeq": 4})
		// The post-replay initialize: this CLI reports no pending_* arrays, which
		// is exactly the observed behavior the spool fallback exists for.
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			c.Reply(call, nil)
			var p struct {
				Line string `json:"line"`
			}
			var frame struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
			}
			if json.Unmarshal(call.Params, &p) == nil && json.Unmarshal([]byte(p.Line), &frame) == nil &&
				frame.Type == claudeControlRequestType {
				body, _ := json.Marshal(map[string]any{
					"type": claudeControlResponseType,
					"response": map[string]any{
						"subtype": claudeControlSuccess, "request_id": frame.RequestID, "response": map[string]any{},
					},
				})
				c.Line(5, "recv", string(body))
			}
		}
	})

	sess, _ := claudeTestSession(t)
	claude, err := attachClaudeSession(sess, shim.WSURL(), "native-1", claudeReplayPlan{}, claudeSpawnPosture{Bin: "claude"}, hostedstore.Attachment{})
	if err != nil {
		t.Fatalf("attachClaudeSession: %v", err)
	}
	defer claude.Close()

	waitForClaude(t, "the orphaned request to be recovered", func() bool {
		return len(sess.delivery.PendingInteractionList()) > 0
	})
	pending := sess.delivery.PendingInteractionList()
	if len(pending) != 1 || pending[0].ID != "old-orphan" {
		t.Fatalf("pending = %+v, want only the request nothing ever answered", pending)
	}
	if pending[0].Kind != kindUserInput {
		t.Fatalf("kind = %q, want %q", pending[0].Kind, kindUserInput)
	}
}

// The initialize response is the authoritative re-attach primitive: whatever it
// reports as still pending gets re-parked, whether or not the spool replayed it.
func TestInitializeRehydratesPendingRequests(t *testing.T) {
	shim := controlResponder(t, func(subtype string) any {
		if subtype != claudeSubtypeInitialize {
			return map[string]any{}
		}
		return map[string]any{
			"models": []any{},
			"pending_permission_requests": []any{
				map[string]any{
					"request_id": "resumed-1",
					"request": map[string]any{
						"subtype": "can_use_tool", "tool_name": "AskUserQuestion",
						"input": map[string]any{"questions": []any{map[string]any{"question": "Go on?"}}},
					},
				},
			},
		}
	})
	sess, _ := startControlSession(t, shim)

	waitForClaude(t, "the rehydrated request to park", func() bool {
		return len(sess.delivery.PendingInteractionList()) > 0
	})
	pending := sess.delivery.PendingInteractionList()
	if len(pending) != 1 || pending[0].ID != "resumed-1" || pending[0].Kind != kindUserInput {
		t.Fatalf("pending = %+v, want the rehydrated question", pending)
	}
}

// Claude's model switch is a live-session control request, not a per-turn
// override: nothing is recorded until the CLI has accepted it, so a rejected id
// cannot leave the pane's control claiming a model the session is not running.
func TestClaudeSetModelRecordsOnlyWhatTheWireAccepted(t *testing.T) {
	shim := controlResponder(t, func(string) any { return map[string]any{} })
	sess, _ := startControlSession(t, shim)

	res := postJSON(t, sess.view.HandleTurnOptionsUpdate, "/api/sessions/x/turn-options", `{"model":"haiku"}`)
	if res.Code != http.StatusOK {
		t.Fatalf("set model = %d %s", res.Code, res.Body.String())
	}
	if model, _, _ := sess.stateOwner.turnOptions(); model != "haiku" {
		t.Fatalf("turnModel = %q, want haiku", model)
	}
	frames := controlFramesOf(shim, claudeSubtypeSetModel)
	if len(frames) != 1 || !strings.Contains(frames[0], `"model":"haiku"`) {
		t.Fatalf("set_model frames = %#v", frames)
	}

	// Clearing the override sends the CLI's own reset token rather than an empty
	// string, which it would not recognize.
	postJSON(t, sess.view.HandleTurnOptionsUpdate, "/api/sessions/x/turn-options", `{"model":""}`)
	frames = controlFramesOf(shim, claudeSubtypeSetModel)
	if len(frames) != 2 || !strings.Contains(frames[1], `"model":"`+claudeSetModelDefault+`"`) {
		t.Fatalf("reset frame = %#v", frames)
	}
}

// The regression this pins cost a live session: the shim only sends
// session/replayDone to an ATTACHING client, so a freshly STARTED session sat
// in "replaying" forever and every inbound control request — every dialog the
// child ever raised — was dropped as stale.
func TestStartedSessionParksInboundRequests(t *testing.T) {
	const ask = `{"type":"control_request","request_id":"live-1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Go on?"}]}}}`

	var pushed bool
	shim := newFakeShim(t, func(c *claudeshim.Conn) {
		start, ok := c.Next()
		if !ok {
			return
		}
		c.Reply(start, map[string]any{"shimSessionId": "shim-1"})
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			c.Reply(call, nil)
			if !pushed {
				pushed = true
				c.Line(1, "recv", ask)
			}
		}
	})
	sess, _ := startControlSession(t, shim)

	waitForClaude(t, "the child's question to park", func() bool {
		return len(sess.delivery.PendingInteractionList()) > 0
	})
	if pending := sess.delivery.PendingInteractionList(); pending[0].Kind != kindUserInput {
		t.Fatalf("pending = %+v, want the question parked", pending)
	}
}
