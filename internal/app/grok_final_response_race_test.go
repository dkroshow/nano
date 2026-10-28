package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
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

// grokRaceSession is a hosted Grok session with a running operation for
// grokRacePrompt. withCore attaches the real core session mailbox.
func grokRaceSession(t *testing.T, withCore bool) (*session, *agentExecutionStore, string) {
	t.Helper()
	store := newTestAgentExecutionStore(t, "run")
	hosted, _, err := store.beginSession("caller", "hash", "grok", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(hosted.ID, "nano", "native-thread", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	op, _, err := store.beginTurn(hosted.ID, "action", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnDelivery(op.ID, grokRacePrompt, agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	var sess *session
	if withCore {
		sess, _ = readyTransactionalSendTestSession(t, nil)
	} else {
		sess = &session{id: "nano-session", threadID: "native-thread", lifecycleState: testLifecycleSnapshot("ready", false)}
	}
	sess.provider = "grok"
	sess.hostedThreadID = hosted.ID
	sess.agentExecutions = store
	sess.projection = newLiveProjection("grok", hosted.ID)
	sess.projection.SetNativeThreadID("native-thread")
	if withCore {
		if err := sess.ensureCoreSession(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sess.coreSession().Close)
	}
	return sess, store, op.ID
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

func waitGrokOperation(t *testing.T, store *agentExecutionStore, id string) agentTurnRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, _ := store.turn(id)
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
	sess, store, opID := grokRaceSession(t, true)
	core := sess.coreSession()
	g := &grokSession{sess: sess}

	sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
	flushMailbox(t, core)

	release := holdMailbox(t, core)
	sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-2", "-R2-OK"), true)
	sess.recordWireLive(protocol.Recv, grokRaceTurnCompleted(), true)
	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})
	release()
	flushMailbox(t, core)

	rec := waitGrokOperation(t, store, opID)
	if rec.Status != agentTurnCompleted || rec.FinalResponse != "-R2-OK" || rec.FinalResponseTruncated {
		t.Fatalf("operation = status %q finalResponse %q truncated %v; want completed with the final chunk %q", rec.Status, rec.FinalResponse, rec.FinalResponseTruncated, "-R2-OK")
	}
}

// Fails if a session without a core mailbox stops completing from the RPC
// outcome. Boundary: the direct-call path of observeResolvedPromptOutcome.
func TestGrokRPCOutcomeCompletesDirectlyWithoutCoreSession(t *testing.T) {
	sess, store, opID := grokRaceSession(t, false)
	g := &grokSession{sess: sess}
	sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
	sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-2", "-R2-OK"), true)

	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})

	rec, _ := store.turn(opID)
	if rec.Status != agentTurnCompleted || rec.FinalResponse != "-R2-OK" {
		t.Fatalf("direct outcome = status %q finalResponse %q; want completed with the final chunk %q", rec.Status, rec.FinalResponse, "-R2-OK")
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
			sess, store, opID := grokRaceSession(t, true)
			core := sess.coreSession()
			g := &grokSession{sess: sess}
			sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
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
			rec := waitGrokOperation(t, store, opID)
			if rec.Status != agentTurnCompleted || rec.FinalResponse != "GROK47-CHECK" {
				t.Fatalf("operation = status %q finalResponse %q; want completed from the applied projection", rec.Status, rec.FinalResponse)
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
func TestGrokRPCOutcomeTerminalAppliesExactlyOnce(t *testing.T) {
	setGrokQueuedOutcomeBound(t, 500*time.Millisecond)
	sess, store, opID := grokRaceSession(t, true)
	core := sess.coreSession()
	g := &grokSession{sess: sess}
	release := holdMailbox(t, core)
	sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-1", "GROK47-CHECK"), true)
	sess.recordWireLive(protocol.Recv, grokRaceChunk("native-thread-2", "-R2-OK"), true)
	sess.recordWireLive(protocol.Recv, grokRaceTurnCompleted(), true)
	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "end_turn"}})
	release()
	flushMailbox(t, core)

	first := waitGrokOperation(t, store, opID)
	if first.Status != agentTurnCompleted || first.FinalResponse != "-R2-OK" {
		t.Fatalf("operation = status %q finalResponse %q; want completed with the final chunk", first.Status, first.FinalResponse)
	}
	time.Sleep(700 * time.Millisecond) // past the bound
	sess.observeGrokAgentTerminal("native-thread", grokRacePrompt, agentTurnFailed, "late", nil)
	after, _ := store.turn(opID)
	if after.Revision != first.Revision || after.Status != agentTurnCompleted || after.FinalResponse != first.FinalResponse {
		t.Fatalf("terminal rewritten: revision %d->%d status %q finalResponse %q", first.Revision, after.Revision, after.Status, after.FinalResponse)
	}
}

// Fails if an interrupted or failed RPC outcome, which carries no reply text,
// waits behind a held mailbox; its runtime notification must stay immediate.
// Boundary: the completed-only queueing in observeResolvedPromptOutcome.
func TestGrokRPCNonCompletedOutcomeIsNotQueued(t *testing.T) {
	setGrokQueuedOutcomeBound(t, time.Hour)
	sess, store, opID := grokRaceSession(t, true)
	t.Cleanup(holdMailbox(t, sess.coreSession()))
	g := &grokSession{sess: sess}

	g.observeResolvedPromptOutcome(grokRacePrompt, grokPromptOutcome{response: grokPromptResponse{StopReason: "cancelled"}})

	rec, _ := store.turn(opID)
	if rec.Status != agentTurnInterrupted {
		t.Fatalf("interrupted outcome waited behind the mailbox: status %q", rec.Status)
	}
}
