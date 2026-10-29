package app

import (
	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// newControlAgentExecutionStore composes feature admission with durable execution loading.
func newControlAgentExecutionStore(control *hostedstore.MetadataStore, runID string) (*hostedstore.ExecutionStore, error) {
	return hostedstore.NewExecutionStore(control, runID, subscriptions.IsFeatureAction)
}
