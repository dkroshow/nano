package hosted

import (
	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// ViewNames supplies the independent durable-name overlay for hosted metadata.
type ViewNames interface {
	// DurableName reads the value-only name contract.
	DurableName(string, string) (string, bool)
	// DurableNameRef reads the name and its provenance.
	DurableNameRef(string, string) (hostedstore.MetadataName, bool)
}

// ViewService assembles hosted read views from canonical facts and bounded runtime reads.
// The app supplies current dependency references for each read.
type ViewService struct {
	Executions                    *hostedstore.ExecutionStore
	Metadata                      *hostedstore.MetadataStore
	ExecutionError, MetadataError string
	Sessions                      *sessions.Registry
	Queue                         *prompts.LegacyQueue
	Names                         ViewNames
	Waiting                       func(string) string
	ResolveRecord                 func(string) (ownedthreads.SessionRecord, bool)
}

func (s *ViewService) sessionForHosted(id string) *sessions.Session {
	if s.Sessions == nil {
		return nil
	}
	return s.Sessions.SessionForHostedThread(id)
}
