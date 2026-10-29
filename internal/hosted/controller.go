package hosted

import (
	"context"
	"net/http"
	"sync"

	"github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

// CloseStars supplies only the star reads and removal used by a tree close.
type CloseStars interface {
	IsStarred(string, string) bool
	UnstarMany([]hostedstore.AttachmentKey) error
}

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
	// Waiting and Reattach are construction-time boot actions until the lifecycle
	// landing moves boot ownership down. They do not expose the app server.
	Waiting                   func(string) string
	Reattach                  func(string)
	Stars                     CloseStars
	Views                     func() *ViewService
	ResolveIdentity           func(context.Context, string) (IdentityResolution, error)
	PrepareCreation           func(CreationRequest) (CreationPlan, error)
	PromptWithUnreadFooter    func(string, string) (string, error)
	UnreadFooterUnavailable   error
	FeatureFooter             func(string) (string, bool)
	CloseLifecycle            func(string, func() error) error
	NotifyRuntime             func()
	RefreshRuntime            func()
	PublishRelationshipChange func()
	LegacyCreate              http.HandlerFunc
	relationshipMu            sync.Mutex
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
