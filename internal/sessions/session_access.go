package sessions

import (
	"context"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/threads"
)

// ID returns the identity of this runtime attachment.
func (sess *Session) ID() string { return sess.identity.id }

// Provider returns the attachment's provider.
func (sess *Session) Provider() string { return sess.identity.provider }

// HostedThreadID returns the durable hosted identity carried by this attachment.
func (sess *Session) HostedThreadID() string { return sess.identity.hostedThreadID }

// PromptStore returns this attachment's canonical prompt store.
// It can be unavailable before the runtime is initialized.
func (sess *Delivery) PromptStore() *prompts.Store { return sess.turns }

// LiveTurnActive reads the view's active-turn fact without exposing its state.
func (sess *View) LiveTurnActive() bool {
	return sess.projection != nil && sess.projection.turnActive()
}

// PersistedOperation reads the hosted operation for one transport action.
func (sess *Delivery) PersistedOperation(ctx context.Context, actionID string) (threads.TurnRecord, bool, error) {
	if sess.identity.agentExecutions == nil {
		return threads.TurnRecord{}, false, nil
	}
	return sess.identity.agentExecutions.PersistedTurnForTransportAction(ctx, sess.identity.hostedThreadID, actionID)
}

// CWD returns the working directory retained by this attachment.
func (sess *Session) CWD() string { return sess.stateOwner.cwd }

// Wire returns the attachment's concrete wire owner.
func (sess *Session) Wire() *Wire {
	if sess == nil {
		return nil
	}
	return &sess.wire
}

// Delivery returns the attachment's concrete delivery owner.
func (sess *Session) Delivery() *Delivery {
	if sess == nil {
		return nil
	}
	return &sess.delivery
}

// State returns the attachment's atomic runtime state owner.
func (sess *Session) State() *State {
	if sess == nil {
		return nil
	}
	return &sess.stateOwner
}

// Runtime returns the attachment's concrete runtime owner.
func (sess *Session) Runtime() *Runtime {
	if sess == nil {
		return nil
	}
	return &sess.runtime
}

// Lifecycle returns the attachment's concrete lifecycle owner.
func (sess *Session) Lifecycle() *Lifecycle {
	if sess == nil {
		return nil
	}
	return &sess.lifecycle
}

// View returns the attachment's concrete view owner.
func (sess *Session) View() *View {
	if sess == nil {
		return nil
	}
	return &sess.view
}
