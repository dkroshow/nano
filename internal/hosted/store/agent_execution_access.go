package store

import (
	"context"
)

// Control returns the canonical metadata store backing these execution records.
func (s *ExecutionStore) Control() *MetadataStore { return s.control }

// SetPruneEligible connects retention to the durable prompt owner's eligibility read.
func (s *ExecutionStore) SetPruneEligible(read func(context.Context, string, []string) (map[string]bool, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneEligible = read
}

// RunID identifies the server run that created new execution records.
func (s *ExecutionStore) RunID() string { return s.runID }
