package app

import hostedstore "github.com/dkroshow/nano/internal/hosted"

// newControlAgentExecutionStore composes feature admission with durable execution loading.
func newControlAgentExecutionStore(control *controlMetadataStore, runID string) (*agentExecutionStore, error) {
	return hostedstore.NewExecutionStore(control, runID, serverFeatureAction)
}
