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
func (delivery *Delivery) PromptStore() *prompts.Store { return delivery.turns }

// LiveTurnActive reads the view's active-turn fact without exposing its state.
func (views *View) LiveTurnActive() bool {
	return views.projection != nil && views.projection.turnActive()
}

// PersistedOperation reads the hosted operation for one transport action.
func (delivery *Delivery) PersistedOperation(ctx context.Context, actionID string) (threads.TurnRecord, bool, error) {
	if delivery.identity.agentExecutions == nil {
		return threads.TurnRecord{}, false, nil
	}
	return delivery.identity.agentExecutions.PersistedTurnForTransportAction(ctx, delivery.identity.hostedThreadID, actionID)
}

// CWD returns the working directory retained by this attachment.
func (sess *Session) CWD() string { return sess.state.cwd }

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

// State returns the attachment's atomic runtime state.
func (sess *Session) State() *State {
	if sess == nil {
		return nil
	}
	return &sess.state
}

// Runtime returns the attachment's concrete runtime owner.
func (sess *Session) Runtime() *Runtime {
	if sess == nil {
		return nil
	}
	return &sess.runtime
}

// Teardown returns the attachment's close and repair owner.
func (sess *Session) Teardown() *Teardown {
	if sess == nil {
		return nil
	}
	return &sess.teardown
}

// View returns the attachment's concrete view owner.
func (sess *Session) View() *View {
	if sess == nil {
		return nil
	}
	return &sess.view
}
