package app

import (
	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// newControlAgentExecutionStore composes feature admission with durable execution loading.
func newControlAgentExecutionStore(control *controlMetadataStore, runID string) (*agentExecutionStore, error) {
	return hostedstore.NewExecutionStore(control, runID, subscriptions.IsFeatureAction)
}
