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
