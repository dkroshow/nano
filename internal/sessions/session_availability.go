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
func (attachmentState *State) AvailabilityState() Availability {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return Availability{
		Status: attachmentState.connectionStatusLocked(), Active: attachmentState.workActiveLocked(),
		Waiting: attachmentState.waitingLocked(), Permission: attachmentState.lifecyclePermissionLocked(),
		LastHumanPromptAt: attachmentState.lastHumanPromptAt,
	}
}

// QueuedPromptCount counts prompts waiting on this attachment.
func (delivery *Delivery) QueuedPromptCount() int {
	if delivery.queuedTurns == nil {
		return 0
	}
	return delivery.queuedTurns.QueuedCount(delivery.identity.provider, delivery.state.ThreadID(), delivery.identity.hostedThreadID)
}

// ActiveTurnSnapshot returns one atomic snapshot of the live turn fields.
// activeTurn is one atomic snapshot of the monitor's live turn fields.
func (attachmentState *State) ActiveTurnSnapshot() (observability.ActiveTurn, bool) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	return observability.ActiveTurn{Provider: attachmentState.identity.provider, ThreadID: attachmentState.threadID,
		HostedThreadID: attachmentState.identity.hostedThreadID, Name: attachmentState.displayName,
		TurnStartedAtMs: attachmentState.turnStartedAtMs}, attachmentState.workActiveLocked()
}

// WithIdleDelivery holds the delivery lock from readiness through the caller's
// durable admission and queue update. Hosted admission remains with the caller.
func (delivery *Delivery) WithIdleDelivery(rested bool, deliver func() error) (string, error) {
	delivery.sendMu.Lock()
	defer delivery.sendMu.Unlock()
	status, permission := delivery.state.deliveryPermission()
	// A resting thread had no open work at boot and queues behind its attach.
	restingIdle := rested && status == "starting"
	switch {
	case status == StatusExited:
		return "detached", nil
	case delivery.nanoIsStopping() || status != "ready" && !restingIdle:
		return "notReady", nil
	case !restingIdle && (permission.Submit != lifecycle.SendNow || delivery.runtime.CorePromptInFlight()):
		return "busy", nil
	}
	if delivery.queuedTurns != nil {
		if _, queued := delivery.queuedTurns.First(delivery.identity.provider, delivery.state.ThreadID(), delivery.identity.hostedThreadID); queued {
			return "queuedBacklog", nil
		}
	}
	return "", deliver()
}

// BusyForMaintenance reports live, restoring or delivering work. Locks do not
// extend into the caller's maintenance store operations.
func (delivery *Delivery) BusyForMaintenance() bool {
	active := delivery.state.maintenanceActive()
	if active {
		return true
	}
	delivery.sendMu.Lock()
	defer delivery.sendMu.Unlock()
	for _, op := range delivery.sendOps {
		select {
		case <-op.done:
		default:
			return true
		}
	}
	return false
}
