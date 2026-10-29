package sessions

import (
	"log"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	coresession "github.com/dkroshow/nano/internal/core/session"
	"github.com/dkroshow/nano/internal/lifecycle"
)

// connectionStatusLocked is the compatibility projection for existing API
// readers. It is never stored or accepted as current provider evidence.
func (sess *State) connectionStatusLocked() string {
	switch sess.lifecycleState.State.Connection {
	case lifecycle.Parked:
		return "reconnect_parked"
	case lifecycle.Unreachable:
		return "reconnect_failed"
	default:
		return string(sess.lifecycleState.State.Connection)
	}
}

// threadStatusLocked is the thread's turn state while Nano holds it. Every
// surface that shows it (pane, session list, runtime overlay, catalog row)
// reads this one value.
func (sess *State) threadStatusLocked() string {
	return catalog.FleetLabelFromLifecycle(sess.lifecycleState.State)
}

func (sess *State) workActiveLocked() bool {
	state := sess.lifecycleState.State
	return state.Connection == lifecycle.Ready && state.Work != lifecycle.Idle
}

// transitionLifecycleLocked submits a qualified observation. Its callers
// have already qualified the provider identity/turn and discarded stale replay.
// stateMu serializes the observation with the current snapshot and its effects.
func (sess *State) transitionLifecycleLocked(event lifecycle.Event) (changed, drain bool) {
	before := sess.lifecycleState
	observation := lifecycle.Observation{Event: event, Epoch: before.Epoch, Sequence: sess.nextEvidenceSequenceLocked()}
	return sess.applyLifecycleObservationLocked(observation)
}

func (sess *State) applyLifecycleObservationLocked(observation lifecycle.Observation) (changed, drain bool) {
	before := sess.lifecycleState
	out, err := lifecycle.Transition(before, observation)
	if err != nil {
		log.Printf("session %s lifecycle observation %s: %v", sess.session.id, observation.Event, err)
		return false, false
	}
	sess.lifecycleState = out.Snapshot
	if !sess.workActiveLocked() {
		sess.compactingSinceMs = 0 // compaction belongs to the turn in flight
	}
	if after := out.Snapshot.State.Connection; after != before.State.Connection {
		switch after {
		case lifecycle.Ready:
			sess.recordRestartStep(restartLive)
		case lifecycle.Unreachable:
			sess.recordRestartFailure("reconnect_failed")
		case lifecycle.Parked:
			sess.recordRestartFailure("reconnect_parked")
		case lifecycle.Exited:
			sess.recordRestartFailure("exited")
		case lifecycle.Detached:
			sess.recordRestartFailure("detached")
		}
	}
	if core := sess.session.runtime.coreSession(); core != nil {
		copy := observation
		core.ObserveAsync(coresession.AdapterEvent{Lifecycle: &copy})
		// A thread that starts or returns to working can take a prompt queued
		// while it could not, however it got there: a started turn, a
		// reconnect, an answered approval. The kick follows the core's
		// observation, and the drain reads the current turn only after the
		// caller releases stateMu.
		if workingLifecycle(out.Snapshot.State) && !workingLifecycle(before.State) && core.TurnCapabilities().RunningTurnInput {
			sess.session.delivery.kickQueuedTurnDrain()
		}
	}
	return before.State != out.Snapshot.State, out.Drain
}

func workingLifecycle(state lifecycle.State) bool {
	return state.Connection == lifecycle.Ready && state.Work == lifecycle.Working
}

func (sess *State) readyLifecycleEventLocked() lifecycle.Event {
	if len(sess.interactions) > 0 {
		return lifecycle.EventReadyWaiting
	}
	if sess.turnRestoreInProgress {
		if sess.turnRestoreAccepted {
			return lifecycle.EventReadyAccepted
		}
		if sess.turnRestoreActive {
			return lifecycle.EventReadyWorking
		}
		return lifecycle.EventReadyIdle
	}
	if sess.lifecycleState.State.Connection == lifecycle.Ready {
		switch sess.lifecycleState.State.Work {
		case lifecycle.Accepted:
			return lifecycle.EventReadyAccepted
		case lifecycle.Working:
			return lifecycle.EventReadyWorking
		case lifecycle.WaitingOnYou:
			return lifecycle.EventReadyWaiting
		}
	}
	return lifecycle.EventReadyIdle
}

func (sess *State) observeConnection(event lifecycle.Event) {
	sess.stateMu.Lock()
	if event == lifecycle.EventReadyIdle {
		if sess.turnRestoreInProgress {
			event = lifecycle.EventReadyBeforeReplaySettled
		} else {
			event = sess.readyLifecycleEventLocked()
		}
	}
	wasActive := sess.workActiveLocked()
	if event == lifecycle.EventLinkLost {
		sess.codexTurnEpoch++ // nothing this connection saw proves a drop now
	}
	changed, drain := sess.transitionLifecycleLocked(event)
	status, closed := sess.connectionStatusLocked(), sess.closed
	if changed && (sess.lifecycleState.State.Connection == lifecycle.Exited || sess.lifecycleState.State.Connection == lifecycle.Detached) {
		if wasActive {
			sess.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		sess.turnID, sess.turnStartedAtMs = "", 0
		sess.clearTurnRestoreLocked()
	}
	sess.stateMu.Unlock()
	if !changed {
		return
	}
	if status == StatusExited {
		sess.session.delivery.settleAgentTurnsOnExit(closed)
	}
	sess.session.view.PublishProjectionRuntime()
	sess.NotifyRuntime()
	sess.updateHostedThread(status, "")
	if drain {
		sess.session.delivery.kickQueuedTurnDrain()
	}
}

// Provider acceptance is distinct from a local send reservation. An ACK that
// follows the same turn's terminal cannot make that completed turn active again.
func (sess *State) acceptProviderTurn(turnID string) {
	sess.stateMu.Lock()
	if turnID != "" && (turnID == sess.lastProviderTerminalTurnID || turnID == sess.durablyTerminalTurnID) {
		sess.stateMu.Unlock()
		return
	}
	if sess.turnRestoreInProgress {
		if !sess.turnRestoreActive {
			sess.turnRestoreSequence = sess.nextEvidenceSequenceLocked()
			sess.turnRestoreActive, sess.turnRestoreAccepted = true, true
			sess.turnRestoreID = turnID
		}
		sess.stateMu.Unlock()
		return
	}
	changed, _ := sess.transitionLifecycleLocked(lifecycle.EventAccepted)
	if changed {
		sess.turnID = turnID
	}
	sess.stateMu.Unlock()
	if changed {
		sess.session.view.PublishProjectionRuntime()
		sess.NotifyRuntime()
	}
}

func (sess *State) lifecyclePermissionLocked() lifecycle.Permission {
	permission, _ := lifecycle.Permissions(sess.lifecycleState.State)
	return permission
}

func (sess *State) waitingLocked() bool {
	return sess.lifecycleState.State.Connection == lifecycle.Ready && sess.lifecycleState.State.Work == lifecycle.WaitingOnYou
}

// During replay, interaction changes are provider evidence to settle with the
// response. They cannot advance the visible lifecycle ahead of that barrier.
func (sess *State) observeInteractionLocked(event lifecycle.Event, turnID string) {
	if sess.turnRestoreInProgress {
		sess.turnRestoreSequence = sess.nextEvidenceSequenceLocked()
		sess.turnRestoreActive, sess.turnRestoreAccepted = true, false
		if turnID != "" {
			sess.turnRestoreID = turnID
		}
		return
	}
	sess.transitionLifecycleLocked(event)
}
