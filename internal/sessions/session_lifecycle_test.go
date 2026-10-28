package sessions

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/lifecycle"
)

func lifecycleTestSession(status string, active bool, turnID string) *Session {
	return &Session{turnID: turnID,
		attachmentGeneration: 1,
		state:                map[string]json.RawMessage{},
		lifecycleState:       testLifecycleSnapshot(status, active)}
}

func TestSessionLifecycleConnectionLossHidesWork(t *testing.T) {
	sess := lifecycleTestSession("ready", true, "running")
	sess.setStatus("reconnecting")
	if sum := sess.Summary(); sum.Status != "reconnecting" || sum.TurnActive || sum.Waiting {
		t.Fatalf("disconnected summary derived work: %+v", sum)
	}
}

func TestSessionLifecycleClosedAttachmentRejectsLateReady(t *testing.T) {
	sess := lifecycleTestSession("ready", false, "")
	sess.setStatus("exited")
	sess.setStatus("ready")
	if got := sess.Summary().Status; got != "exited" {
		t.Fatalf("late provider readiness reopened closed attachment: %s", got)
	}
}

func TestSessionLifecycleOldTerminalCannotEndCurrentTurn(t *testing.T) {
	sess := lifecycleTestSession("ready", true, "current")
	sess.setTurnActive(false, "old", 0, true)
	if !sess.workActiveLocked() || sess.turnID != "current" {
		t.Fatalf("old terminal cleared current turn: active=%v turn=%q", sess.workActiveLocked(), sess.turnID)
	}
}

func TestSessionLifecycleReplayMustSettleBeforeReady(t *testing.T) {
	sess := lifecycleTestSession("starting", false, "")
	sess.beginTurnRestore()
	sess.restoreTurnActive(true, "provider-current", 10)
	sess.setStatus("ready")
	if sum := sess.Summary(); sum.Status != "starting" || sum.TurnActive {
		t.Fatalf("unsettled replay escaped through readiness: %+v", sum)
	}
	sess.settleRestoredTurnState()
	if sum := sess.Summary(); sum.Status != "ready" || !sum.TurnActive || sum.TurnStartedAtMs != 10 {
		t.Fatalf("settled provider state did not become current: %+v", sum)
	}
}

func (sess *Session) setStatus(status string) {
	if status == "starting" {
		sess.stateMu.Lock()
		sess.lifecycleState = testLifecycleSnapshot(status, false)
		sess.stateMu.Unlock()
		return
	}
	events := map[string]lifecycle.Event{"ready": lifecycle.EventReadyIdle, "reconnecting": lifecycle.EventLinkLost,
		"reconnect_parked": lifecycle.EventRetriesExhausted, "reconnect_failed": lifecycle.EventNativeMissing,
		"exited": lifecycle.EventRuntimeExited, "detached": lifecycle.EventDetach}
	sess.observeConnection(events[status])
}
