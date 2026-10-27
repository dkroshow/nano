package interact

import (
	"context"
	"encoding/json"
)

const (
	AgentOrigin  = "agent"
	SystemOrigin = "system"
)

type SubmitPromptRequest struct {
	ThreadRef       string `json:"-"`
	Prompt          string `json:"prompt"`
	ClientID        string `json:"clientActionId"`
	Origin          string `json:"origin"`
	RequireIdle     bool   `json:"requireIdle,omitempty"`
	SenderThreadRef string `json:"senderThreadRef,omitempty"`
}

type Turn struct {
	ID             string `json:"id"`
	HostedThreadID string `json:"hostedThreadId"`
	ClientID       string `json:"clientActionId"`
	Status         string `json:"status"`
	Origin         string `json:"origin,omitempty"`
	Terminal       bool   `json:"terminal"`
	QueuePosition  int    `json:"queuePosition,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ProviderTurnID string `json:"providerTurnId,omitempty"`
	FinalResponse  string `json:"finalResponse,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
	Revision       int64  `json:"revision,omitempty"`
}

type HostedThreadCreateRequest struct {
	Provider              string         `json:"provider"`
	InvokerThreadRef      string         `json:"invokerThreadRef"`
	ClientSessionID       string         `json:"clientSessionId"`
	Name                  string         `json:"name"`
	CWD                   string         `json:"cwd"`
	Model                 string         `json:"model"`
	Effort                string         `json:"effort"`
	PresentationMode      string         `json:"presentationMode"`
	Subscriptions         map[string]any `json:"subscriptions"`
	Sandbox               string         `json:"sandbox"`
	Approval              string         `json:"approval"`
	BaseInstructions      string         `json:"baseInstructions,omitempty"`
	DeveloperInstructions string         `json:"developerInstructions,omitempty"`
	Config                map[string]any `json:"config"`
}

type HostedThread struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Status         string `json:"status"`
	Ready          bool   `json:"ready"`
	Working        bool   `json:"working"`
	TurnActive     bool   `json:"turnActive"`
	Revision       int64  `json:"revision"`
	Metadata       *struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
	} `json:"metadata,omitempty"`
}

type Identity struct {
	DisplayName               string `json:"displayName"`
	NativeThreadID            string `json:"nativeThreadId"`
	NativeThreadIDProvisional bool   `json:"nativeThreadIdProvisional"`
	HostedThreadID            string `json:"hostedThreadId"`
	Provider                  string `json:"provider"`
	MatchCount                int    `json:"matchCount"`
}

type PromptSubmitter interface {
	SubmitPrompt(context.Context, SubmitPromptRequest) (Turn, error)
}

type MessageRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type SendMessageRequest struct {
	RecipientThreadRef  string       `json:"-"`
	ClientMessageID     string       `json:"clientMessageId"`
	SenderThreadRef     string       `json:"senderThreadRef,omitempty"`
	Operator            bool         `json:"operator,omitempty"`
	Subject             string       `json:"subject,omitempty"`
	Body                string       `json:"body"`
	Delivery            string       `json:"delivery,omitempty"`
	Refs                []MessageRef `json:"refs,omitempty"`
	DispositionFeedback bool         `json:"dispositionFeedback,omitempty"`
}

type MessageSender interface {
	SendMessage(context.Context, SendMessageRequest) (json.RawMessage, error)
}

type MessagePageRequest struct {
	ThreadRef string
	ActorRef  string
	Since     int64
	Limit     int
	Status    string
	Ref       string
	Peek      bool
}

type PageRequest struct {
	ThreadRef string
	Cursor    string
	Limit     int
}

type CompletedTurn struct {
	Provider string `json:"provider"`
	// TurnID is the provider execution's identity: the feed cursor and the
	// operation's turn.
	TurnID string `json:"turnId"`
	// NativeThreadID and SavedTurnID name the saved transcript turn that holds
	// the answer, when it is not found by TurnID: a Claude answer's turn is
	// the saved turn that consumed its prompt.
	NativeThreadID string      `json:"nativeThreadId,omitempty"`
	SavedTurnID    string      `json:"savedTurnId,omitempty"`
	OperationID    string      `json:"operationId,omitempty"`
	FinalMessageID string      `json:"finalMessageId,omitempty"`
	Outcome        string      `json:"outcome"`
	Usage          *TokenUsage `json:"usage,omitempty"`
	CompletedAt    string      `json:"completedAt"`
}

type TokenUsage struct {
	InputTokens       *int64 `json:"inputTokens,omitempty"`
	CachedInputTokens *int64 `json:"cachedInputTokens,omitempty"`
	OutputTokens      *int64 `json:"outputTokens,omitempty"`
}

type CompletedTurnPage struct {
	Items      []CompletedTurn `json:"items"`
	NextCursor string          `json:"nextCursor"`
}

type CompletedTurnSubscriber interface {
	SubscribeCompletedTurns(context.Context, PageRequest) (CompletedTurnPage, error)
}

type TranscriptPage struct {
	HasMoreBefore bool    `json:"hasMoreBefore"`
	StartsMidTurn bool    `json:"startsMidTurn"`
	NextCursor    *string `json:"nextBeforeCursor"`
}

type TranscriptTurn struct {
	ID           string `json:"id"`
	NativeTurnID string `json:"nativeTurnId"`
}

type TranscriptWindow struct {
	ThreadID string            `json:"threadId"`
	Rows     []json.RawMessage `json:"items"`
	Turns    []TranscriptTurn  `json:"turns"`
	Page     TranscriptPage    `json:"page"`
}

type TranscriptReader interface {
	ReadTranscript(context.Context, PageRequest) (TranscriptWindow, error)
}

// ThreadDiagnostic is one content-free delivery or session fact, such
// as a prompt parked at boot. It is diagnostic, not terminal.
type ThreadDiagnostic struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Source      string `json:"source"`
	ActionID    string `json:"actionId,omitempty"`
	Occurrences uint64 `json:"occurrences"`
}

type LifecycleState struct {
	ThreadID            string             `json:"id"`
	Status              string             `json:"status"`
	Attached            bool               `json:"attached"`
	Ready               bool               `json:"ready"`
	Working             bool               `json:"working"`
	TurnActive          bool               `json:"turnActive"`
	Availability        string             `json:"availability"`
	AvailabilityReasons []string           `json:"availabilityReasons"`
	LastHumanMessageAt  string             `json:"lastHumanMessageAt,omitempty"`
	AttachmentState     string             `json:"attachmentState"`
	ThreadDiagnostics   []ThreadDiagnostic `json:"threadDiagnostics,omitempty"`
	Metadata            *struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
	} `json:"metadata,omitempty"`
}

type LifecycleReader interface {
	ReadLifecycle(context.Context, string) (LifecycleState, error)
	ReadAvailability(context.Context, string) (LifecycleState, error)
}
