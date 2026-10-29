package hosted

import (
	"context"
	"sync"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
)

// Controller owns hosted admission and relationship mutation ordering.
// Dependencies are fixed when wired; the stores retain the sole durable facts.
type Controller struct {
	Executions                    *hostedstore.ExecutionStore
	Metadata                      *hostedstore.MetadataStore
	MetadataError, ExecutionError string
	Attachments                   *hostedstore.AttachmentStore
	Queue                         *prompts.LegacyQueue
	Sessions                      *sessions.Registry
	SendAttempts                  *prompts.LegacyJournal
	ClaudeShimWS                  string
	Boot                          *sessions.BootAttachments
	AttachmentsControl            *AttachmentCoordinator
	Stars                         *sidebar.Stars
	Views                         func() *ViewService
	ResolveIdentity               func(context.Context, string) (IdentityResolution, error)
	PromptWithUnreadFooter        func(string, string) (string, error)
	UnreadFooterUnavailable       error
	FeatureFooter                 func(string) (string, bool)
	CloseLifecycle                func(string, func() error) error
	NotifyRuntime                 func()
	RefreshRuntime                func()
	PublishRelationshipChange     func()
	relationshipMu                sync.Mutex
}

func (s *Controller) controlStore() *hostedstore.MetadataStore {
	if s.Metadata != nil && s.MetadataError == "" {
		return s.Metadata
	}
	if s.Executions != nil {
		return s.Executions.Control()
	}
	return nil
}

func (s *Controller) sessionForHosted(id string) *sessions.Session {
	if s.Sessions == nil {
		return nil
	}
	return s.Sessions.SessionForHostedThread(id)
}
