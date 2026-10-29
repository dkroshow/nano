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
func (attachmentState *State) connectionStatusLocked() string {
	switch attachmentState.lifecycleState.State.Connection {
	case lifecycle.Parked:
		return "reconnect_parked"
	case lifecycle.Unreachable:
		return "reconnect_failed"
	default:
		return string(attachmentState.lifecycleState.State.Connection)
	}
}

// threadStatusLocked is the thread's turn state while Nano holds it. Every
// surface that shows it (pane, session list, runtime overlay, catalog row)
// reads this one value.
func (attachmentState *State) threadStatusLocked() string {
	return catalog.FleetLabelFromLifecycle(attachmentState.lifecycleState.State)
}

func (attachmentState *State) workActiveLocked() bool {
	state := attachmentState.lifecycleState.State
	return state.Connection == lifecycle.Ready && state.Work != lifecycle.Idle
}

// transitionLifecycleLocked submits a qualified observation. Its callers
// have already qualified the provider identity/turn and discarded stale replay.
// stateMu serializes the observation with the current snapshot and its effects.
func (attachmentState *State) transitionLifecycleLocked(event lifecycle.Event) (changed, drain bool) {
	before := attachmentState.lifecycleState
	observation := lifecycle.Observation{Event: event, Epoch: before.Epoch, Sequence: attachmentState.nextEvidenceSequenceLocked()}
	return attachmentState.applyLifecycleObservationLocked(observation)
}

func (attachmentState *State) applyLifecycleObservationLocked(observation lifecycle.Observation) (changed, drain bool) {
	before := attachmentState.lifecycleState
	out, err := lifecycle.Transition(before, observation)
	if err != nil {
		log.Printf("session %s lifecycle observation %s: %v", attachmentState.identity.id, observation.Event, err)
		return false, false
	}
	attachmentState.lifecycleState = out.Snapshot
	if !attachmentState.workActiveLocked() {
		attachmentState.compactingSinceMs = 0 // compaction belongs to the turn in flight
	}
	if after := out.Snapshot.State.Connection; after != before.State.Connection {
		switch after {
		case lifecycle.Ready:
			attachmentState.recordRestartStep(restartLive)
		case lifecycle.Unreachable:
			attachmentState.recordRestartFailure("reconnect_failed")
		case lifecycle.Parked:
			attachmentState.recordRestartFailure("reconnect_parked")
		case lifecycle.Exited:
			attachmentState.recordRestartFailure("exited")
		case lifecycle.Detached:
			attachmentState.recordRestartFailure("detached")
		}
	}
	if core := attachmentState.runtime.coreSession(); core != nil {
		copy := observation
		core.ObserveAsync(coresession.AdapterEvent{Lifecycle: &copy})
		// A thread that starts or returns to working can take a prompt queued
		// while it could not, however it got there: a started turn, a
		// reconnect, an answered approval. The kick follows the core's
		// observation, and the drain reads the current turn only after the
		// caller releases stateMu.
		if workingLifecycle(out.Snapshot.State) && !workingLifecycle(before.State) && core.TurnCapabilities().RunningTurnInput {
			attachmentState.delivery.kickQueuedTurnDrain()
		}
	}
	return before.State != out.Snapshot.State, out.Drain
}

func workingLifecycle(state lifecycle.State) bool {
	return state.Connection == lifecycle.Ready && state.Work == lifecycle.Working
}

func (attachmentState *State) readyLifecycleEventLocked() lifecycle.Event {
	if len(attachmentState.interactions) > 0 {
		return lifecycle.EventReadyWaiting
	}
	if attachmentState.turnRestoreInProgress {
		if attachmentState.turnRestoreAccepted {
			return lifecycle.EventReadyAccepted
		}
		if attachmentState.turnRestoreActive {
			return lifecycle.EventReadyWorking
		}
		return lifecycle.EventReadyIdle
	}
	if attachmentState.lifecycleState.State.Connection == lifecycle.Ready {
		switch attachmentState.lifecycleState.State.Work {
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

func (attachmentState *State) observeConnection(event lifecycle.Event) {
	attachmentState.stateMu.Lock()
	if event == lifecycle.EventReadyIdle {
		if attachmentState.turnRestoreInProgress {
			event = lifecycle.EventReadyBeforeReplaySettled
		} else {
			event = attachmentState.readyLifecycleEventLocked()
		}
	}
	wasActive := attachmentState.workActiveLocked()
	if event == lifecycle.EventLinkLost {
		attachmentState.codexTurnEpoch++ // nothing this connection saw proves a drop now
	}
	changed, drain := attachmentState.transitionLifecycleLocked(event)
	status, closed := attachmentState.connectionStatusLocked(), attachmentState.closed
	if changed && (attachmentState.lifecycleState.State.Connection == lifecycle.Exited || attachmentState.lifecycleState.State.Connection == lifecycle.Detached) {
		if wasActive {
			attachmentState.lastTurnEndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		attachmentState.turnID, attachmentState.turnStartedAtMs = "", 0
		attachmentState.clearTurnRestoreLocked()
	}
	attachmentState.stateMu.Unlock()
	if !changed {
		return
	}
	if status == StatusExited {
		attachmentState.settleAgentTurnsOnExit(closed)
	}
	attachmentState.PublishProjectionRuntime()
	attachmentState.NotifyRuntime()
	attachmentState.updateHostedThread(status, "")
	if drain {
		attachmentState.delivery.kickQueuedTurnDrain()
	}
}

// Provider acceptance is distinct from a local send reservation. An ACK that
// follows the same turn's terminal cannot make that completed turn active again.
func (attachmentState *State) acceptProviderTurn(turnID string) {
	attachmentState.stateMu.Lock()
	if turnID != "" && (turnID == attachmentState.lastProviderTerminalTurnID || turnID == attachmentState.durablyTerminalTurnID) {
		attachmentState.stateMu.Unlock()
		return
	}
	if attachmentState.turnRestoreInProgress {
		if !attachmentState.turnRestoreActive {
			attachmentState.turnRestoreSequence = attachmentState.nextEvidenceSequenceLocked()
			attachmentState.turnRestoreActive, attachmentState.turnRestoreAccepted = true, true
			attachmentState.turnRestoreID = turnID
		}
		attachmentState.stateMu.Unlock()
		return
	}
	changed, _ := attachmentState.transitionLifecycleLocked(lifecycle.EventAccepted)
	if changed {
		attachmentState.turnID = turnID
	}
	attachmentState.stateMu.Unlock()
	if changed {
		attachmentState.PublishProjectionRuntime()
		attachmentState.NotifyRuntime()
	}
}

func (attachmentState *State) lifecyclePermissionLocked() lifecycle.Permission {
	permission, _ := lifecycle.Permissions(attachmentState.lifecycleState.State)
	return permission
}

func (attachmentState *State) waitingLocked() bool {
	return attachmentState.lifecycleState.State.Connection == lifecycle.Ready && attachmentState.lifecycleState.State.Work == lifecycle.WaitingOnYou
}

// During replay, interaction changes are provider evidence to settle with the
// response. They cannot advance the visible lifecycle ahead of that barrier.
func (attachmentState *State) observeInteractionLocked(event lifecycle.Event, turnID string) {
	if attachmentState.turnRestoreInProgress {
		attachmentState.turnRestoreSequence = attachmentState.nextEvidenceSequenceLocked()
		attachmentState.turnRestoreActive, attachmentState.turnRestoreAccepted = true, false
		if turnID != "" {
			attachmentState.turnRestoreID = turnID
		}
		return
	}
	attachmentState.transitionLifecycleLocked(event)
}
