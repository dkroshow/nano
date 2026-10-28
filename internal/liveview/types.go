package liveview

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/interact"
)

// View selects the conversation rows requested by an HTTP client.
type View string

// Item is a provider-neutral item in the live transcript view.
type Item struct {
	ID               string         `json:"id"`
	NativeItemID     string         `json:"nativeItemID,omitempty"`
	DurableItemID    string         `json:"durableItemID,omitempty"`
	ClientActionID   string         `json:"clientActionId,omitempty"`
	ProjectedSeq     int64          `json:"projectedSeq,omitempty"`
	TurnID           string         `json:"turnId,omitempty"`
	TurnOrdinal      int            `json:"turnOrdinal,omitempty"`
	Type             string         `json:"type"`
	Category         string         `json:"category,omitempty"`
	Role             string         `json:"role,omitempty"`
	Phase            string         `json:"phase,omitempty"`
	Text             string         `json:"text,omitempty"`
	Status           string         `json:"status"`
	Done             bool           `json:"done"`
	CreatedAt        string         `json:"createdAt,omitempty"`
	ToolName         string         `json:"toolName,omitempty"`
	CallID           string         `json:"callID,omitempty"`
	ContentRef       string         `json:"contentRef,omitempty"`
	ContentTruncated bool           `json:"contentTruncated,omitempty"`
	OriginalBytes    int            `json:"originalBytes,omitempty"`
	Item             map[string]any `json:"item,omitempty"`
	DurationMS       int64          `json:"durationMs,omitempty"`
}

// Turn is the bounded live view of one provider turn.
type Turn struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	StartedAtMs   int64           `json:"startedAtMs,omitempty"`
	CompletedAtMs int64           `json:"completedAtMs,omitempty"`
	DurationMS    int64           `json:"durationMs,omitempty"`
	FinalItemID   string          `json:"finalItemID,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
}

// Runtime carries session activity alongside transcript updates.
type Runtime struct {
	SessionStatus     string          `json:"sessionStatus,omitempty"`
	ThreadStatus      string          `json:"threadStatus,omitempty"`
	TurnActive        bool            `json:"turnActive"`
	TurnID            string          `json:"turnId,omitempty"`
	TurnStartedAtMs   int64           `json:"turnStartedAtMs,omitempty"`
	CompactingSinceMs int64           `json:"compactingSinceMs,omitempty"`
	TokenUsage        json.RawMessage `json:"tokenUsage,omitempty"`
	LastError         string          `json:"lastError,omitempty"`
	// DeliveryExpiries counts the provider's recent delivery deadline
	// expiries, across all its threads (deliveryExpiries).
	DeliveryExpiries int `json:"deliveryExpiries,omitempty"`
}

// Snapshot is one complete live transcript view.
type Snapshot struct {
	Version        int                           `json:"version"`
	Provider       string                        `json:"provider"`
	NativeThreadID string                        `json:"nativeThreadID,omitempty"`
	Epoch          string                        `json:"epoch"`
	Revision       int64                         `json:"revision"`
	Items          []Item                        `json:"items"`
	Turns          []Turn                        `json:"turns"`
	Runtime        Runtime                       `json:"runtime"`
	Interactions   []interact.PendingInteraction `json:"interactions"`
	Page           *Page                         `json:"page,omitempty"`
}

// Page describes the saved-file page attached to a view.
type Page struct {
	HasMoreBefore    bool    `json:"hasMoreBefore"`
	NextBeforeCursor *string `json:"nextBeforeCursor,omitempty"`
	Source           string  `json:"source,omitempty"`
}

// Update is an ordered change to a live transcript view.
type Update struct {
	Version     int                          `json:"version"`
	Op          string                       `json:"op"`
	Epoch       string                       `json:"epoch"`
	Revision    int64                        `json:"revision"`
	Snapshot    *Snapshot                    `json:"snapshot,omitempty"`
	Runtime     *Runtime                     `json:"runtime,omitempty"`
	Turn        *Turn                        `json:"turn,omitempty"`
	Interaction *interact.PendingInteraction `json:"interaction,omitempty"`
	RequestID   string                       `json:"requestID,omitempty"`
}

// DurableTurnEvidence is the saved turn fact used to settle an open live turn.
type DurableTurnEvidence struct {
	Turn Turn
	// Newer reports a later saved conversation turn, which ends this one.
	Newer bool
}

// Status pairs the live runtime and pending interactions at one revision.
type Status struct {
	// Epoch names the projection; revisions order statuses within one.
	Epoch        string                        `json:"epoch"`
	Revision     int64                         `json:"revision"`
	Runtime      Runtime                       `json:"runtime"`
	Interactions []interact.PendingInteraction `json:"interactions"`
}
