package app

import (
	"context"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// ID returns the identity of this runtime attachment.
func (sess *session) ID() string { return sess.id }

// Provider returns the attachment's provider.
func (sess *session) Provider() string { return sess.provider }

// HostedThreadID returns the durable hosted identity carried by this attachment.
func (sess *session) HostedThreadID() string { return sess.hostedThreadID }

// PromptStore returns this attachment's canonical prompt store.
// It can be unavailable before the runtime is initialized.
func (sess *session) PromptStore() *prompts.Store { return sess.turns }

// LiveTurnActive reads the view's active-turn fact without exposing its state.
func (sess *session) LiveTurnActive() bool {
	return sess.projection != nil && sess.projection.TurnActive()
}

// PersistedOperation reads the hosted operation for one transport action.
func (sess *session) PersistedOperation(ctx context.Context, actionID string) (agentTurnRecord, bool, error) {
	if sess.agentExecutions == nil {
		return agentTurnRecord{}, false, nil
	}
	return sess.agentExecutions.PersistedTurnForTransportAction(ctx, sess.hostedThreadID, actionID)
}
