package app

import "encoding/json"

// providerRequest is the resolved launch input consumed by provider transports.
// Hosted admission and caller-authored orchestration stay with spawnRequest.
type providerRequest struct {
	Provider         string   `json:"provider"`
	Mode             string   `json:"mode"`     // start | resume
	ThreadID         string   `json:"threadId"` // resume target (native thread/session id)
	HostedThreadID   string   `json:"-"`        // server-owned durable execution identity
	Cwd              string   `json:"cwd"`
	Model            string   `json:"model"`
	Effort           string   `json:"effort"`         // codex + claude + grok reasoning effort
	ServiceTier      string   `json:"serviceTier"`    // codex: priority / service tier id
	Sandbox          string   `json:"sandbox"`        // codex
	Approval         string   `json:"approval"`       // codex
	PermissionMode   string   `json:"permissionMode"` // claude; grok start: auto | bypassPermissions
	AllowedTools     string   `json:"allowedTools"`   // claude
	ClaudeBin        string   `json:"-"`              // resolved server-side only
	ClaudeShimWS     string   `json:"-"`              // resolved server-side only; empty = direct child
	ClaudeShimSpools string   `json:"-"`              // resolved server-side only; the shim's spool directory
	ClaudeArgs       []string `json:"-"`              // resolved server-side only; claude resume: the original child's argv
	GrokBin          string   `json:"-"`              // resolved server-side only
	GrokLeaderSocket string   `json:"-"`              // empty uses Grok's short default ~/.grok/leader.sock
	// ClaudeAnchor is the attachment record's live-turn anchor, read at resolve
	// time because the attach replay rewrites the record before anything else
	// could read it. Zero-valued for a start, or for a record that names no
	// turn in flight.
	ClaudeAnchor attachmentRecord `json:"-"`
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
	// apiOrigin is this server's own HTTP origin. Starts use it to mint the
	// provider identity block; Claude fallback resumes use it to rebuild or
	// refresh that same immutable block when transport argv is absent or stale.
	apiOrigin             string `json:"-"`
	externalResumePosture bool   `json:"-"`

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
