package hosted

import (
	"context"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// ViewService assembles hosted read views from canonical stores, saved names
// and the concrete session registry.
// The app supplies current dependency references for each read.
type ViewService struct {
	Executions                    *hostedstore.ExecutionStore
	Metadata                      *hostedstore.MetadataStore
	ExecutionError, MetadataError string
	Sessions                      *sessions.Registry
	Queue                         *prompts.LegacyQueue
	Catalog                       *threadindex.Service
	Boot                          *sessions.BootAttachments
	ResolveRecord                 func(string) (ownedthreads.SessionRecord, bool)
}

func (s *ViewService) sessionForHosted(id string) *sessions.Session {
	if s.Sessions == nil {
		return nil
	}
	return s.Sessions.SessionForHostedThread(id)
}

func (s *ViewService) durableThreadName(provider, native string) (string, bool) {
	if s.Catalog == nil || native == "" {
		return "", false
	}
	return s.Catalog.ThreadNameFor(context.Background(), provider, native)
}
