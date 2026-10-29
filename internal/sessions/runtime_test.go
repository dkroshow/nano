package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// TestTurnActiveTransition pins the wire vocabulary that drives turnActive:
// codex turn/started sets, turn/completed clears (failure and interruption
// arrive as turn/completed per the v2 schema); claude's sent user line or
// received execution evidence sets, and the received result line clears.
func TestTurnActiveTransition(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		dir        wire.Direction
		line       string
		active, ok bool
		turnID     string
	}{
		{"codex turn started", "codex", protocol.Recv, `{"method":"turn/started","params":{"threadId":"t1","turn":{"id":"turn_1"}}}`, true, true, "turn_1"},
		{"codex turn completed", "codex", protocol.Recv, `{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"completed"}}}`, false, true, "turn_1"},
		{"codex turn completed failed", "codex", protocol.Recv, `{"method":"turn/completed","params":{"threadId":"t1","turn":{"id":"turn_1","status":"failed"}}}`, false, true, "turn_1"},
		{"codex turn started without id", "codex", protocol.Recv, `{"method":"turn/started","params":{"threadId":"t1"}}`, true, true, ""},
		{"codex item completed", "codex", protocol.Recv, `{"method":"item/completed","params":{"threadId":"t1"}}`, false, false, ""},
		{"codex sent turn/start request", "codex", protocol.Send, `{"id":1,"method":"turn/start","params":{"threadId":"t1"}}`, false, false, ""},
		{"codex request not notification", "codex", protocol.Recv, `{"id":9,"method":"turn/started","params":{"threadId":"t1"}}`, false, false, ""},
		{"codex malformed", "codex", protocol.Recv, `{"method":`, false, false, ""},
		{"claude sent user line", "claude", protocol.Send, `{"type":"user","message":{"role":"user"}}`, true, true, ""},
		{"claude received result line", "claude", protocol.Recv, `{"type":"result","subtype":"success"}`, false, true, ""},
		{"claude received task notification", "claude", protocol.Recv, `{"type":"user","origin":{"kind":"task-notification"},"message":{"role":"user"}}`, true, true, ""},
		{"claude received assistant", "claude", protocol.Recv, `{"type":"assistant","message":{"role":"assistant"}}`, true, true, ""},
		{"claude partial stream event", "claude", protocol.Recv, `{"type":"stream_event","event":{}}`, true, true, ""},
		{"claude init is inert", "claude", protocol.Recv, `{"type":"system","subtype":"init"}`, false, false, ""},
		{"unknown provider", "gemini", protocol.Recv, `{"method":"turn/started","params":{}}`, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			active, turnID, _, ok := turnActiveTransition(tc.provider, tc.dir, []byte(tc.line))
			if active != tc.active || ok != tc.ok || turnID != tc.turnID {
				t.Fatalf("turnActiveTransition = (%v, %q, %v), want (%v, %q, %v)",
					active, turnID, ok, tc.active, tc.turnID, tc.ok)
			}
		})
	}
}

// TestSessionTracksTurnID proves the id an interrupt needs is learned from
// turn/started and dropped again on turn/completed — a session with no turn in
// flight must have nothing to address, or the stop affordance could interrupt
// a turn that already ended.
func TestSessionTracksTurnID(t *testing.T) {
	sess := sessionWithOwners(&Session{provider: "codex",
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}},
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	sess.stateOwner.threadID = "thread_a"

	if _, turnID := sess.stateOwner.activeTurn(); turnID != "" {
		t.Fatalf("idle session already has turn %q", turnID)
	}
	sess.wire.recordWire(protocol.Recv, []byte(`{"method":"turn/started","params":{"threadId":"thread_a","turn":{"id":"turn_7"}}}`))
	threadID, turnID := sess.stateOwner.activeTurn()
	if threadID != "thread_a" || turnID != "turn_7" {
		t.Fatalf("activeTurn = (%q, %q), want (thread_a, turn_7)", threadID, turnID)
	}
	sess.wire.recordWire(protocol.Recv, []byte(`{"method":"turn/completed","params":{"threadId":"thread_a","turn":{"id":"turn_7","status":"completed"}}}`))
	if _, turnID := sess.stateOwner.activeTurn(); turnID != "" {
		t.Fatalf("completed turn still addressable as %q", turnID)
	}
}

// TestHandleInterruptWithoutTurn pins the quiet-degradation contract: asking to
// stop a session with no turn in flight is a 200 saying nothing was
// interrupted, not an error the operator has to read.
func TestHandleInterruptWithoutTurn(t *testing.T) {
	sess := sessionWithOwners(&Session{provider: "codex",
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}},
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	sess.stateOwner.threadID = "thread_a"

	rec := httptest.NewRecorder()
	sess.delivery.HandleInterrupt(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interrupt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Interrupted bool   `json:"interrupted"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Interrupted || body.Reason == "" {
		t.Fatalf("body = %+v, want interrupted:false with a reason", body)
	}
}

// TestInterruptRaceReason pins how a lost interrupt race is classified. The
// app-server does not answer a turn/interrupt for a turn that already ended
// (probed 2026-07-30), so the deadline — not an error object — is the shape the
// race actually takes, and it must read as a non-event.
func TestInterruptRaceReason(t *testing.T) {
	if reason, raced := interruptRaceReason(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)); !raced || reason == "" {
		t.Fatalf("deadline = (%q, %v), want a quiet reason", reason, raced)
	}
	if reason, raced := interruptRaceReason(&protocol.RPCError{Code: -32602, Message: "unknown turn"}); !raced || reason != "unknown turn" {
		t.Fatalf("rpc error = (%q, %v), want (unknown turn, true)", reason, raced)
	}
	if _, raced := interruptRaceReason(errors.New("websocket: connection closed")); raced {
		t.Fatal("a dead daemon must not be reported as a completion race")
	}
}

// TestHandleTurnOptions proves the per-turn overrides round-trip and that an
// omitted field is left alone while an explicit empty string clears it.
func TestHandleTurnOptions(t *testing.T) {
	sess := sessionWithOwners(&Session{provider: "codex",
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}},
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	sess.stateOwner.model = "gpt-5.6-sol"
	sess.stateOwner.threadID = "native-1"

	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		sess.view.HandleTurnOptionsUpdate(rec, httptest.NewRequest(http.MethodPost, "/x/turn-options", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	got := post(`{"model":"gpt-5.6-luna","effort":"high","serviceTier":"priority"}`)
	if got["model"] != "gpt-5.6-luna" || got["effort"] != "high" || got["serviceTier"] != "priority" || got["threadModel"] != "gpt-5.6-sol" {
		t.Fatalf("after set: %+v", got)
	}
	if model, effort, tier := sess.stateOwner.turnOptions(); model != "gpt-5.6-luna" || effort != "high" || tier != "priority" {
		t.Fatalf("session overrides = (%q, %q, %q)", model, effort, tier)
	}
	// Effort alone: the model override must survive an untouched field.
	if got := post(`{"effort":"low"}`); got["model"] != "gpt-5.6-luna" || got["effort"] != "low" || got["serviceTier"] != "priority" {
		t.Fatalf("after partial update: %+v", got)
	}
	// Standard is explicit, not an empty/inherit alias: a user whose config
	// defaults to priority must still be able to turn priority off.
	if got := post(`{"serviceTier":"default"}`); got["serviceTier"] != "default" {
		t.Fatalf("after standard: %+v", got)
	}
	// Explicit empty clears back to the thread's own model.
	if got := post(`{"model":""}`); got["model"] != "" || got["effort"] != "low" {
		t.Fatalf("after clear: %+v", got)
	}
	if overrides := CodexTurnOverrides(sess.stateOwner.turnModel, sess.stateOwner.turnEffort, sess.stateOwner.turnServiceTier, "detailed"); overrides["model"] != nil || overrides["serviceTier"] != "default" {
		t.Fatalf("cleared model still rides the wire: %+v", overrides)
	}
}

// TestSessionTurnActiveFromWire proves recordWire maintains turnActive and
// that only real flips notify the runtime stream — a repeated turn/started is
// silent, and exit clears a stale in-flight flag.
func TestSessionTurnActiveFromWire(t *testing.T) {
	var nudges int
	sess := sessionWithOwners(&Session{provider: "codex",
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}},
		stateOwner: State{
			runtimeNotify:  func() { nudges++ },
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	started := []byte(`{"method":"turn/started","params":{"threadId":"t1"}}`)
	completed := []byte(`{"method":"turn/completed","params":{"threadId":"t1"}}`)

	sess.wire.recordWire(protocol.Recv, started)
	if sum := sess.view.Summary(); !sum.TurnActive || nudges != 1 {
		t.Fatalf("after turn/started: turnActive = %v nudges = %d", sum.TurnActive, nudges)
	}
	sess.wire.recordWire(protocol.Recv, started)
	if nudges != 1 {
		t.Fatalf("repeated turn/started nudged again: nudges = %d", nudges)
	}
	sess.wire.recordWire(protocol.Recv, completed)
	if sum := sess.view.Summary(); sum.TurnActive || sum.LastTurnEndedAt == "" || nudges != 2 {
		t.Fatalf("after turn/completed: turnActive = %v nudges = %d", sum.TurnActive, nudges)
	}

	sess.wire.recordWire(protocol.Recv, started)
	sess.setStatus("exited")
	if sum := sess.view.Summary(); sum.TurnActive {
		t.Fatal("exit left turnActive set")
	}
}

// A shim-replayed turn close reconstructs state but the turn did not end NOW:
// stamping arrival time rewrote every reattached thread's "last turn ended"
// to the deploy moment on each nano restart (2026-08-06, 14 threads sharing
// one restart minute). Replay flips the flag; only a live close stamps.
func TestReplayedTurnCloseDoesNotStampLastTurnEnded(t *testing.T) {
	sess := sessionWithOwners(&Session{provider: "codex",
		wire: Wire{
			subs: map[chan wire.Event]struct{}{}},
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	started := []byte(`{"method":"turn/started","params":{"threadId":"t1"}}`)
	completed := []byte(`{"method":"turn/completed","params":{"threadId":"t1"}}`)

	sess.wire.recordWireLive(protocol.Recv, started, false)
	sess.wire.recordWireLive(protocol.Recv, completed, false)
	sum := sess.view.Summary()
	if sum.TurnActive {
		t.Fatal("replayed close left turnActive set — state must still reconstruct")
	}
	if sum.LastTurnEndedAt != "" {
		t.Fatalf("replayed close stamped lastTurnEndedAt = %q, want absent (the turn ended before this process existed)", sum.LastTurnEndedAt)
	}

	sess.wire.recordWireLive(protocol.Recv, started, true)
	sess.wire.recordWireLive(protocol.Recv, completed, true)
	if sum := sess.view.Summary(); sum.LastTurnEndedAt == "" {
		t.Fatal("live close no longer stamps lastTurnEndedAt")
	}
}

func TestVolatileSessionSummariesDoesNotHoldServerLockAcrossSessionState(t *testing.T) {
	sess := sessionWithOwners(&Session{id: "blocked-summary",
		stateOwner: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	s := &Registry{sessions: map[string]*Session{sess.id: sess}}
	sess.stateOwner.stateMu.Lock()
	done := make(chan struct{})
	go func() {
		s.VolatileSummaries(nil)
		close(done)
	}()

	locked := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(250 * time.Millisecond):
		sess.stateOwner.stateMu.Unlock()
		t.Fatal("server lock stayed held while session summary was blocked")
	}
	sess.stateOwner.stateMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("live summaries did not finish after session summary unblocked")
	}
}

// A Nano restart replays Claude's complete shim spool before following live
// output. Historical prompt/result edges must rebuild the final state without
// making the sidebar promote and demote the same row for every old turn.
func TestClaudeReplayPublishesOnlySettledTurnState(t *testing.T) {
	nudges := 0
	sess := sessionWithOwners(&Session{provider: "claude",
		stateOwner: State{
			state:          map[string]json.RawMessage{},
			runtimeNotify:  func() { nudges++ },
			lifecycleState: testLifecycleSnapshot("ready", false)},
		view: View{
			projection: newDeliveryTestProjection("claude")}})
	sess.stateOwner.beginTurnRestore()

	user := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"old prompt"}]}}`)
	result := []byte(`{"type":"result","subtype":"success"}`)
	sess.wire.recordWireLive(protocol.Send, user, false)
	if sess.view.Summary().TurnActive || nudges != 0 {
		t.Fatalf("replayed prompt = publicly active %v, nudges %d; want private reconstruction", sess.view.Summary().TurnActive, nudges)
	}
	sess.wire.recordWireLive(protocol.Recv, result, false)
	if sess.view.Summary().TurnActive || nudges != 0 {
		t.Fatalf("replayed result = active %v, nudges %d; want reconstructed but unpublished", sess.view.Summary().TurnActive, nudges)
	}
	// A second historical turn proves this is not accidental one-slot SSE
	// conflation: no intermediate edge reaches the runtime hub at all.
	sess.wire.recordWireLive(protocol.Send, user, false)
	sess.wire.recordWireLive(protocol.Recv, result, false)
	if nudges != 0 {
		t.Fatalf("historical turn sequence published %d runtime nudges, want 0", nudges)
	}

	sess.stateOwner.settleRestoredTurnState()
	if sess.view.Summary().TurnActive || nudges != 1 {
		t.Fatalf("settled replay = active %v, nudges %d; want one idle publication", sess.view.Summary().TurnActive, nudges)
	}

	activeNudges := 0
	active := sessionWithOwners(&Session{provider: "claude",
		stateOwner: State{
			state:          map[string]json.RawMessage{},
			runtimeNotify:  func() { activeNudges++ },
			lifecycleState: testLifecycleSnapshot("ready", false)},
		view: View{
			projection: newDeliveryTestProjection("claude")}})
	active.stateOwner.beginTurnRestore()
	active.wire.recordWireLive(protocol.Send, user, false)
	if active.view.Summary().TurnActive || activeNudges != 0 {
		t.Fatalf("open replayed turn = publicly active %v, nudges %d; want private reconstruction", active.view.Summary().TurnActive, activeNudges)
	}
	active.stateOwner.settleRestoredTurnState()
	if !active.view.Summary().TurnActive || activeNudges != 1 {
		t.Fatalf("settled active replay = active %v, nudges %d; want one active publication", active.view.Summary().TurnActive, activeNudges)
	}
}

func TestClaudeRestoreDefersGenuineLiveCompletionUntilSettlement(t *testing.T) {
	nudges := 0
	sess := sessionWithOwners(&Session{provider: "claude",
		stateOwner: State{
			state:          map[string]json.RawMessage{},
			runtimeNotify:  func() { nudges++ },
			lifecycleState: testLifecycleSnapshot("starting", false)},
		view: View{
			projection: newDeliveryTestProjection("claude")}})
	sess.stateOwner.beginTurnRestore()
	sess.stateOwner.setTurnActive(true, "live-during-restore", 100, true)
	sess.stateOwner.setTurnActive(false, "", 0, true)
	if nudges != 0 {
		t.Fatalf("pre-settlement runtime = nudges %d; want deferred", nudges)
	}
	sess.stateOwner.settleRestoredTurnState()
	if nudges != 1 || sess.view.Summary().TurnActive {
		t.Fatalf("settled runtime = active %v, nudges %d; want one idle publication", sess.view.Summary().TurnActive, nudges)
	}
}
