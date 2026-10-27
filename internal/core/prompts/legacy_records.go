package prompts

// CompletedTurn exists only for the lossless schema-20 import. Runtime turn
// writes belong to internal/core/turns.
type CompletedTurn struct {
	ID, TargetHostedThreadID, Provider, TurnID, FinalMessageID, CompletedAt, Source, CreatedAt string
}

// QueuedTurn retains caller text until the existing confirmation path proves
// that the exact provider turn owns it. Position is stored separately as FIFO.
type QueuedTurn struct {
	ClientActionID string `json:"clientActionId"`
	Provider       string `json:"provider"`
	HostedThreadID string `json:"hostedThreadId,omitempty"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Text           string `json:"text"`
	// PresentationText is the server-owned browser row. For agent-origin
	// prompts it excludes Nano's provider-only attribution and inbox footers.
	// Empty legacy rows fall back to Text during recovery.
	PresentationText string `json:"presentationText,omitempty"`
	TextHash         string `json:"textHash"`
	// RequestHash covers caller-owned text and admission options. TextHash
	// covers the exact provider-bound text, including Nano footers. Legacy rows
	// predate footers and therefore use TextHash for both meanings.
	RequestHash string `json:"requestHash,omitempty"`
	State       string `json:"state,omitempty"`
	// Origin is the submission class: human (UI), agent (agent API), or
	// system (Nano-generated). Empty means legacy/human. Delivery order is by
	// class — human before agent before system — then FIFO within a class
	//.
	Origin    string `json:"origin,omitempty"`
	CreatedAt string `json:"createdAt"`
	// ProviderTurnID is the exact Codex turn/start identity for this action.
	// Missing on older records; treat as unknown, never infer from text.
	ProviderTurnID         string `json:"providerTurnId,omitempty"`
	ProviderTerminalStatus string `json:"providerTerminalStatus,omitempty"`
	ProviderTerminalAt     string `json:"providerTerminalAt,omitempty"`
}

// SendAttempt is the hash-only delivery journal, including exact identity and
// conflict evidence. Storage never derives confirmation from these fields.
type SendAttempt struct {
	ClientActionID string `json:"clientActionId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId"`
	TextHash       string `json:"textHash"`
	Status         string `json:"status"`
	RunID          string `json:"runId"`
	Reason         string `json:"reason,omitempty"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	// Confirmed records that provider transcript evidence was joined to this
	// action. The journal stays hash-only, so this bit is the only thing a pane
	// loading after the transient send.confirmed event can read to know that its
	// textless accepted record already landed.
	Confirmed   bool   `json:"confirmed,omitempty"`
	ConfirmedAt string `json:"confirmedAt,omitempty"`
	// ProviderTurnID is the exact provider turn bound to this action. Codex emits
	// it from turn/start, Claude uses Nano's persisted synthetic ordinal, and Grok
	// uses its prompt id. Optional on older records; required for
	// interrupted-before-transcript settlement.
	ProviderTurnID         string `json:"providerTurnId,omitempty"`
	ProviderTerminalStatus string `json:"providerTerminalStatus,omitempty"`
	ProviderTerminalAt     string `json:"providerTerminalAt,omitempty"`
	// CanonicalMessageID is the durable normalized source-lineage identity once
	// catalog evidence arrives. IdentityConflict is explicit evidence that one
	// action was delivered into more than one provider turn; it is never healed
	// by text matching or by rebinding the action.
	CanonicalMessageID string `json:"canonicalMessageId,omitempty"`
	IdentityConflict   string `json:"identityConflict,omitempty"`
}
