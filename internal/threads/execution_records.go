package threads

import "encoding/json"

// ExecutionError, SessionRecord and TurnRecord are durable values. They carry
// identity, delivery evidence and settings across storage boundaries without
// importing the runtime, provider protocols or lifecycle. Historical status
// fields describe recorded observations, never current connection/work truth.
type ExecutionError struct {
	Source  string          `json:"source"`
	Code    string          `json:"code,omitempty"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

type SessionRecord struct {
	ID                    string `json:"id"`
	ClientSessionID       string `json:"clientSessionId,omitempty"`
	RequestHash           string `json:"requestHash"`
	Provider              string `json:"provider"`
	PresentationMode      string `json:"presentationMode"`
	SidebarPlacement      string `json:"sidebarPlacement"`
	NanoSessionID         string `json:"nanoSessionId,omitempty"`
	NativeThreadID        string `json:"nativeThreadId,omitempty"`
	InvokerHostedThreadID string `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string `json:"invokerProvider,omitempty"`
	SubscriptionInbox     bool   `json:"subscriptionInbox"`
	SubscriptionPrompt    bool   `json:"subscriptionPrompt"`
	LastHumanPromptAt     string `json:"lastHumanPromptAt,omitempty"`
	Status                string `json:"status"`
	Error                 string `json:"error,omitempty"`
	RunID                 string `json:"runId"`
	CreatedAt             string `json:"createdAt"`
	UpdatedAt             string `json:"updatedAt"`
	Revision              int64  `json:"revision"`
}

type TurnRecord struct {
	ID                     string          `json:"id"`
	HostedThreadID         string          `json:"hostedThreadId"`
	LegacyAgentSessionID   string          `json:"agentSessionId,omitempty"`
	ClientActionID         string          `json:"clientActionId"`
	TransportActionID      string          `json:"transportActionId"`
	PromptHash             string          `json:"promptHash"`
	AssignmentEventID      *int64          `json:"-"`
	Provider               string          `json:"provider"`
	NanoSessionID          string          `json:"nanoSessionId,omitempty"`
	NativeThreadID         string          `json:"nativeThreadId,omitempty"`
	ProviderTurnID         string          `json:"providerTurnId,omitempty"`
	Status                 string          `json:"status"`
	Error                  *ExecutionError `json:"error,omitempty"`
	FinalItemID            string          `json:"finalItemId,omitempty"`
	FinalResponse          string          `json:"finalResponse,omitempty"`
	FinalResponseTruncated bool            `json:"finalResponseTruncated,omitempty"`
	FinalResponseBytes     int             `json:"finalResponseBytes,omitempty"`
	CandidateItemID        string          `json:"candidateItemId,omitempty"`
	CandidatePhase         string          `json:"candidatePhase,omitempty"`
	CandidateResponse      string          `json:"candidateResponse,omitempty"`
	CandidateBytes         int             `json:"candidateBytes,omitempty"`
	CandidateTruncated     bool            `json:"candidateTruncated,omitempty"`
	// Attribution: who submitted this prompt. Origin is
	// human | agent | system; sender fields are the send-time snapshot of the
	// submitting agent, empty for human and system origins.
	Origin               string `json:"origin,omitempty"`
	SenderHostedThreadID string `json:"senderHostedThreadId,omitempty"`
	SenderNativeThreadID string `json:"senderNativeThreadId,omitempty"`
	SenderProvider       string `json:"senderProvider,omitempty"`
	SenderName           string `json:"senderName,omitempty"`
	// SenderSubmittedAt is Nano's server-side UTC admission timestamp for an
	// agent-origin prompt. It is captured before the prompt can enter a queue
	// and remains stable across retries and provider delivery.
	SenderSubmittedAt string `json:"senderSubmittedAt,omitempty"`
	RunID             string `json:"runId"`
	CreatedAt         string `json:"createdAt"`
	UpdatedAt         string `json:"updatedAt"`
	Revision          int64  `json:"revision"`
}
