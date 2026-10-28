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
func (sess *Session) AvailabilityState() Availability {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return Availability{
		Status: sess.connectionStatusLocked(), Active: sess.workActiveLocked(),
		Waiting: sess.waitingLocked(), Permission: sess.lifecyclePermissionLocked(),
		LastHumanPromptAt: sess.lastHumanPromptAt,
	}
}

// QueuedPromptCount counts prompts waiting on this attachment.
func (sess *Session) QueuedPromptCount() int {
	if sess.queuedTurns == nil {
		return 0
	}
	return sess.queuedTurns.QueuedCount(sess.provider, sess.ThreadID(), sess.hostedThreadID)
}

// ActiveTurnSnapshot returns one atomic snapshot of the live turn fields.
// activeTurn is one atomic snapshot of the monitor's live turn fields.
func (sess *Session) ActiveTurnSnapshot() (observability.ActiveTurn, bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return observability.ActiveTurn{Provider: sess.provider, ThreadID: sess.threadID,
		HostedThreadID: sess.hostedThreadID, Name: sess.displayName,
		TurnStartedAtMs: sess.turnStartedAtMs}, sess.workActiveLocked()
}

// WithIdleDelivery holds the delivery lock from readiness through the caller's
// durable admission and queue update. Hosted admission remains with the caller.
func (sess *Session) WithIdleDelivery(rested bool, deliver func() error) (string, error) {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	sess.stateMu.Lock()
	status, permission := sess.connectionStatusLocked(), sess.lifecyclePermissionLocked()
	sess.stateMu.Unlock()
	// A resting thread had no open work at boot and queues behind its attach.
	restingIdle := rested && status == "starting"
	switch {
	case status == StatusExited:
		return "detached", nil
	case sess.nanoIsStopping() || status != "ready" && !restingIdle:
		return "notReady", nil
	case !restingIdle && (permission.Submit != lifecycle.SendNow || sess.CorePromptInFlight()):
		return "busy", nil
	}
	if sess.queuedTurns != nil {
		if _, queued := sess.queuedTurns.First(sess.provider, sess.ThreadID(), sess.hostedThreadID); queued {
			return "queuedBacklog", nil
		}
	}
	return "", deliver()
}

// BusyForMaintenance reports live, restoring or delivering work. Locks do not
// extend into the caller's maintenance store operations.
func (sess *Session) BusyForMaintenance() bool {
	sess.stateMu.Lock()
	active := sess.workActiveLocked() || sess.turnRestoreInProgress
	sess.stateMu.Unlock()
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
