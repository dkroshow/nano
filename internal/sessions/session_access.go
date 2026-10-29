package sessions

import (
	"context"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/threads"
)

// ID returns the identity of this runtime attachment.
func (sess *Session) ID() string { return sess.id }

// Provider returns the attachment's provider.
func (sess *Session) Provider() string { return sess.provider }

// HostedThreadID returns the durable hosted identity carried by this attachment.
func (sess *Session) HostedThreadID() string { return sess.hostedThreadID }

// PromptStore returns this attachment's canonical prompt store.
// It can be unavailable before the runtime is initialized.
func (sess *Session) PromptStore() *prompts.Store { return sess.turns }

// LiveTurnActive reads the view's active-turn fact without exposing its state.
func (sess *Session) LiveTurnActive() bool {
	return sess.projection != nil && sess.projection.turnActive()
}

// PersistedOperation reads the hosted operation for one transport action.
func (sess *Session) PersistedOperation(ctx context.Context, actionID string) (threads.TurnRecord, bool, error) {
	if sess.agentExecutions == nil {
		return threads.TurnRecord{}, false, nil
	}
	return sess.agentExecutions.PersistedTurnForTransportAction(ctx, sess.hostedThreadID, actionID)
}

// CWD returns the working directory retained by this attachment.
func (sess *Session) CWD() string { return sess.cwd }

// Wire returns the attachment's concrete wire owner.
func (sess *Session) Wire() *Wire {
	if sess == nil {
		return nil
	}
	return &sess.wire
}
