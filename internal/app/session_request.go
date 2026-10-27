package app

import (
	"encoding/json"
	"errors"

	"github.com/dkroshow/nano/internal/features/subscriptions"
)

// spawnRequest is the POST /api/sessions body: everything the New Window
// dialog can pick. Empty fields fall back to server flag defaults (or the
// provider's own defaults).
type spawnRequest struct {
	Provider              string                   `json:"provider"`
	Mode                  string                   `json:"mode"`                       // start | resume
	ThreadID              string                   `json:"threadId"`                   // resume target (native thread/session id)
	ClientSessionID       string                   `json:"clientSessionId,omitempty"`  // optional idempotent creation alias
	InvokerThreadRef      string                   `json:"invokerThreadRef,omitempty"` // agent API only: caller's own hosted/native/current session reference
	Subscriptions         subscriptions.Choices    `json:"subscriptions,omitempty"`    // agent API only: completion delivery channels on the immutable invoker edge
	Assignment            *orchestrationAssignment `json:"assignment,omitempty"`       // optional caller-authored execution assignment
	HostedThreadID        string                   `json:"-"`                          // server-owned durable execution identity
	InvokerHostedThreadID string                   `json:"-"`                          // normalized durable invocation edge
	InvokerNativeThreadID string                   `json:"-"`                          // provider-native snapshot for display/debugging
	InvokerProvider       string                   `json:"-"`                          // provider snapshot; the edge itself is cross-provider
	PresentationMode      string                   `json:"presentationMode,omitempty"` // shared | background
	SidebarPlacement      string                   `json:"sidebarPlacement,omitempty"` // invoker | standalone
	Cwd                   string                   `json:"cwd"`
	Model                 string                   `json:"model"`
	Effort                string                   `json:"effort"`         // codex + claude + grok reasoning effort
	ServiceTier           string                   `json:"serviceTier"`    // codex: priority / service tier id
	Name                  string                   `json:"name"`           // optional user-assigned thread name, written once the native id binds
	PendingName           string                   `json:"-"`              // boot-only operator rename staged before a real native id bound
	Sandbox               string                   `json:"sandbox"`        // codex
	Approval              string                   `json:"approval"`       // codex
	PermissionMode        string                   `json:"permissionMode"` // claude; grok start: auto | bypassPermissions
	AllowedTools          string                   `json:"allowedTools"`   // claude
	ClaudeBin             string                   `json:"-"`              // resolved server-side only
	ClaudeShimWS          string                   `json:"-"`              // resolved server-side only; empty = direct child
	ClaudeShimSpools      string                   `json:"-"`              // resolved server-side only; the shim's spool directory
	ClaudeArgs            []string                 `json:"-"`              // resolved server-side only; claude resume: the original child's argv
	GrokBin               string                   `json:"-"`              // resolved server-side only
	GrokLeaderSocket      string                   `json:"-"`              // empty uses Grok's short default ~/.grok/leader.sock
	// ClaudeAnchor is the attachment record's live-turn anchor, read at resolve
	// time because the attach replay rewrites the record before anything else
	// could read it. Zero-valued for a start, or for a record that names no
	// turn in flight.
	ClaudeAnchor attachmentRecord `json:"-"`
	// retiredClaudeHandle is the provisional handle whose record an
	// interrupted re-key left beside the native's. Only an actual resume
	// retires it; a preview resolves the same request and must not.
	retiredClaudeHandle string `json:"-"`
	// codexZeroTurnResume is boot-only: a supervised daemon can still hold a
	// thread that has not taken its first turn, but thread/resume cannot load it
	// because Codex writes no rollout until that turn. codexResumeModel restores
	// the attachment's display posture without sending it as a resume override.
	codexZeroTurnResume bool   `json:"-"`
	codexResumeModel    string `json:"-"`
	grokResumeModel     string `json:"-"`
	// bootReattach makes provider selection asynchronous after the session shell
	// is registered. Boot has no HTTP caller waiting for a synchronous resume
	// refusal, and one slow daemon must not hide every other remembered session.
	bootReattach *bootProviderControl `json:"-"`
	// Prioritized registrations must appear immediately even during fleet boot.
	immediateRuntimePublish bool `json:"-"`
	// restart is this thread's entry in the boot's restart record, if any.
	restart *restartThread `json:"-"`
	// catalogBackground queues the catalog holds of a thread the sidebar did
	// not show at boot behind foreground ones, for a bounded time
	// (catalog_attached.go).
	catalogBackground bool `json:"-"`
	// apiOrigin is this server's own HTTP origin. Starts use it to mint the
	// provider identity block; Claude fallback resumes use it to rebuild or
	// refresh that same immutable block when transport argv is absent or stale.
	apiOrigin string `json:"-"`
	// externalResume is set only by read-only native evidence verification when
	// this provider conversation has never had a Nano hosted record. Metadata is
	// retained separately because provider resume builders intentionally blank
	// settings that must not be sent as start overrides.
	externalResume         bool              `json:"-"`
	externalResumePosture  bool              `json:"-"`
	externalResumeCatalog  string            `json:"-"`
	externalResumeMetadata map[string]string `json:"-"`

	// Advanced codex thread/start params (docs/thread-start-parameters §1/§7).
	Personality           string          `json:"personality"`           // none | friendly | pragmatic
	ApprovalsReviewer     string          `json:"approvalsReviewer"`     // user | auto_review
	Ephemeral             bool            `json:"ephemeral"`             // true → no rollout on disk, no resume
	BaseInstructions      string          `json:"baseInstructions"`      // replaces built-in system-prompt layer
	DeveloperInstructions string          `json:"developerInstructions"` // injected at developer role
	ModelProvider         string          `json:"modelProvider"`         // key into config [model_providers]
	Config                json.RawMessage `json:"config"`                // per-thread config.toml overrides (object)
	ThreadSource          string          `json:"threadSource"`          // client analytics classification
	SessionStartSource    string          `json:"sessionStartSource"`    // startup | clear
	ServiceName           string          `json:"serviceName"`           // analytics service name
}

// CreateHostedThreadRequest is the agent-facing creation shape. It shares the
// server's spawn decoder because hosted-thread creation is one constrained
// form of session creation.
type CreateHostedThreadRequest = spawnRequest

var errHostedControlUnavailable = errors.New("hosted control unavailable; new hosted sessions and resumes are disabled")
