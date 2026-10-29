package hosted

import (
	"net/http"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/sessions"
)

const (
	availabilityBusy        = "busy"
	availabilityAvailable   = "available"
	availabilityUnreachable = "unreachable"
)

type availabilityFacts struct {
	TurnActive               bool   `json:"turnActive"`
	TurnActiveMayUnderReport bool   `json:"turnActiveMayUnderReport"`
	QueuedPrompts            int    `json:"queuedPrompts"`
	PendingInteraction       bool   `json:"pendingInteraction"`
	LastHumanMessageAt       string `json:"lastHumanMessageAt,omitempty"`
	Attachment               string `json:"attachment"` // live | detached | exited | deleted
	SessionStatus            string `json:"sessionStatus,omitempty"`
}

// AvailabilityView reports whether a hosted thread can currently accept a prompt.
type AvailabilityView struct {
	HostedThreadID string            `json:"hostedThreadId"`
	State          string            `json:"state"`
	Reasons        []string          `json:"reasons"`
	Facts          availabilityFacts `json:"facts"`
	ObservedAt     string            `json:"observedAt"`
}

// HandleAvailability serves the hosted availability facts and their derived state.
func (s *ViewService) HandleAvailability(w http.ResponseWriter, r *http.Request, hostedThreadID string) {
	httpapi.WriteJSON(w, s.AvailabilityFor(hostedThreadID, time.Now().UTC()))
}

// AvailabilityFor derives the state. Connection takes precedence over work;
// waiting takes precedence over busy work. A recent human message is exposed as a fact,
// but it does not change availability: elapsed time is not a stable signal of
// which party owns the next move.
func (s *ViewService) AvailabilityFor(hostedThreadID string, now time.Time) AvailabilityView {
	view := AvailabilityView{HostedThreadID: hostedThreadID, ObservedAt: now.Format(time.RFC3339Nano), Reasons: []string{}}
	view.Facts.TurnActiveMayUnderReport = true
	record, ok := s.Executions.Session(hostedThreadID)
	if !ok {
		view.State, view.Facts.Attachment = availabilityUnreachable, "deleted"
		view.Reasons = append(view.Reasons, "unknown_or_deleted")
		return view
	}
	view.Facts.LastHumanMessageAt = record.LastHumanPromptAt
	live := s.sessionForHosted(hostedThreadID)
	attached := live != nil
	var state sessions.Availability
	var pending bool
	var queued int
	if attached {
		state = live.AvailabilityState()
		pending = live.CorePromptInFlight()
		queued = live.QueuedPromptCount()
	}
	if !attached && record.Status == hostedstore.StatusExited {
		view.State, view.Facts.Attachment, view.Facts.SessionStatus = availabilityUnreachable, "exited", record.Status
		view.Reasons = append(view.Reasons, "exited")
		return view
	}
	_, bootState := s.Boot.WaitingHosted(hostedThreadID)
	switch {
	case !attached && bootState == "starting":
		view.State, view.Facts.Attachment, view.Facts.SessionStatus = availabilityBusy, "starting", "starting"
		view.Reasons = append(view.Reasons, "session_starting")
		return view
	case !attached && bootState == "resting":
		// A resting thread has no work open (bootWorkOpen), and a prompt to
		// it attaches it first; an idle-only one queues behind that attach.
		view.State, view.Facts.Attachment, view.Facts.SessionStatus = availabilityAvailable, "resting", "resting"
		view.Reasons = append(view.Reasons, "attaches_on_demand")
		return view
	}
	if !attached {
		view.State, view.Facts.Attachment, view.Facts.SessionStatus = availabilityUnreachable, "detached", record.Status
		view.Reasons = append(view.Reasons, "detached")
		return view
	}
	view.Facts.Attachment = "live"
	status, active, waiting := state.Status, state.Active, state.Waiting
	permission, lastHuman := state.Permission, state.LastHumanPromptAt
	view.Facts.SessionStatus = status
	view.Facts.TurnActive = active
	view.Facts.QueuedPrompts = queued
	view.Facts.PendingInteraction = waiting
	if !lastHuman.IsZero() && (view.Facts.LastHumanMessageAt == "" || lastHuman.Format(time.RFC3339Nano) > view.Facts.LastHumanMessageAt) {
		view.Facts.LastHumanMessageAt = lastHuman.UTC().Format(time.RFC3339Nano)
	}

	view.State = string(permission.Availability)
	switch {
	case permission.Availability == lifecycle.Unavailable || permission.Availability == lifecycle.Invalid:
		view.State = availabilityUnreachable
		view.Reasons = append(view.Reasons, status)
	case status != "ready":
		view.Reasons = append(view.Reasons, "session_"+status)
	case waiting:
		view.Reasons = append(view.Reasons, "pending_interaction")
	case active:
		view.Reasons = append(view.Reasons, "turn_active")
	case pending:
		view.State = availabilityBusy
		view.Reasons = append(view.Reasons, "prompt_in_flight")
	case view.Facts.QueuedPrompts > 0:
		view.State = availabilityBusy
		view.Reasons = append(view.Reasons, "queued_backlog")
	}
	return view
}
