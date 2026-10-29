package sessions

import (
	"context"
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/liveview"
)

func retainedTestProjection(turnID string, startedAtMs int64) *LiveProjection {
	p := newDeliveryTestProjection("codex")
	p.seedResumeTurns("native-thread", []*liveview.Turn{{ID: turnID, Status: "in_progress", StartedAtMs: startedAtMs}})
	return p
}
func durableTurnTestSession(turnID string, startedAtMs int64) *Session {
	p := retainedTestProjection(turnID, startedAtMs)
	return sessionWithOwners(&Session{provider: "codex",
		stateOwner: State{threadID: "native-thread", turnID: turnID, turnStartedAtMs: startedAtMs, lifecycleState: testLifecycleSnapshot("ready", true)},
		view:       View{projection: p}})
}
func reconcileWithSavedTurn(sess *Session, saved liveview.DurableTurnEvidence) {
	sess.stateOwner.savedTurn = func(_ context.Context, id string) (liveview.DurableTurnEvidence, bool) {
		return saved, id == saved.Turn.ID
	}
	sess.stateOwner.ReconcileDurableTurn(context.Background())
}
func TestDurableReconciliationSettlesResumeBeforeHandshakeCannotResurrect(t *testing.T) {
	started := time.Now().Add(-time.Minute).UnixMilli()
	sess := durableTurnTestSession("turn-resume", started)
	reconcileWithSavedTurn(sess, liveview.DurableTurnEvidence{
		Turn: liveview.Turn{ID: "turn-resume", Status: "completed", CompletedAtMs: started + 1, DurationMS: 1},
	})
	sess.stateOwner.setTurnActive(true, "turn-resume", started, true) // late thread/resume handshake
	sess.stateOwner.stateMu.Lock()
	active, guard := sess.stateOwner.workActiveLocked(), sess.stateOwner.durablyTerminalTurnID
	sess.stateOwner.stateMu.Unlock()
	if active || guard != "turn-resume" {
		t.Fatalf("terminal resume was resurrected: active=%v guard=%q", active, guard)
	}
}

func TestDurableCatalogReconciliationSettlesExactCurrentTurnAndAllowsNewerTurn(t *testing.T) {
	started := time.Now().Add(-time.Minute).UnixMilli()
	sess := durableTurnTestSession("turn-current", started)
	reconcileWithSavedTurn(sess, liveview.DurableTurnEvidence{
		Turn: liveview.Turn{ID: "turn-current", Status: "interrupted", CompletedAtMs: started + 1},
	})
	sess.stateOwner.stateMu.Lock()
	active := sess.stateOwner.workActiveLocked()
	sess.stateOwner.stateMu.Unlock()
	if active {
		t.Fatal("catalog terminal evidence left the exact live turn working")
	}
	sess.stateOwner.setTurnActive(true, "turn-newer", started+2, true)
	sess.stateOwner.stateMu.Lock()
	active, current, guard := sess.stateOwner.workActiveLocked(), sess.stateOwner.turnID, sess.stateOwner.durablyTerminalTurnID
	sess.stateOwner.stateMu.Unlock()
	if !active || current != "turn-newer" || guard != "turn-current" {
		t.Fatalf("newer turn did not preserve the old terminal guard: active=%v current=%q guard=%q", active, current, guard)
	}
}

func TestDurableReconciliationLeavesWeakOrMismatchedEvidenceWorking(t *testing.T) {
	started := time.Now().UnixMilli()
	for _, saved := range []liveview.DurableTurnEvidence{
		{Turn: liveview.Turn{ID: "turn-current", Status: "completed"}},
		{Turn: liveview.Turn{ID: "turn-other", Status: "completed", CompletedAtMs: started + 1}},
	} {
		sess := durableTurnTestSession("turn-current", started)
		reconcileWithSavedTurn(sess, saved)
		sess.stateOwner.stateMu.Lock()
		active := sess.stateOwner.workActiveLocked()
		sess.stateOwner.stateMu.Unlock()
		if !active {
			t.Fatalf("weak or mismatched durable evidence cleared the live turn: %#v", saved)
		}
	}
}

func TestDurableFileTurnDoesNotWriteCompletedTurnFeed(t *testing.T) {
	sess, _ := readyTransactionalSendTestSession(t, nil)
	store, err := coreturns.OpenStore(context.Background(), sess.delivery.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	sess.runtime.completedTurns = func(context.Context) (*coreturns.Store, error) { return store, nil }
	started := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	sess.stateOwner.turnID = "turn-file"
	sess.stateOwner.lifecycleState = testLifecycleSnapshot("ready", true)
	sess.view.projection = retainedTestProjection("turn-file", started.UnixMilli())

	reconcileWithSavedTurn(sess, liveview.DurableTurnEvidence{
		Turn: liveview.Turn{ID: "turn-file", Status: "completed", StartedAtMs: started.UnixMilli(), CompletedAtMs: started.Add(time.Minute).UnixMilli()},
	})
	rows, err := store.After(context.Background(), sess.hostedThreadID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("durable file completion wrote feed rows: %+v", rows)
	}
}

// Attachment replacement and live-work cases share the same reconciliation test.
func checkSavedAttachmentReplacement(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Session)
	}{
		{"attachment replaced during the read", func(sess *Session) {
			sess.stateOwner.stateMu.Lock()
			sess.stateOwner.attachmentGeneration++
			sess.stateOwner.stateMu.Unlock()
		}},
		{"native thread replaced during the read", func(sess *Session) {
			sess.stateOwner.stateMu.Lock()
			sess.stateOwner.threadID = "native-replacement"
			sess.stateOwner.stateMu.Unlock()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now().Add(-time.Minute)
			sess := durableTurnTestSession("turn-1", started.UnixMilli())
			sess.stateOwner.savedTurn = func(_ context.Context, id string) (liveview.DurableTurnEvidence, bool) {
				test.change(sess)
				return liveview.DurableTurnEvidence{Turn: liveview.Turn{ID: "turn-1", Status: "completed", CompletedAtMs: started.Add(time.Second).UnixMilli(), DurationMS: 1000}}, id == "turn-1"
			}
			sess.stateOwner.ReconcileDurableTurn(context.Background())
			sess.stateOwner.stateMu.Lock()
			working := sess.stateOwner.workActiveLocked()
			sess.stateOwner.stateMu.Unlock()
			if !working {
				t.Fatal("working after the saved completion = false, want true")
			}
		})
	}
}
