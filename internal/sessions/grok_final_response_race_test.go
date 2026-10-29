package sessions

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const grokRacePrompt = "cd70f2be-6894-4ff6-aee4-e1d5ded92289"

func grokRaceChunk(event, text string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"native-thread","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}},"_meta":{"eventId":%q,"promptId":%q}}}`, text, event, grokRacePrompt))
}

func grokRaceTurnCompleted() []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"_x.ai/session_notification","params":{"sessionId":"native-thread","update":{"sessionUpdate":"turn_completed","prompt_id":%q,"stop_reason":"end_turn"},"_meta":{"eventId":"native-thread-3"}}}`, grokRacePrompt))
}

// grokRaceSession records the candidate visible at the first terminal call.
// withCore attaches the real core session mailbox; persistence stays in app tests.
func grokRaceSession(t *testing.T, withCore bool) (*Session, *grokTerminalRecorder) {
	t.Helper()
	store := &grokTerminalRecorder{}
	var sess *Session
	if withCore {
		sess, _ = readyTransactionalSendTestSession(t, nil)
	} else {
		sess = sessionWithOwners(&Session{id: "nano-session",
			stateOwner: State{threadID: "native-thread", lifecycleState: testLifecycleSnapshot("ready", false)}})
	}
	sess.provider = "grok"
	sess.hostedThreadID = "hosted"
	sess.agentExecutions = store
	sess.view.projection = newDeliveryTestProjection("grok")
	sess.view.projection.setNativeThreadID("native-thread")
	if withCore {
		if err := sess.runtime.ensureCoreSession(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sess.runtime.coreSession().Close)
	}
	return sess, store
}

// holdMailbox parks the core session loop until the returned release runs.
func holdMailbox(t *testing.T, core *coresession.Session) func() {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	core.ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error {
		close(entered)
		<-release
		return nil
	}})
	<-entered
	return func() { close(release) }
}

func flushMailbox(t *testing.T, core *coresession.Session) {
	t.Helper()
	if err := core.Observe(context.Background(), coresession.AdapterEvent{}); err != nil {
		t.Fatal(err)
	}
}

func waitGrokTerminal(t *testing.T, store *grokTerminalRecorder) grokTerminalCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, _ := store.firstTerminal()
		if threadvalues.TurnTerminal(rec.Status) || time.Now().After(deadline) {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Fails if the RPC outcome completes the operation from a projection that has
// not yet applied the final chunks queued ahead of it (the recorded
// finalResponse was "GROK47-CHECK" for the reply "GROK47-CHECK-R2-OK").
// Without a saved-result reader (production always has one) the operation
// records the wire's own text, the newest chunk, so the final chunk "-R2-OK"
// shows the outcome waited for it. Boundary:
// grokSession.observeResolvedPromptOutcome versus the core mailbox.
func TestGrokRPCOutcomeWaitsForQueuedFinalChunks(t *testing.T) {
	sess, store := grokRaceSession(t, true)
	core := sess.runtime.coreSession()
	g := &grokSession{sess: sess}

	sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
	flushMailbox(t, core)

	release := holdMailbox(t, core)
	sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-2", "-R2-OK"), true)
	sess.wire.recordWireLive(protocol.Recv, grokRaceTurnCompleted(), true)
	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})
	release()
	flushMailbox(t, core)

	rec := waitGrokTerminal(t, store)
	if rec.Status != agentTurnCompleted || rec.Candidate != "-R2-OK" {
		t.Fatalf("first terminal = status %q candidate %q; want completed with the final chunk %q", rec.Status, rec.Candidate, "-R2-OK")
	}
}

// Fails if a session without a core mailbox stops completing from the RPC
// outcome. Boundary: the direct-call path of observeResolvedPromptOutcome.
func TestGrokRPCOutcomeCompletesDirectlyWithoutCoreSession(t *testing.T) {
	sess, store := grokRaceSession(t, false)
	g := &grokSession{sess: sess}
	sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
	sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-2", "-R2-OK"), true)

	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})

	rec, _ := store.firstTerminal()
	if rec.Status != agentTurnCompleted || rec.Candidate != "-R2-OK" {
		t.Fatalf("direct outcome = status %q candidate %q; want completed with the final chunk %q", rec.Status, rec.Candidate, "-R2-OK")
	}
}

func setGrokQueuedOutcomeBound(t *testing.T, bound time.Duration) {
	t.Helper()
	previous := grokQueuedOutcomeBound
	grokQueuedOutcomeBound = bound
	t.Cleanup(func() { grokQueuedOutcomeBound = previous })
}

// Fails if a completed RPC outcome waits without bound, or blocks its caller,
// when the session mailbox is stalled (the 2026-09-21 stall case) or already
// closed. Boundary: the queued completion's fallback bound.
func TestGrokRPCOutcomeCompletesWithinBoundWhenMailboxStalls(t *testing.T) {
	for _, mode := range []string{"stalled", "closed"} {
		t.Run(mode, func(t *testing.T) {
			setGrokQueuedOutcomeBound(t, 50*time.Millisecond)
			sess, store := grokRaceSession(t, true)
			core := sess.runtime.coreSession()
			g := &grokSession{sess: sess}
			sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
			flushMailbox(t, core)
			if mode == "stalled" {
				t.Cleanup(holdMailbox(t, core))
			} else {
				core.Close()
			}

			started := time.Now()
			g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})
			if returned := time.Since(started); returned > time.Second {
				t.Fatalf("outcome blocked its caller for %s", returned)
			}
			rec := waitGrokTerminal(t, store)
			if rec.Status != agentTurnCompleted || rec.Candidate != "GROK47-CHECK" {
				t.Fatalf("first terminal = status %q candidate %q; want completed from the applied projection", rec.Status, rec.Candidate)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("completion took %s with a 50ms bound", elapsed)
			}
		})
	}
}

// Fails if the queued RPC outcome and its fallback bound complete the
// operation twice, or if a later terminal overwrites the first. Boundary: the
// wire turn_completed, the queued outcome, the bound, and a late failure.
func TestGrokRPCOutcomeTerminalSessionTimer(t *testing.T) {
	setGrokQueuedOutcomeBound(t, 500*time.Millisecond)
	sess, store := grokRaceSession(t, true)
	core := sess.runtime.coreSession()
	g := &grokSession{sess: sess}
	release := holdMailbox(t, core)
	sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
	sess.wire.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-2", "-R2-OK"), true)
	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})
	release()
	flushMailbox(t, core)

	first := waitGrokTerminal(t, store)
	if first.Status != agentTurnCompleted || first.Candidate != "-R2-OK" {
		t.Fatalf("first terminal = status %q candidate %q; want completed with the final chunk", first.Status, first.Candidate)
	}
	time.Sleep(700 * time.Millisecond) // past the bound
	if calls := store.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("queued outcome and timer recorded %d terminals, want exactly one", len(calls))
	}
}

// Fails if an interrupted or failed RPC outcome, which carries no reply text,
// waits behind a held mailbox; its runtime notification must stay immediate.
// Boundary: the completed-only queueing in observeResolvedPromptOutcome.
func TestGrokRPCNonCompletedOutcomeIsNotQueued(t *testing.T) {
	setGrokQueuedOutcomeBound(t, time.Hour)
	sess, store := grokRaceSession(t, true)
	t.Cleanup(holdMailbox(t, sess.runtime.coreSession()))
	g := &grokSession{sess: sess}

	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "cancelled"}})

	rec, _ := store.firstTerminal()
	if rec.Status != agentTurnInterrupted {
		t.Fatalf("interrupted outcome waited behind the mailbox: status %q", rec.Status)
	}
}

// grokTerminalRecorder records the candidate visible at each terminal call.
// It deliberately applies every call; the app sibling tests durable first-terminal wins.
type grokTerminalRecorder struct {
	Executions
	mu        sync.Mutex
	candidate string
	calls     []grokTerminalCall
}
type grokTerminalCall struct{ Status, Candidate string }

func (r *grokTerminalRecorder) ObserveCandidate(_, _, _, _, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.candidate = text
	return nil
}
func (r *grokTerminalRecorder) MarkTurnTerminal(_, _, _, status string, _ *threadvalues.ExecutionError, _, _ string, _ ...hostedstore.ResultMeta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, grokTerminalCall{status, r.candidate})
	return nil
}
func (r *grokTerminalRecorder) firstTerminal() (grokTerminalCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return grokTerminalCall{}, false
	}
	return r.calls[0], true
}
func (r *grokTerminalRecorder) callsSnapshot() []grokTerminalCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]grokTerminalCall(nil), r.calls...)
}
func (*grokTerminalRecorder) BindSession(string, string, string, string, string) error   { return nil }
func (*grokTerminalRecorder) UpdateSession(string, string, string, string, string) error { return nil }
func (*grokTerminalRecorder) MarkSessionTurns(string, string, *threadvalues.ExecutionError) error {
	return nil
}
func (*grokTerminalRecorder) MarkWaiting(string, string, bool) error { return nil }
func (*grokTerminalRecorder) OpenTurnsForProviderTurn(string, string, string) []threadvalues.TurnRecord {
	return nil
}
func (*grokTerminalRecorder) TurnsForHosted(string) []threadvalues.TurnRecord { return nil }
func (*grokTerminalRecorder) ReadsSavedResults() bool                         { return false }
func (*grokTerminalRecorder) TurnForTransportAction(string) (threadvalues.TurnRecord, bool) {
	return threadvalues.TurnRecord{}, false
}
func (*grokTerminalRecorder) PersistedTurnForTransportAction(context.Context, string, string) (threadvalues.TurnRecord, bool, error) {
	return threadvalues.TurnRecord{}, false, nil
}
