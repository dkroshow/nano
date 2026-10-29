package sessions

import (
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
)

// Availability describes whether the attachment can accept a prompt.
type Availability struct {
	Status            string
	Active            bool
	Waiting           bool
	Permission        lifecycle.Permission
	LastHumanPromptAt time.Time
}

// AvailabilityState reads the runtime facts used by admission and availability.
func (sess *State) AvailabilityState() Availability {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return Availability{
		Status: sess.connectionStatusLocked(), Active: sess.workActiveLocked(),
		Waiting: sess.waitingLocked(), Permission: sess.lifecyclePermissionLocked(),
		LastHumanPromptAt: sess.lastHumanPromptAt,
	}
}

// QueuedPromptCount counts prompts waiting on this attachment.
func (sess *Delivery) QueuedPromptCount() int {
	if sess.queuedTurns == nil {
		return 0
	}
	return sess.queuedTurns.QueuedCount(sess.session.provider, sess.session.stateOwner.ThreadID(), sess.session.hostedThreadID)
}

// ActiveTurnSnapshot returns one atomic snapshot of the live turn fields.
// activeTurn is one atomic snapshot of the monitor's live turn fields.
func (sess *State) ActiveTurnSnapshot() (observability.ActiveTurn, bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return observability.ActiveTurn{Provider: sess.session.provider, ThreadID: sess.threadID,
		HostedThreadID: sess.session.hostedThreadID, Name: sess.displayName,
		TurnStartedAtMs: sess.turnStartedAtMs}, sess.workActiveLocked()
}

// WithIdleDelivery holds the delivery lock from readiness through the caller's
// durable admission and queue update. Hosted admission remains with the caller.
func (sess *Delivery) WithIdleDelivery(rested bool, deliver func() error) (string, error) {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	sess.session.stateOwner.stateMu.Lock()
	status, permission := sess.session.stateOwner.connectionStatusLocked(), sess.session.stateOwner.lifecyclePermissionLocked()
	sess.session.stateOwner.stateMu.Unlock()
	// A resting thread had no open work at boot and queues behind its attach.
	restingIdle := rested && status == "starting"
	switch {
	case status == StatusExited:
		return "detached", nil
	case sess.nanoIsStopping() || status != "ready" && !restingIdle:
		return "notReady", nil
	case !restingIdle && (permission.Submit != lifecycle.SendNow || sess.session.runtime.CorePromptInFlight()):
		return "busy", nil
	}
	if sess.queuedTurns != nil {
		if _, queued := sess.queuedTurns.First(sess.session.provider, sess.session.stateOwner.ThreadID(), sess.session.hostedThreadID); queued {
			return "queuedBacklog", nil
		}
	}
	return "", deliver()
}

// BusyForMaintenance reports live, restoring or delivering work. Locks do not
// extend into the caller's maintenance store operations.
func (sess *Delivery) BusyForMaintenance() bool {
	sess.session.stateOwner.stateMu.Lock()
	active := sess.session.stateOwner.workActiveLocked() || sess.session.stateOwner.turnRestoreInProgress
	sess.session.stateOwner.stateMu.Unlock()
	if active {
		return true
	}
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	for _, op := range sess.sendOps {
		select {
		case <-op.done:
		default:
			return true
		}
	}
	return false
}
