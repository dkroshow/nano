package app

import (
	"errors"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

// spawnRequest is the POST /api/sessions body: everything the New Window
// dialog can pick. Empty fields fall back to server flag defaults (or the
// provider's own defaults).
type spawnRequest struct {
	sessionruntime.ProviderRequest
	ClientSessionID       string                   `json:"clientSessionId,omitempty"`  // optional idempotent creation alias
	InvokerThreadRef      string                   `json:"invokerThreadRef,omitempty"` // agent API only: caller's own hosted/native/current session reference
	Subscriptions         subscriptions.Choices    `json:"subscriptions,omitempty"`    // agent API only: completion delivery channels on the immutable invoker edge
	Assignment            *orchestrationAssignment `json:"assignment,omitempty"`       // optional caller-authored execution assignment
	InvokerHostedThreadID string                   `json:"-"`                          // normalized durable invocation edge
	InvokerNativeThreadID string                   `json:"-"`                          // provider-native snapshot for display/debugging
	InvokerProvider       string                   `json:"-"`                          // provider snapshot; the edge itself is cross-provider
	PresentationMode      string                   `json:"presentationMode,omitempty"` // shared | background
	SidebarPlacement      string                   `json:"sidebarPlacement,omitempty"` // invoker | standalone
	Name                  string                   `json:"name"`                       // optional user-assigned thread name, written once the native id binds
	PendingName           string                   `json:"-"`                          // boot-only operator rename staged before a real native id bound
	// retiredClaudeHandle is the provisional handle whose record an
	// interrupted re-key left beside the native's. Only an actual resume
	// retires it; a preview resolves the same request and must not.
	retiredClaudeHandle string `json:"-"`
	// Prioritized registrations must appear immediately even during fleet boot.
	immediateRuntimePublish bool `json:"-"`
	// restart is this thread's entry in the boot's restart record, if any.
	restart *restartThread `json:"-"`
	// catalogBackground queues the catalog holds of a thread the sidebar did
	// not show at boot behind foreground ones, for a bounded time
	// (catalog_attached.go).
	catalogBackground bool `json:"-"`
	// externalResume is set only by read-only native evidence verification when
	// this provider conversation has never had a Nano hosted record. Metadata is
	// retained separately because provider resume builders intentionally blank
	// settings that must not be sent as start overrides.
	externalResume         bool              `json:"-"`
	externalResumeCatalog  string            `json:"-"`
	externalResumeMetadata map[string]string `json:"-"`
}

// CreateHostedThreadRequest is the agent-facing creation shape. It shares the
// server's spawn decoder because hosted-thread creation is one constrained
// form of session creation.
type CreateHostedThreadRequest = spawnRequest

var errHostedControlUnavailable = errors.New("hosted control unavailable; new hosted sessions and resumes are disabled")
