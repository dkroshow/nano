package hosted

import (
	"context"
	"net/http"
	"sync"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// AdmissionSession is the bounded delivery action surface used during hosted admission.
// WithIdleDelivery keeps the session's delivery fence through the supplied durable admission.
type AdmissionSession interface {
	WithIdleDelivery(bool, func() error) (string, error)
	StartPrepared(string, string, string, string, string, bool) (bool, int, string, error)
	StartLockedPrepared(string, string, string, string, string, bool) (bool, int, string, error)
	QueuedText(string) (string, bool)
	NotifyRuntime()
}

// CloseSession pins one live attachment for explicit close and detach actions.
type CloseSession interface {
	ID() string
	Provider() string
	ThreadID() string
	HostedThreadID() string
	Current() bool
	VerifyClaudeAttachment(string) error
	CloseWithResult() error
	IsClosed() bool
	RemoveCurrent()
	ConfirmClaudeClose()
	Detach()
}

// ControlRuntime supplies bounded session actions; provider state and locks stay with sessions.
type ControlRuntime interface {
	Admission(string) AdmissionSession
	CloseSession(string) CloseSession
	SetPresentation(string, string)
	SetSidebarPlacement(string, string)
	AttachmentSession(string, string) CloseSession
	ResolveLiveRecord(string, func(string) (agentSessionRecord, bool)) (agentSessionRecord, bool)
	Waiting(string) string
	Reattach(string)
	StopDetached(Attachment) error
	ForgetDetached(string, func() error) error
	HasDeliveryEvidence(string, string) bool
	DeliveryWasAttempted(string, string) bool
	CancelPrompt(string, string) (bool, error)
	CancelStoredPrompt(string) error
	CheckRetiredReferences(map[string]bool, func() error) error
}

// CloseStars supplies only the star reads and removal used by a tree close.
type CloseStars interface {
	IsStarred(string, string) bool
	UnstarMany([]AttachmentKey) error
}

// Controller owns hosted admission and relationship mutation ordering.
// Dependencies are fixed when wired; the stores retain the sole durable facts.
type Controller struct {
	Executions                    *ExecutionStore
	Metadata                      *MetadataStore
	MetadataError, ExecutionError string
	Attachments                   *AttachmentStore
	Queue                         *prompts.LegacyQueue
	Runtime                       ControlRuntime
	Stars                         CloseStars
	Views                         func() *ViewService
	ResolveIdentity               func(context.Context, string) (IdentityResolution, error)
	PrepareCreation               func(CreationRequest) (CreationPlan, error)
	PromptWithUnreadFooter        func(string, string) (string, error)
	UnreadFooterUnavailable       error
	FeatureFooter                 func(string) (string, bool)
	CloseLifecycle                func(string, func() error) error
	NotifyRuntime                 func()
	RefreshRuntime                func()
	PublishRelationshipChange     func()
	LegacyCreate                  http.HandlerFunc
	relationshipMu                sync.Mutex
}

func (s *Controller) controlStore() *MetadataStore {
	if s.Metadata != nil && s.MetadataError == "" {
		return s.Metadata
	}
	if s.Executions != nil {
		return s.Executions.Control()
	}
	return nil
}
