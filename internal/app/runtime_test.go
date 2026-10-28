package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
		dir        protocol.Direction
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

func TestVolatileSessionSummariesDoesNotHoldServerLockAcrossSessionState(t *testing.T) {
	sess := &session{id: "blocked-summary",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{sess.id: sess}}}
	sess.stateMu.Lock()
	done := make(chan struct{})
	go func() {
		s.volatileSessionSummaries()
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
		sess.stateMu.Unlock()
		t.Fatal("server lock stayed held while session summary was blocked")
	}
	sess.stateMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("live summaries did not finish after session summary unblocked")
	}
}

// A harness notification can wake Claude without Nano sending a user line.
// The first provider execution line must open a fresh runtime/projection turn,
// and the ordinary result line remains the single terminal authority.
func TestClaudeProviderExecutionEvidenceTracksNotificationStartedTurn(t *testing.T) {
	sess := &session{provider: "claude",
		subs:           map[chan wireEvent]struct{}{},
		projection:     newLiveProjection("claude"),
		lifecycleState: testLifecycleSnapshot("ready", false)}

	sess.observeClaudeShimEvent(claudeShimEvent{Kind: claudeShimAccepted, ProviderTurnID: "claude:notification:1"})
	sess.recordWire(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"notification-turn"}}}`))
	if sum := sess.summary(); !sum.TurnActive || sum.TurnStartedAtMs == 0 {
		t.Fatalf("execution evidence summary = %+v, want an anchored active turn", sum)
	}
	snapshot := projectionStateForTest(sess.projection)
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Status != "in_progress" || !snapshot.Runtime.TurnActive {
		t.Fatalf("execution evidence projection = turns %+v runtime %+v", snapshot.Turns, snapshot.Runtime)
	}

	sess.recordWire(protocol.Recv, []byte(`{"type":"result","subtype":"success"}`))
	if sum := sess.summary(); sum.TurnActive {
		t.Fatalf("terminal result left notification-started turn active: %+v", sum)
	}
}

// TestSessionTracksTurnID proves the id an interrupt needs is learned from
// turn/started and dropped again on turn/completed — a session with no turn in
// flight must have nothing to address, or the stop affordance could interrupt
// a turn that already ended.
func TestSessionTracksTurnID(t *testing.T) {
	sess := &session{provider: "codex",
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.threadID = "thread_a"

	if _, turnID := sess.activeTurn(); turnID != "" {
		t.Fatalf("idle session already has turn %q", turnID)
	}
	sess.recordWire(protocol.Recv, []byte(`{"method":"turn/started","params":{"threadId":"thread_a","turn":{"id":"turn_7"}}}`))
	threadID, turnID := sess.activeTurn()
	if threadID != "thread_a" || turnID != "turn_7" {
		t.Fatalf("activeTurn = (%q, %q), want (thread_a, turn_7)", threadID, turnID)
	}
	sess.recordWire(protocol.Recv, []byte(`{"method":"turn/completed","params":{"threadId":"thread_a","turn":{"id":"turn_7","status":"completed"}}}`))
	if _, turnID := sess.activeTurn(); turnID != "" {
		t.Fatalf("completed turn still addressable as %q", turnID)
	}
}

// TestHandleInterruptWithoutTurn pins the quiet-degradation contract: asking to
// stop a session with no turn in flight is a 200 saying nothing was
// interrupted, not an error the operator has to read.
func TestHandleInterruptWithoutTurn(t *testing.T) {
	sess := &session{provider: "codex",
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.threadID = "thread_a"

	rec := httptest.NewRecorder()
	sess.handleInterrupt(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/x/interrupt", nil))
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
	sess := &session{provider: "codex",
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.model = "gpt-5.6-sol"
	sess.threadID = "native-1"

	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		sess.handleTurnOptionsUpdate(rec, httptest.NewRequest(http.MethodPost, "/x/turn-options", strings.NewReader(body)))
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
	if model, effort, tier := sess.turnOptions(); model != "gpt-5.6-luna" || effort != "high" || tier != "priority" {
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
	if overrides := codexTurnOverrides(sess.turnModel, sess.turnEffort, sess.turnServiceTier, "detailed"); overrides["model"] != nil || overrides["serviceTier"] != "default" {
		t.Fatalf("cleared model still rides the wire: %+v", overrides)
	}
}

// TestSessionTurnActiveFromWire proves recordWire maintains turnActive and
// that only real flips notify the runtime stream — a repeated turn/started is
// silent, and exit clears a stale in-flight flag.
func TestSessionTurnActiveFromWire(t *testing.T) {
	var nudges int
	sess := &session{provider: "codex",
		subs:           map[chan wireEvent]struct{}{},
		runtimeNotify:  func() { nudges++ },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	started := []byte(`{"method":"turn/started","params":{"threadId":"t1"}}`)
	completed := []byte(`{"method":"turn/completed","params":{"threadId":"t1"}}`)

	sess.recordWire(protocol.Recv, started)
	if sum := sess.summary(); !sum.TurnActive || nudges != 1 {
		t.Fatalf("after turn/started: turnActive = %v nudges = %d", sum.TurnActive, nudges)
	}
	sess.recordWire(protocol.Recv, started)
	if nudges != 1 {
		t.Fatalf("repeated turn/started nudged again: nudges = %d", nudges)
	}
	sess.recordWire(protocol.Recv, completed)
	if sum := sess.summary(); sum.TurnActive || sum.LastTurnEndedAt == "" || nudges != 2 {
		t.Fatalf("after turn/completed: turnActive = %v nudges = %d", sum.TurnActive, nudges)
	}

	sess.recordWire(protocol.Recv, started)
	sess.setStatus("exited")
	if sum := sess.summary(); sum.TurnActive {
		t.Fatal("exit left turnActive set")
	}
}

// A Nano restart replays Claude's complete shim spool before following live
// output. Historical prompt/result edges must rebuild the final state without
// making the sidebar promote and demote the same row for every old turn.
func TestClaudeReplayPublishesOnlySettledTurnState(t *testing.T) {
	nudges := 0
	sess := &session{provider: "claude",
		state:          map[string]json.RawMessage{},
		projection:     newLiveProjection("claude"),
		runtimeNotify:  func() { nudges++ },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	sess.beginTurnRestore()

	user := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"old prompt"}]}}`)
	result := []byte(`{"type":"result","subtype":"success"}`)
	sess.recordWireLive(protocol.Send, user, false)
	if sess.summary().TurnActive || nudges != 0 {
		t.Fatalf("replayed prompt = publicly active %v, nudges %d; want private reconstruction", sess.summary().TurnActive, nudges)
	}
	sess.recordWireLive(protocol.Recv, result, false)
	if sess.summary().TurnActive || nudges != 0 {
		t.Fatalf("replayed result = active %v, nudges %d; want reconstructed but unpublished", sess.summary().TurnActive, nudges)
	}
	// A second historical turn proves this is not accidental one-slot SSE
	// conflation: no intermediate edge reaches the runtime hub at all.
	sess.recordWireLive(protocol.Send, user, false)
	sess.recordWireLive(protocol.Recv, result, false)
	if nudges != 0 {
		t.Fatalf("historical turn sequence published %d runtime nudges, want 0", nudges)
	}

	sess.settleRestoredTurnState()
	if sess.summary().TurnActive || nudges != 1 {
		t.Fatalf("settled replay = active %v, nudges %d; want one idle publication", sess.summary().TurnActive, nudges)
	}

	activeNudges := 0
	active := &session{provider: "claude",
		state:          map[string]json.RawMessage{},
		projection:     newLiveProjection("claude"),
		runtimeNotify:  func() { activeNudges++ },
		lifecycleState: testLifecycleSnapshot("ready", false)}
	active.beginTurnRestore()
	active.recordWireLive(protocol.Send, user, false)
	if active.summary().TurnActive || activeNudges != 0 {
		t.Fatalf("open replayed turn = publicly active %v, nudges %d; want private reconstruction", active.summary().TurnActive, activeNudges)
	}
	active.settleRestoredTurnState()
	if !active.summary().TurnActive || activeNudges != 1 {
		t.Fatalf("settled active replay = active %v, nudges %d; want one active publication", active.summary().TurnActive, activeNudges)
	}
}

func TestClaudeRestoreDefersGenuineLiveCompletionUntilSettlement(t *testing.T) {
	nudges := 0
	sess := &session{provider: "claude",
		state:          map[string]json.RawMessage{},
		projection:     newLiveProjection("claude"),
		runtimeNotify:  func() { nudges++ },
		lifecycleState: testLifecycleSnapshot("starting", false)}
	sess.beginTurnRestore()
	sess.setTurnActive(true, "live-during-restore", 100, true)
	sess.setTurnActive(false, "", 0, true)
	if nudges != 0 {
		t.Fatalf("pre-settlement runtime = nudges %d; want deferred", nudges)
	}
	sess.settleRestoredTurnState()
	if nudges != 1 || sess.summary().TurnActive {
		t.Fatalf("settled runtime = active %v, nudges %d; want one idle publication", sess.summary().TurnActive, nudges)
	}
}

// A shim-replayed turn close reconstructs state but the turn did not end NOW:
// stamping arrival time rewrote every reattached thread's "last turn ended"
// to the deploy moment on each nano restart (2026-08-06, 14 threads sharing
// one restart minute). Replay flips the flag; only a live close stamps.
func TestReplayedTurnCloseDoesNotStampLastTurnEnded(t *testing.T) {
	sess := &session{provider: "codex",
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	started := []byte(`{"method":"turn/started","params":{"threadId":"t1"}}`)
	completed := []byte(`{"method":"turn/completed","params":{"threadId":"t1"}}`)

	sess.recordWireLive(protocol.Recv, started, false)
	sess.recordWireLive(protocol.Recv, completed, false)
	sum := sess.summary()
	if sum.TurnActive {
		t.Fatal("replayed close left turnActive set — state must still reconstruct")
	}
	if sum.LastTurnEndedAt != "" {
		t.Fatalf("replayed close stamped lastTurnEndedAt = %q, want absent (the turn ended before this process existed)", sum.LastTurnEndedAt)
	}

	sess.recordWireLive(protocol.Recv, started, true)
	sess.recordWireLive(protocol.Recv, completed, true)
	if sum := sess.summary(); sum.LastTurnEndedAt == "" {
		t.Fatal("live close no longer stamps lastTurnEndedAt")
	}
}

// TestRuntimeOverlayOnListAndDetail covers the frozen runtime overlay contract
// on both catalog responses: present with attached/sessionId/status/turnActive
// while a session matches provider+nativeId, absent when detached and when the
// runtime source is nil.
func TestRuntimeOverlayOnListAndDetail(t *testing.T) {
	fake := &fakeThreadCatalog{
		page: catalogPage{Items: []catalogThread{
			{ID: "nth_1", Provider: "codex", NativeID: "native-live", Kind: "main", UpdatedAt: strPtr("2026-07-28T12:00:01Z")},
			{ID: "nth_2", Provider: "claude", NativeID: "native-idle", Kind: "main", UpdatedAt: strPtr("2026-07-28T12:00:00Z")},
		}},
		thread: catalogThread{ID: "nth_1", Provider: "codex", NativeID: "native-live", Kind: "main"},
	}
	s := &server{catalog: fake, sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}
	s.runtime = s
	s.sessions["s1"] = &session{id: "s1",
		provider:       "codex",
		threadID:       "native-live",
		lifecycleState: testLifecycleSnapshot("ready", true)}

	assertRuntime := func(t *testing.T, raw json.RawMessage, wantTurnActive bool) {
		t.Helper()
		var runtime runtimeInfo
		if err := json.Unmarshal(raw, &runtime); err != nil {
			t.Fatal(err)
		}
		if !runtime.Attached || runtime.SessionID != "s1" || runtime.Status != "ready" || runtime.TurnActive != wantTurnActive {
			t.Fatalf("runtime = %#v", runtime)
		}
	}

	rec := httptest.NewRecorder()
	s.handleCatalogThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items = %d", len(list.Items))
	}
	assertRuntime(t, list.Items[0]["runtime"], true)
	if _, present := list.Items[1]["runtime"]; present {
		t.Fatal("detached thread carried a runtime field")
	}

	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertRuntime(t, detail["runtime"], true)

	// turnActive flips are reflected on the next response.
	s.sessions["s1"].setTurnActive(false, "", 0, true)
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	detail = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	assertRuntime(t, detail["runtime"], false)

	// Detached (session closed): the field disappears entirely.
	delete(s.sessions, "s1")
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	detail = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if _, present := detail["runtime"]; present {
		t.Fatal("closed session left a runtime field on detail")
	}

	// Nil runtime source (catalog wired without sessions): never present. The
	// fake reuses its page value across calls, so clear the stamp the earlier
	// list request left (the real service builds fresh items per call).
	s.runtime = nil
	fake.page.Items[0].Runtime = nil
	s.sessions["s1"] = &session{id: "s1",
		provider:       "codex",
		threadID:       "native-live",
		lifecycleState: testLifecycleSnapshot("ready", false)}
	rec = httptest.NewRecorder()
	s.handleCatalogThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	list.Items = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if _, present := list.Items[0]["runtime"]; present {
		t.Fatal("nil runtime source still produced a runtime field")
	}
}

// TestRuntimeForThreeStates pins the overlay's three answers per thread —
// attached (a live session hosts it), detached (no live session but a
// remembered attachment survives), and absent (neither) — provider-generic
// across codex and claude records. The exited case is the post-crash reality:
// a dead session lingering in the map must not read as attached, and must not
// hide the detached record behind it.
func TestRuntimeForThreeStates(t *testing.T) {
	attachments := newTestStore(t)
	attachments.remember(attachmentRecord{Provider: "codex", NativeThreadID: "codex-detached", Cwd: "/a", AttachedAt: "2026-07-27T10:00:00Z"})
	attachments.remember(attachmentRecord{Provider: "claude", NativeThreadID: "claude-detached", Cwd: "/b", AttachedAt: "2026-07-27T11:00:00Z"})
	attachments.remember(attachmentRecord{Provider: "claude", NativeThreadID: "claude-exited", Cwd: "/c", AttachedAt: "2026-07-27T12:00:00Z"})

	s := &server{attachments: attachments, sessionRegistry: sessionRegistry{sessions: map[string]*session{
		"s1": {id: "s1", provider: "codex", threadID: "codex-live", lifecycleState: testLifecycleSnapshot("ready", true)},
		"s2": {id: "s2", provider: "claude", threadID: "claude-live", lifecycleState: testLifecycleSnapshot("starting", false)},
		// Exited, but its attachment record is still there: detached.
		"s3": {id: "s3", provider: "claude", threadID: "claude-exited", lifecycleState: testLifecycleSnapshot("exited", false)},
		// Exited with no record at all: no overlay whatsoever.
		"s4": {id: "s4", provider: "codex", threadID: "codex-gone", lifecycleState: testLifecycleSnapshot("exited", false)},
	}}}
	s.runtime = s

	cases := []struct {
		name     string
		provider string
		nativeID string
		want     runtimeInfo
		wantOK   bool
	}{
		{"codex attached working", "codex", "codex-live", runtimeInfo{Attached: true, SessionID: "s1", Status: "ready", TurnActive: true, ThreadStatus: fleetWorking, PresentationMode: presentationShared}, true},
		{"claude attached starting", "claude", "claude-live", runtimeInfo{Attached: true, SessionID: "s2", Status: "starting", ThreadStatus: fleetIdle, PresentationMode: presentationShared}, true},
		{"codex detached", "codex", "codex-detached", runtimeInfo{Detached: true}, true},
		{"claude detached", "claude", "claude-detached", runtimeInfo{Detached: true}, true},
		{"exited session with record reads detached", "claude", "claude-exited", runtimeInfo{Detached: true}, true},
		{"exited session without record reads absent", "codex", "codex-gone", runtimeInfo{}, false},
		{"unknown thread", "codex", "never-seen", runtimeInfo{}, false},
		{"wrong provider for the record", "codex", "claude-detached", runtimeInfo{}, false},
		{"empty native id", "codex", "", runtimeInfo{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.RuntimeFor(tc.provider, tc.nativeID)
			if !reflect.DeepEqual(got, tc.want) || ok != tc.wantOK {
				t.Fatalf("RuntimeFor(%q, %q) = %#v, %v; want %#v, %v", tc.provider, tc.nativeID, got, ok, tc.want, tc.wantOK)
			}
		})
	}

	// A server without an attachment store simply has no detached state.
	bare := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}
	if info, ok := bare.RuntimeFor("codex", "codex-detached"); ok || !reflect.DeepEqual(info, runtimeInfo{}) {
		t.Fatalf("nil attachment store = %#v, %v", info, ok)
	}
}

// TestDetachedOverlayJSON pins the wire spelling of the detached shape: the
// frontend keys on `detached`, and a detached row must not carry invented
// session fields.
func TestDetachedOverlayJSON(t *testing.T) {
	attachments := newTestStore(t)
	attachments.remember(attachmentRecord{Provider: "codex", NativeThreadID: "native-1", Cwd: "/a", AttachedAt: "2026-07-27T10:00:00Z"})
	s := &server{
		catalog:         &fakeThreadCatalog{thread: catalogThread{ID: "nth_1", Provider: "codex", NativeID: "native-1", Kind: "main"}},
		attachments:     attachments,
		sessionRegistry: sessionRegistry{sessions: map[string]*session{}},
	}
	s.runtime = s

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if got := string(detail["runtime"]); got != `{"attached":false,"detached":true}` {
		t.Fatalf("detached runtime JSON = %s", got)
	}
}

// TestAttachmentDismissNudgesRuntime: dismissing a detached row retires a
// runtime overlay, so open Explorers must be told to refetch — the same nudge
// bind and unbind fire.
func TestAttachmentDismissNudgesRuntime(t *testing.T) {
	s := &server{attachments: newTestStore(t), runtimeEvents: newRuntimeHub(), sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}
	s.attachments.remember(attachmentRecord{Provider: "codex", NativeThreadID: "t-1", AttachedAt: "2026-07-27T10:00:00Z"})
	nudges := s.runtimeEvents.subscribe()

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/attachments/t-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss status = %d", rec.Code)
	}
	select {
	case <-nudges:
	default:
		t.Fatal("dismiss did not nudge the runtime stream")
	}
}

// TestRuntimeEventsOnStream drives the full chain over a live SSE connection:
// bind, turnActive flip, and unbind each emit a compact runtime delta, while
// catalog events keep their version-only payload.
func TestRuntimeEventsOnStream(t *testing.T) {
	fakeCodex := newFakeAppServer(t)
	hub := newCatalogHub()
	catalog := &fakeStreamCatalog{fakeThreadCatalog: &fakeThreadCatalog{}, hub: hub, version: 5}
	s := newResumeTestServer(t, fakeCodex.wsURL())
	s.catalog = catalog

	srv := httptest.NewServer(http.HandlerFunc(s.handleCatalogIndexEvents))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	if event, data := readSSEEvent(t, reader); event != "catalog" || data != `{"catalogVersion":5,"refresh":true}` {
		t.Fatalf("connect event = %q %q", event, data)
	}

	// Bind: spawning a resume session nudges the stream while the handshake
	// is still in flight (async spawn, status starting). The reply delay
	// separates the bind nudge from the starting→ready nudge deterministically.
	const threadID = "0199a213-abcd-7000-8000-00000000000a"
	fakeCodex.setReplyDelay("thread/resume", 200*time.Millisecond)
	rec := postResume(t, s, threadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sum sessionSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Status != "starting" {
		t.Fatalf("spawn summary status = %q, want starting", sum.Status)
	}
	if event, data := readSSEEvent(t, reader); event != "runtime" || data != `{"rows":[],"seq":1}` {
		t.Fatalf("bind event = %q %q, want a runtime delta naming no thread", event, data)
	}

	// Creation/provider metadata can publish independent runtime wakeups. Skip
	// those lossy hints and require the real ready attachment delta.
	readRuntimeUntil(t, reader, "ready", func(delta runtimeDelta) bool {
		return len(delta.Sessions) == 1 && delta.Sessions[0].ThreadID == threadID && len(delta.Rows) == 1 && delta.Rows[0].NativeID == threadID
	})
	if got := waitForSessionStatus(t, s, sum.ID, "ready"); got.ThreadID != threadID {
		t.Fatalf("ready summary = %#v", got)
	}

	// turnActive flip.
	s.mu.Lock()
	sess := s.sessions[sum.ID]
	s.mu.Unlock()
	sess.recordWire(protocol.Recv, []byte(`{"method":"turn/started","params":{"threadId":"`+threadID+`"}}`))
	readRuntimeUntil(t, reader, "turnActive", func(delta runtimeDelta) bool {
		return !delta.Refresh && len(delta.Rows) == 1 && delta.Rows[0].Runtime != nil && delta.Rows[0].Runtime.TurnActive
	})

	// An ordinary catalog update carries the row that needs repair.
	hub.Notify(catalogChange{Version: 6, ThreadIDs: []string{"nth_changed"}})
	if event, data := readSSEEvent(t, reader); event != "catalog" || data != `{"catalogVersion":6,"threadIds":["nth_changed"]}` {
		t.Fatalf("catalog event = %q %q, want named row repair", event, data)
	}

	// Unbind: closing the session nudges the stream.
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sum.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d", rec.Code)
	}
	if event, data := readSSEEvent(t, reader); event != "runtime" {
		t.Fatalf("unbind event = %q %q, want runtime delta", event, data)
	} else {
		var delta runtimeDelta
		err := json.Unmarshal([]byte(data), &delta)
		// The closed session is listed as exited until the registry drops it.
		listed := slices.Equal(delta.RemovedSessions, []string{sum.ID}) ||
			len(delta.Sessions) == 1 && delta.Sessions[0].ID == sum.ID && delta.Sessions[0].Status == sessionStatusExited
		if err != nil || delta.Refresh || !listed || len(delta.Rows) != 1 || delta.Rows[0].Runtime != nil {
			t.Fatalf("unbind delta = %q / %#v / %v", data, delta, err)
		}
	}

	// A page whose list read came just before the latest delta connects from
	// that position: its first runtime event replays what it missed, up to now,
	// instead of a refresh that would read the list again.
	at := s.runtimeEvents.position()
	query := url.Values{"runtimeGeneration": {at.Generation}, "runtimeSeq": {strconv.FormatUint(at.Seq-1, 10)}}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Body.Close()
	readRuntimeUntil(t, bufio.NewReader(replay.Body), "replay", func(delta runtimeDelta) bool {
		if delta.Refresh || delta.Seq < at.Seq {
			t.Fatalf("first runtime event after connecting from seq %d = %+v, want the replay through seq %d", at.Seq-1, delta, at.Seq)
		}
		return true
	})
}

func readRuntimeUntil(t *testing.T, reader *bufio.Reader, want string, matches func(runtimeDelta) bool) {
	t.Helper()
	for attempts := 0; attempts < 8; attempts++ {
		event, data := readSSEEvent(t, reader)
		if event != "runtime" {
			continue
		}
		var delta runtimeDelta
		if err := json.Unmarshal([]byte(data), &delta); err != nil {
			t.Fatalf("%s runtime JSON %q: %v", want, data, err)
		}
		if matches(delta) {
			return
		}
	}
	t.Fatalf("did not receive %s runtime transition after metadata-only wakeups", want)
}

// TestRuntimeHubConflatesAndNeverBlocks mirrors the catalog hub guarantees for
// the runtime hub: a never-draining subscriber cannot stall notify, repeated
// nudges conflate into the single pending slot, and unsubscribe is idempotent.
func TestRuntimeHubConflatesAndNeverBlocks(t *testing.T) {
	hub := newRuntimeHub()
	slow := hub.subscribe()

	done := make(chan struct{})
	go func() {
		hub.notify()
		hub.notify()
		hub.notify()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked on a slow subscriber")
	}
	if len(slow) != 1 {
		t.Fatalf("pending nudges = %d, want 1 (conflated)", len(slow))
	}
	first := <-slow
	hub.notify(runtimeDelta{Rows: []runtimeDeltaRow{{Provider: "codex", NativeID: "old"}}})
	hub.notify(runtimeDelta{Rows: []runtimeDeltaRow{{Provider: "codex", NativeID: "latest"}}})
	latest := <-slow
	if len(first.Rows) != 0 || len(latest.Rows) != 2 || latest.Rows[0].NativeID != "latest" || latest.Rows[1].NativeID != "old" {
		t.Fatalf("runtime deltas = first:%#v latest:%#v, want the pending rows merged", first, latest)
	}

	hub.unsubscribe(slow)
	hub.unsubscribe(slow) // idempotent, no double-close
	hub.notify()          // no subscribers, no panic

	// A nil hub on the server is inert.
	(&server{}).notifyRuntime()
}

func TestRuntimeHubPreservesPendingRefresh(t *testing.T) {
	hub := newRuntimeHub()
	slow := hub.subscribe()
	defer hub.unsubscribe(slow)

	hub.notify(runtimeDelta{Refresh: true})
	hub.notify(runtimeDelta{Rows: []runtimeDeltaRow{{Provider: "codex", NativeID: "latest"}}})
	latest := <-slow
	if !latest.Refresh || len(latest.Rows) != 1 || latest.Rows[0].NativeID != "latest" {
		t.Fatalf("conflated runtime delta = %#v, want latest rows with pending refresh", latest)
	}
}

// Failure caught (2026-10-19, served after a restart): each change event of
// the re-attach carried the whole detached list, which then held nearly every
// thread, about 4.5 MB per browser over three minutes. Boundary: the runtime
// delta. One thread re-attaching sends its own row and key, not the list.
func TestRuntimeDeltaSendsOnlyTheDetachedRowsThatChanged(t *testing.T) {
	now := time.Now()
	s := &server{attachments: newTestStore(t), sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}
	for i := range 40 {
		s.attachments.remember(attachmentRecord{Provider: "codex", NativeThreadID: fmt.Sprintf("parked-%02d", i), AttachedAt: "2026-07-27T10:00:00Z"})
	}
	s.runtimeDeltaSnapshot() // the first snapshot only records the baseline
	s.sessions["s-0"] = memorySession("s-0", "codex", "parked-00", "ready", "/w", "gpt-5.6-sol", now)
	attached := s.runtimeDeltaSnapshot()
	if attached.Refresh || len(attached.DetachedRows) != 0 || !slices.Equal(attached.RemovedDetached, []string{"codex:parked-00"}) {
		t.Fatalf("one re-attach = refresh:%v detached rows %d removed %v, want only codex:parked-00 removed", attached.Refresh, len(attached.DetachedRows), attached.RemovedDetached)
	}
	if len(attached.Sessions) != 1 || attached.Sessions[0].ID != "s-0" {
		t.Fatalf("one re-attach sessions = %+v, want s-0's row", attached.Sessions)
	}

	// A burst beyond the bound reaches a browser as one refresh marker with no
	// rows of any kind, runtime overlays included: first 33 of the parked
	// threads re-attaching together (33 sessions + 33 detached keys, only 33
	// runtime rows), then a burst of new sessions.
	s.runtimeEvents = newRuntimeHub()
	browser := s.runtimeEvents.subscribe()
	defer s.runtimeEvents.unsubscribe(browser)
	emptyRefresh := func(event runtimeDelta) bool {
		return event.Refresh && event.rebuild && !event.sidebarCurrent && event.Rows != nil &&
			len(event.Rows)+len(event.Sessions)+len(event.RemovedSessions)+len(event.DetachedRows)+len(event.RemovedDetached) == 0
	}
	for i := 1; i <= 33; i++ {
		id := fmt.Sprintf("r-%d", i)
		s.sessions[id] = memorySession(id, "codex", fmt.Sprintf("parked-%02d", i), "ready", "/w", "gpt-5.6-sol", now)
	}
	// Published by a star update (already current for its own change), the
	// re-attach still needs the sidebar rebuilt.
	s.publishRuntimeDelta(true, true)
	if reattach := <-browser; !emptyRefresh(reattach) {
		t.Fatalf("33 re-attaches = refresh:%v rows %d sessions %d removed detached %d, want an empty refresh marker",
			reattach.Refresh, len(reattach.Rows), len(reattach.Sessions), len(reattach.RemovedDetached))
	}
	for i := 1; i <= runtimeChangeLimit+1; i++ {
		id := fmt.Sprintf("s-%d", i)
		s.sessions[id] = memorySession(id, "codex", fmt.Sprintf("burst-%02d", i), "ready", "/w", "gpt-5.6-sol", now)
	}
	s.publishRuntimeDelta(false, false)
	if burst := <-browser; !emptyRefresh(burst) {
		t.Fatalf("burst event = refresh:%v rebuild:%v rows %d sessions %d, want an empty refresh marker", burst.Refresh, burst.rebuild, len(burst.Rows), len(burst.Sessions))
	}
}

// A slow subscriber's pending deltas merge; small changes that add up past
// the bound reach it as one refresh marker, not as an unbounded event.
func TestRuntimeHubBoundsAMergedBacklog(t *testing.T) {
	hub := newRuntimeHub()
	slow := hub.subscribe()
	defer hub.unsubscribe(slow)
	for i := range runtimeChangeLimit + 1 {
		hub.notify(runtimeDelta{Rows: []runtimeDeltaRow{{Provider: "codex", NativeID: fmt.Sprintf("t-%02d", i)}}})
	}
	merged := <-slow
	if !merged.Refresh || merged.Rows == nil || len(merged.Rows) != 0 {
		t.Fatalf("merged backlog = refresh:%v rows %d, want an empty refresh marker", merged.Refresh, len(merged.Rows))
	}
}

// A page reads the session list at a stream position and connects from it.
// Before, runtime events were not replayed, so every page load read the list
// again when its stream opened. A connect from a position this process still
// holds must replay exactly what came after it; one it cannot replay must
// refresh, or the page keeps a stale list with nothing to correct it.
func TestRuntimeHubReplaysFromAListReadPosition(t *testing.T) {
	hub := newRuntimeHub()
	row := func(id string) runtimeDelta {
		return runtimeDelta{Rows: []runtimeDeltaRow{{Provider: "codex", NativeID: id}}}
	}
	hub.notify(row("before-read"))
	read := hub.position()
	hub.notify(row("missed-1"))
	hub.notify(row("missed-2"))
	connect := func(from runtimePosition) (runtimeDelta, bool) {
		ch := hub.subscribeFrom(from)
		defer hub.unsubscribe(ch)
		select {
		case delta := <-ch:
			return delta, true
		default:
			return runtimeDelta{}, false
		}
	}

	replayed, ok := connect(read)
	ids := []string{}
	for _, r := range replayed.Rows {
		ids = append(ids, r.NativeID)
	}
	if !ok || replayed.Refresh || !slices.Equal(ids, []string{"missed-1", "missed-2"}) || replayed.Seq != read.Seq+2 {
		t.Fatalf("connect from the read = %v %+v, want missed-1 and missed-2 at seq %d", ok, replayed, read.Seq+2)
	}
	if delta, ok := connect(hub.position()); ok {
		t.Fatalf("connect from the current position replayed %+v, want nothing", delta)
	}
	for name, from := range map[string]runtimePosition{
		"another process": {Generation: "restarted", Seq: read.Seq},
		"no read yet":     {},
	} {
		if delta, ok := connect(from); !ok || !delta.Refresh {
			t.Fatalf("connect from %s = %v %+v, want a refresh", name, ok, delta)
		}
	}
	for i := range runtimeReplayLimit {
		hub.notify(row(fmt.Sprintf("later-%02d", i)))
	}
	if delta, ok := connect(read); !ok || !delta.Refresh {
		t.Fatalf("connect from a position older than the kept deltas = %v %+v, want a refresh", ok, delta)
	}
}

// A slow subscriber reads merged deltas; merging session changes wrongly
// would leave its page holding a session that left, or missing one that
// came back, with no list read to correct it.
func TestRuntimeHubMergesSessionChanges(t *testing.T) {
	hub := newRuntimeHub()
	slow := hub.subscribe()
	defer hub.unsubscribe(slow)

	row := func(id string) listedSession { return listedSession{sessionSummary: sessionSummary{ID: id}} }
	detached := func(native string) detachedSessionView {
		return detachedSessionView{attachmentRecord: attachmentRecord{Provider: "codex", NativeThreadID: native}}
	}
	hub.notify(runtimeDelta{
		Sessions: []listedSession{row("came-and-left")}, RemovedSessions: []string{"left-and-came"},
		DetachedRows: []detachedSessionView{detached("parked-then-attached")}, RemovedDetached: []string{"codex:attached-then-parked"},
	})
	hub.notify(runtimeDelta{
		Sessions: []listedSession{row("left-and-came")}, RemovedSessions: []string{"came-and-left"},
		DetachedRows: []detachedSessionView{detached("attached-then-parked")}, RemovedDetached: []string{"codex:parked-then-attached"},
	})
	merged := <-slow
	if len(merged.Sessions) != 1 || merged.Sessions[0].ID != "left-and-came" ||
		!slices.Equal(merged.RemovedSessions, []string{"came-and-left"}) {
		t.Fatalf("merged session changes = sessions %+v removed %v, want left-and-came present and came-and-left removed",
			merged.Sessions, merged.RemovedSessions)
	}
	if len(merged.DetachedRows) != 1 || merged.DetachedRows[0].NativeThreadID != "attached-then-parked" ||
		!slices.Equal(merged.RemovedDetached, []string{"codex:parked-then-attached"}) {
		t.Fatalf("merged detached changes = rows %+v removed %v, want attached-then-parked present and parked-then-attached removed",
			merged.DetachedRows, merged.RemovedDetached)
	}
}

// readSSEEvent reads the next named SSE event (event: X then data: Y),
// skipping heartbeats and blank separators.
func readSSEEvent(t *testing.T, reader *bufio.Reader) (event, data string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "event: ") {
			continue
		}
		event = strings.TrimPrefix(line, "event: ")
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE data line: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("event %q not followed by data, got %q", event, line)
		}
		return event, strings.TrimPrefix(line, "data: ")
	}
	t.Fatal("no SSE event within deadline")
	return "", ""
}

// A session without a native id has no runtime row, so before this rule a
// spawn that failed pre-identity reached browsers only if something else
// happened to trigger a full session-list read. Since 2026-10-19 the delta
// carries that session's list row instead of asking every browser to re-read
// every session.
func TestRuntimeDeltaCarriesSessionsWithoutRows(t *testing.T) {
	pending := &session{id: "pending", lifecycleState: testLifecycleSnapshot("starting", false)}
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}}
	s.runtimeDeltaSnapshot() // the first snapshot only records the baseline

	carries := func(delta runtimeDelta) bool {
		return !delta.Refresh && len(delta.Sessions) == 1 && delta.Sessions[0].ID == "pending"
	}
	s.sessions[pending.id] = pending
	if joined := s.runtimeDeltaSnapshot(); !carries(joined) {
		t.Fatalf("a new session without a native id = %+v, want its list row and no re-read", joined)
	}
	if same := s.runtimeDeltaSnapshot(); len(same.Sessions) != 0 || same.Refresh || len(same.DetachedRows) != 0 {
		t.Fatalf("an unchanged session set = %+v, want nothing", same)
	}
	pending.lifecycleState = testLifecycleSnapshot("ready", false)
	if changed := s.runtimeDeltaSnapshot(); !carries(changed) || changed.Sessions[0].Status != "ready" {
		t.Fatalf("a status change of a session without a native id = %+v, want its new row", changed)
	}
}

// A spawn that fails before Claude or Codex names its thread stays listed with
// its error (failSpawn's contract). Carried as a removal, the page dropped the
// row and detached its tab, losing the only place the error shows.
func TestRuntimeDeltaKeepsAFailedSpawnListed(t *testing.T) {
	pending := &session{id: "pending", lifecycleState: testLifecycleSnapshot("starting", false)}
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{"pending": pending}}}
	s.runtimeDeltaSnapshot() // the first snapshot only records the baseline

	pending.failSpawn("handshake failed")
	failed := s.runtimeDeltaSnapshot()
	if len(failed.RemovedSessions) != 0 || len(failed.Sessions) != 1 || failed.Sessions[0].Status != sessionStatusExited || failed.Sessions[0].Error != "handshake failed" {
		t.Fatalf("failed spawn delta = sessions %+v removed %v, want its exited row with the error", failed.Sessions, failed.RemovedSessions)
	}
	if again := s.runtimeDeltaSnapshot(); len(again.Sessions) != 0 || len(again.RemovedSessions) != 0 {
		t.Fatalf("an unchanged failed session = %+v, want nothing", again)
	}
}

// Publications can arrive out of order; a revision that moved back would hide
// the hint a stale pane needs to re-read its metadata.
func TestLatestRevisionNeverMovesBack(t *testing.T) {
	store := &controlMetadataStore{}
	store.publishChange("nht_a", 5, "model")
	store.publishChange("nht_a", 3, "effort")
	if got := store.latestRevision("nht_a"); got != 5 {
		t.Fatalf("latestRevision = %d, want 5", got)
	}
}

// The runtime snapshot that follows every boot registration read each detached
// thread's presentation mode under its own execution-store lock, while
// registrations hold that lock across their control-store writes. At a
// restart it waited once per waiting thread: 21 s of the boot loop's first
// 30 s. It reads them all under one lock. Boundary: the snapshot over real
// attachment and execution stores while a writer keeps taking the lock.
func TestRuntimeSnapshotWaitsForTheExecutionStoreOnce(t *testing.T) {
	s, store := bootReattachServer(t, "", "")
	const threads = 200
	for i := 0; i < threads; i++ {
		rec := bootCodexRecord(t)
		rec.NativeThreadID = fmt.Sprintf("codex-thread-%d", i)
		store.remember(rec)
	}
	seedBootAttachments(t, s)
	// Several registrations and requests take the lock in turn, as at a
	// restart, so a waiter always queues behind someone.
	stop := make(chan struct{})
	held := make(chan struct{}, 1)
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.agentExecutions.mu.Lock()
				select {
				case held <- struct{}{}:
				default:
				}
				time.Sleep(5 * time.Millisecond)
				s.agentExecutions.mu.Unlock()
			}
		}()
	}
	<-held // time the snapshot only once the store is busy
	started := time.Now()
	delta := s.runtimeDeltaSnapshot()
	took := time.Since(started)
	close(stop)
	writers.Wait()
	if len(delta.Rows) != threads {
		t.Fatalf("snapshot rows = %d, want %d", len(delta.Rows), threads)
	}
	if took > time.Second {
		t.Fatalf("snapshot of %d detached threads took %s behind a busy execution store", threads, took)
	}
}

// compactionLine is one provider line of a captured compaction, with the time
// Nano received it.
type compactionLine struct {
	at   time.Time
	line []byte
}

// compactionFixture reads testdata/compaction/<provider>.jsonl: scrubbed lines
// captured from real compactions (Codex from Nano's wire spool, Claude from
// the shim's spool), ids replaced.
func compactionFixture(t *testing.T, provider string) []compactionLine {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "compaction", provider+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []compactionLine
	for _, text := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			TS   time.Time       `json:"ts"`
			Line json.RawMessage `json:"line"`
		}
		if err := json.Unmarshal([]byte(text), &entry); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, compactionLine{at: entry.TS, line: entry.Line})
	}
	return lines
}

// While a provider compacts a turn's context, the pane's status and the
// rail's runtime both carry when it began, from the provider's own signals:
// Codex's contextCompaction item start (its startedAtMs) until the item
// completes; Claude's first "compacting" status line, kept through the
// repeats, until a status line without it. Claude's compacting is the turn's
// work, so it shows even when it comes before anything else in the turn. The
// turn's end clears it too. Boundary: Nano's wire ingress, on captured
// compaction lines.
func TestCompactingFollowsTheProvidersOwnSignals(t *testing.T) {
	codex, claude := compactionFixture(t, "codex"), compactionFixture(t, "claude")
	for _, test := range []struct {
		provider string
		start    []compactionLine // the turn's start, then compaction starting
		end      []compactionLine // compaction ending
		turnEnd  []byte
		since    int64
	}{
		{
			provider: "codex",
			start:    []compactionLine{{line: []byte(`{"method":"turn/started","params":{"threadId":"16952569-d518-579a-914e-2c422f5d5946","turn":{"id":"893574cb-2552-53b8-bbd1-667ea32ac097","status":"inProgress"}}}`)}, codex[0]},
			end:      codex[1:],
			turnEnd:  []byte(`{"method":"turn/completed","params":{"threadId":"16952569-d518-579a-914e-2c422f5d5946","turn":{"id":"893574cb-2552-53b8-bbd1-667ea32ac097","status":"interrupted"}}}`),
			since:    1791514710947,
		},
		{
			provider: "claude",
			start:    claude[:4], // requesting, then three compacting lines 30 s apart
			end:      claude[4:],
			turnEnd:  []byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000}`),
			since:    claude[1].at.UnixMilli(),
		},
	} {
		t.Run(test.provider, func(t *testing.T) {
			sess := &session{provider: test.provider, subs: map[chan wireEvent]struct{}{},
				lifecycleState: testLifecycleSnapshot("ready", false),
				projection:     newLiveProjection(test.provider, "hosted")}
			sess.threadID = "16952569-d518-579a-914e-2c422f5d5946"
			apply := func(lines []compactionLine) {
				for _, l := range lines {
					sess.recordWireAt(protocol.Recv, l.line, true, false, l.at)
				}
			}
			shown := func() (pane, rail int64) {
				summary, _ := sess.volatileSummary(false)
				return sess.projection.status().Runtime.CompactingSinceMs, sessionRuntimeInfo(summary).CompactingSinceMs
			}
			apply(test.start)
			if pane, rail := shown(); pane != test.since || rail != test.since {
				t.Fatalf("compacting since: pane %d, rail %d; want %d", pane, rail, test.since)
			}
			apply(test.end)
			if pane, rail := shown(); pane != 0 || rail != 0 {
				t.Fatalf("after compaction ended: pane %d, rail %d; want none", pane, rail)
			}
			apply(test.start[len(test.start)-1:])
			if pane, _ := shown(); pane == 0 {
				t.Fatal("a second compaction did not show")
			}
			sess.recordWire(protocol.Recv, test.turnEnd)
			if pane, rail := shown(); pane != 0 || rail != 0 {
				t.Fatalf("after the turn ended: pane %d, rail %d; want none", pane, rail)
			}
		})
	}
}

// Every delta used to carry every held thread. A delta names only the threads whose runtime changed, and
// names a thread Nano released with no overlay.
func TestRuntimeDeltaCarriesOnlyChangedRows(t *testing.T) {
	now := time.Now()
	a := memorySession("sess-a", "codex", "thread-a", "ready", "/w/a", "gpt-5.6-sol", now)
	b := memorySession("sess-b", "codex", "thread-b", "ready", "/w/b", "gpt-5.6-sol", now)
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{"sess-a": a, "sess-b": b}}}
	if first := s.runtimeDeltaSnapshot(); len(first.Rows) != 2 {
		t.Fatalf("first delta rows = %d, want every held thread", len(first.Rows))
	}
	if same := s.runtimeDeltaSnapshot(); len(same.Rows) != 0 {
		t.Fatalf("an unchanged runtime produced %d rows", len(same.Rows))
	}
	a.lifecycleState = testLifecycleSnapshot("ready", true)
	flip := s.runtimeDeltaSnapshot()
	if len(flip.Rows) != 1 || flip.Rows[0].NativeID != "thread-a" || flip.Rows[0].Runtime == nil || !flip.Rows[0].Runtime.TurnActive {
		t.Fatalf("turnActive flip delta = %#v, want only thread-a", flip.Rows)
	}
	delete(s.sessions, "sess-b")
	gone := s.runtimeDeltaSnapshot()
	if gone.Refresh || !slices.Equal(gone.RemovedSessions, []string{"sess-b"}) || len(gone.Rows) != 1 || gone.Rows[0].NativeID != "thread-b" || gone.Rows[0].Runtime != nil {
		t.Fatalf("release delta = refresh:%v removed:%v %#v, want sess-b removed and thread-b with no overlay", gone.Refresh, gone.RemovedSessions, gone.Rows)
	}
}

// A refresh that needs the sidebar rebuilt, merged with a later star update
// that applied cleanly, must still rebuild: the materializer reads one flag.
func TestRuntimeHubMergeKeepsRebuildNeed(t *testing.T) {
	hub := newRuntimeHub()
	slow := hub.subscribe()
	defer hub.unsubscribe(slow)

	hub.notify(runtimeDelta{Refresh: true})
	hub.notify(runtimeDelta{Refresh: true, sidebarCurrent: true, Rows: []runtimeDeltaRow{}})
	merged := <-slow
	if !merged.Refresh || merged.sidebarCurrent {
		t.Fatalf("merged delta = %+v, want refresh with the rebuild still needed", merged)
	}
	if merged.Rows == nil {
		t.Fatal("merged delta lost its rows field; browsers read a missing one as a whole-list read")
	}
}
