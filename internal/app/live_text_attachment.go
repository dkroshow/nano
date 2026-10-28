package app

import (
	"context"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// liveTextAttachment supplies attachment facts without exposing session state.
// The loop owns saved/live reconciliation and only reads through this boundary.
type liveTextAttachment interface {
	// Provider returns the provider of the live attachment.
	Provider() string
	// HostedThreadID returns the durable hosted identity.
	HostedThreadID() string
	// ThreadID reads the current native provider identity.
	ThreadID() string
	// PromptStore returns the canonical prompt store, when initialized.
	PromptStore() *prompts.Store
	// LiveTurnActive reports the view’s current active-turn fact.
	LiveTurnActive() bool
	// PersistedOperation reads the hosted operation for a transport action.
	PersistedOperation(context.Context, string) (agentTurnRecord, bool, error)
}
