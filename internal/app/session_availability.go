package app

import (
	"time"

	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
)

type sessionAvailability struct {
	Status            string
	Active            bool
	Waiting           bool
	Permission        lifecycle.Permission
	LastHumanPromptAt time.Time
}

// availabilityState reads the runtime facts used by admission and availability.
func (sess *session) availabilityState() sessionAvailability {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return sessionAvailability{
		Status: sess.connectionStatusLocked(), Active: sess.workActiveLocked(),
		Waiting: sess.waitingLocked(), Permission: sess.lifecyclePermissionLocked(),
		LastHumanPromptAt: sess.lastHumanPromptAt,
	}
}

func (sess *session) queuedPromptCount() int {
	if sess.queuedTurns == nil {
		return 0
	}
	return sess.queuedTurns.QueuedCount(sess.provider, sess.ThreadID(), sess.hostedThreadID)
}

// activeTurn is one atomic snapshot of the monitor's live turn fields.
func (sess *session) activeTurnSnapshot() (observability.ActiveTurn, bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	return observability.ActiveTurn{Provider: sess.provider, ThreadID: sess.threadID,
		HostedThreadID: sess.hostedThreadID, Name: sess.displayName,
		TurnStartedAtMs: sess.turnStartedAtMs}, sess.workActiveLocked()
}

// withIdleDelivery holds the delivery lock from readiness through the caller's
// durable admission and queue update. Hosted admission remains with the caller.
func (sess *session) withIdleDelivery(rested bool, deliver func() error) (string, error) {
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
	sess.stateMu.Lock()
	status, permission := sess.connectionStatusLocked(), sess.lifecyclePermissionLocked()
	sess.stateMu.Unlock()
	// A resting thread had no open work at boot and queues behind its attach.
	restingIdle := rested && status == "starting"
	switch {
	case status == sessionStatusExited:
		return "detached", nil
	case sess.nanoIsStopping() || status != "ready" && !restingIdle:
		return "notReady", nil
	case !restingIdle && (permission.Submit != lifecycle.SendNow || sess.corePromptInFlight()):
		return "busy", nil
	}
	if sess.queuedTurns != nil {
		if _, queued := sess.queuedTurns.First(sess.provider, sess.ThreadID(), sess.hostedThreadID); queued {
			return "queuedBacklog", nil
		}
	}
	return "", deliver()
}

// busyForMaintenance reports live, restoring or delivering work. Locks do not
// extend into the caller's maintenance store operations.
func (sess *session) busyForMaintenance() bool {
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
