package sessions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/wire"
)

// TestConnectionLevelRequestsAreNotDecisions is the bug this slice exists to
// fix on the safety side: nano used to answer every server request, including
// an auth-token refresh, with {"decision":"denied"} — a body that satisfies no
// schema on the connection-level arms and could confuse the daemon's own auth
// handling.
func TestConnectionLevelRequestsAreNotDecisions(t *testing.T) {
	for _, method := range []string{methodAuthTokenRefresh, methodAttestationGenerate} {
		t.Run(method, func(t *testing.T) {
			result, rpcErr, handled := connectionLevelResponse(method)
			if !handled {
				t.Fatal("connection-level request fell through to the dialog path")
			}
			if rpcErr == nil {
				t.Fatal("want a method-level refusal, not a result")
			}
			if result != nil {
				t.Fatalf("want no result body, got %v", result)
			}
		})
	}
	// The dynamic-tool arm is thread-scoped but not a decision either: it
	// answers in its own response vocabulary (DynamicToolCallResponse).
	result, rpcErr, handled := connectionLevelResponse(methodDynamicToolCall)
	if !handled || rpcErr != nil {
		t.Fatalf("dynamic tool call: handled=%v err=%v", handled, rpcErr)
	}
	body, _ := json.Marshal(result)
	if !strings.Contains(string(body), `"success":false`) || strings.Contains(string(body), "decision") {
		t.Fatalf("dynamic tool response = %s", body)
	}
	if _, _, handled := connectionLevelResponse(methodCommandApproval); handled {
		t.Fatal("a real approval must not be answered on the read loop")
	}
}

func TestInteractionDecisionResultShapes(t *testing.T) {
	cases := []struct {
		method, decision, want string
	}{
		{methodCommandApproval, decisionAccept, `{"decision":"accept"}`},
		{methodCommandApproval, decisionAcceptForSession, `{"decision":"acceptForSession"}`},
		{methodCommandApproval, decisionDecline, `{"decision":"decline"}`},
		{methodCommandApproval, decisionCancel, `{"decision":"cancel"}`},
		{methodFileChangeApproval, decisionDecline, `{"decision":"decline"}`},
		// The legacy pair still speaks ReviewDecision, a different vocabulary.
		{methodLegacyExecApproval, decisionAccept, `{"decision":"approved"}`},
		{methodLegacyExecApproval, decisionAcceptForSession, `{"decision":"approved_for_session"}`},
		{methodLegacyExecApproval, decisionDecline, `{"decision":"denied"}`},
		{methodLegacyExecApproval, decisionCancel, `{"decision":"abort"}`},
		{methodLegacyPatchApproval, decisionDecline, `{"decision":"denied"}`},
		{methodElicitation, decisionDecline, `{"action":"decline"}`},
		{methodElicitation, decisionCancel, `{"action":"cancel"}`},
	}
	for _, tc := range cases {
		t.Run(tc.method+"/"+tc.decision, func(t *testing.T) {
			result, rpcErr := interactionDecisionResult(tc.method, tc.decision, nil, nil)
			if rpcErr != nil {
				t.Fatalf("unexpected error %v", rpcErr)
			}
			got, _ := json.Marshal(result)
			if string(got) != tc.want {
				t.Fatalf("= %s, want %s", got, tc.want)
			}
		})
	}
	if _, rpcErr := interactionDecisionResult(methodCommandApproval, "reject", nil, nil); rpcErr == nil {
		t.Fatal("the retired M1 verb must not be accepted as a decision")
	}
}

// TestPermissionsApprovalGrantsTheRequestedProfile: this arm has no decision
// verb at all — the response IS the granted profile, so "yes" echoes what was
// asked for and "no" grants an empty one.
func TestPermissionsApprovalGrantsTheRequestedProfile(t *testing.T) {
	params := json.RawMessage(`{"threadId":"t1","permissions":{"network":{"enabled":true}}}`)

	granted, rpcErr := interactionDecisionResult(methodPermissionsApproval, decisionAccept, params, nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	body, _ := json.Marshal(granted)
	if string(body) != `{"permissions":{"network":{"enabled":true}},"scope":"turn"}` {
		t.Fatalf("accept = %s", body)
	}
	session, _ := interactionDecisionResult(methodPermissionsApproval, decisionAcceptForSession, params, nil)
	if body, _ := json.Marshal(session); !strings.Contains(string(body), `"scope":"session"`) {
		t.Fatalf("acceptForSession = %s", body)
	}
	declined, _ := interactionDecisionResult(methodPermissionsApproval, decisionDecline, params, nil)
	if body, _ := json.Marshal(declined); string(body) != `{"permissions":{},"scope":"turn"}` {
		t.Fatalf("decline = %s", body)
	}
}

// TestRequestUserInputAnswerShape: ToolRequestUserInputResponse is
// {answers:{questionId:{answers:[…]}}} — not a decision verb. A declined
// prompt still has to satisfy that shape, per question.
func TestRequestUserInputAnswerShape(t *testing.T) {
	params := json.RawMessage(`{"threadId":"t1","turnId":"u1","itemId":"call_1","questions":[{"id":"q1","header":"Style","question":"tabs or spaces?"}]}`)

	accepted, rpcErr := interactionDecisionResult(methodRequestUserInput, decisionAccept, params, json.RawMessage(`{"q1":["spaces"]}`))
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if body, _ := json.Marshal(accepted); string(body) != `{"answers":{"q1":{"answers":["spaces"]}}}` {
		t.Fatalf("accept = %s", body)
	}
	// The already-wrapped form the schema itself uses must round-trip too.
	wrapped, _ := interactionDecisionResult(methodRequestUserInput, decisionAccept, params, json.RawMessage(`{"q1":{"answers":["tabs"]}}`))
	if body, _ := json.Marshal(wrapped); string(body) != `{"answers":{"q1":{"answers":["tabs"]}}}` {
		t.Fatalf("wrapped accept = %s", body)
	}
	declined, _ := interactionDecisionResult(methodRequestUserInput, decisionDecline, params, nil)
	if body, _ := json.Marshal(declined); string(body) != `{"answers":{"q1":{"answers":[]}}}` {
		t.Fatalf("decline = %s", body)
	}
	if _, rpcErr := interactionDecisionResult(methodRequestUserInput, decisionAccept, params, nil); rpcErr == nil {
		t.Fatal("accepting a question with no answers must fail rather than send an empty one")
	}
}

// TestRouteInteractionLegacyPair: the legacy approvals carry conversationId,
// not threadId. Missing that is why nano never routed them to a session.
func TestRouteInteractionLegacyPair(t *testing.T) {
	legacy := routeInteraction(json.RawMessage(`{"conversationId":"thread_a","callId":"call_9","command":["ls"]}`))
	if legacy.threadID != "thread_a" || legacy.itemID != "call_9" {
		t.Fatalf("legacy routing = %+v", legacy)
	}
	if got := threadIDFromParams(json.RawMessage(`{"conversationId":"thread_a"}`)); got != "thread_a" {
		t.Fatalf("threadIDFromParams(conversationId) = %q — the wire pair would not reach the session buffer", got)
	}
	modern := routeInteraction(json.RawMessage(`{"threadId":"thread_b","turnId":"turn_1","itemId":"call_2","autoResolutionMs":60000}`))
	if modern.threadID != "thread_b" || modern.turnID != "turn_1" || modern.itemID != "call_2" {
		t.Fatalf("modern routing = %+v", modern)
	}
	if modern.expires == "" {
		t.Fatal("autoResolutionMs must produce a deadline; it is the only request kind with one")
	}
	if routeInteraction(json.RawMessage(`{"reason":"expired"}`)).threadID != "" {
		t.Fatal("a connection-level request must not resolve to a thread")
	}
}

// TestPendingInteractionsAreServerOwned proves the snapshot/claim contract:
// /state carries the truth (a late tab cannot replay a bounded wire buffer),
// and claiming is single-shot so a second tab's answer collapses rather than
// double-responding.
func TestPendingInteractionsAreServerOwned(t *testing.T) {
	sess := &Session{provider: "codex",
		subs:           map[chan wire.Event]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	p := interact.PendingInteraction{ID: "7", Method: methodCommandApproval, Kind: kindCommandApproval, CreatedAt: "2026-07-30T10:00:00Z"}
	sess.addPendingInteraction(p)

	if list := sess.PendingInteractionList(); len(list) != 1 || list[0].ID != "7" {
		t.Fatalf("snapshot = %+v", list)
	}
	// The nudge rides the session's own feed as a nano-origin notification.
	found := false
	for _, evt := range sess.events {
		var msg struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(evt.Msg, &msg) == nil && msg.Method == interactionPendingEvent {
			found = true
		}
	}
	if !found {
		t.Fatal("no pending nudge on the session feed")
	}

	if _, ok := sess.takePendingInteraction("7"); !ok {
		t.Fatal("first claim failed")
	}
	if _, ok := sess.takePendingInteraction("7"); ok {
		t.Fatal("second claim succeeded — a second tab would double-respond")
	}
	if len(sess.PendingInteractionList()) != 0 {
		t.Fatal("claimed interaction still in the snapshot")
	}
}

// TestHandleInteractionAlreadyResolved: answering something already answered is
// a 409 the card collapses on, not an error the operator has to read.
func TestHandleInteractionAlreadyResolved(t *testing.T) {
	sess := &Session{provider: "codex",
		subs:           map[chan wire.Event]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x/interactions/7", strings.NewReader(`{"decision":"accept"}`))
	sess.HandleInteraction(rec, req, "7")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleInteractionKeepsMalformedAnswerPending: a typo must not consume the
// dialog — losing it would leave the turn parked with no card to answer it.
func TestHandleInteractionKeepsMalformedAnswerPending(t *testing.T) {
	sess := &Session{provider: "codex",
		subs:           map[chan wire.Event]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.addPendingInteraction(interact.PendingInteraction{
		ID: "7", Method: methodRequestUserInput, Kind: kindUserInput,
		Params: json.RawMessage(`{"questions":[{"id":"q1"}]}`), CreatedAt: "2026-07-30T10:00:00Z",
	})
	rec := httptest.NewRecorder()
	sess.HandleInteraction(rec, httptest.NewRequest(http.MethodPost, "/x/interactions/7",
		strings.NewReader(`{"decision":"accept"}`)), "7") // accept with no answers
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(sess.PendingInteractionList()) != 1 {
		t.Fatal("a rejected answer consumed the pending interaction")
	}
}

// TestServerRequestResolvedCollapses covers the silent-collapse path nano does
// NOT drive: the app-server announces serverRequest/resolved when a request it
// sent is finished — after nano's own answer (a no-op here), and, decisively,
// when the requestUserInput autoResolutionMs timeout fires or another client
// answers. Observed live 2026-07-30.
func TestServerRequestResolvedCollapses(t *testing.T) {
	daemon := NewCodexDaemon("ws://unused", supervisedDaemonLabel, func() bool { return true })
	sess := &Session{provider: "codex",
		subs:           map[chan wire.Event]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.threadID = "thread_a"
	daemon.sessions["thread_a"] = sess
	sess.addPendingInteraction(interact.PendingInteraction{
		ID: "4", Method: methodRequestUserInput, Kind: kindUserInput, ThreadID: "thread_a",
		CreatedAt: "2026-07-30T10:00:00Z",
	})

	// The id arrives as a JSON number; the pending key is the raw id text.
	daemon.handleNotification(serverRequestResolvedMethod, json.RawMessage(`{"threadId":"thread_a","requestId":4}`))
	if len(sess.PendingInteractionList()) != 0 {
		t.Fatal("a request the app-server resolved is still offered as answerable")
	}
	// Idempotent, and inert for threads and ids it does not know.
	daemon.handleNotification(serverRequestResolvedMethod, json.RawMessage(`{"threadId":"thread_a","requestId":4}`))
	daemon.handleNotification(serverRequestResolvedMethod, json.RawMessage(`{"threadId":"nope","requestId":9}`))
	daemon.handleNotification("turn/completed", json.RawMessage(`{"threadId":"thread_a"}`))
}

// TestCloseInteractionsAnswersEverythingParked: the app-server outlives nano,
// so a request left unanswered when a session goes away holds its turn open
// with nobody able to reach it.
func TestCloseInteractionsAnswersEverythingParked(t *testing.T) {
	sess := &Session{provider: "codex",
		subs:           map[chan wire.Event]struct{}{},
		codex:          NewCodexDaemon("ws://unused", supervisedDaemonLabel, func() bool { return true }),
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.addPendingInteraction(interact.PendingInteraction{ID: "1", Method: methodCommandApproval, Kind: kindCommandApproval, CreatedAt: "a"})
	sess.addPendingInteraction(interact.PendingInteraction{ID: "2", Method: methodRequestUserInput, Kind: kindUserInput, CreatedAt: "b"})

	sess.closeInteractions()

	if got := sess.PendingInteractionList(); len(got) != 0 {
		t.Fatalf("still parked after close: %+v", got)
	}
	var resolved int
	for _, evt := range sess.events {
		var msg struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(evt.Msg, &msg) == nil && msg.Method == interactionResolvedEvent {
			resolved++
		}
	}
	if resolved != 2 {
		t.Fatalf("resolved events = %d, want 2", resolved)
	}
}
