package prompts

import "github.com/dkroshow/nano/internal/transcriptparse/model"

// DeliveryState describes the outbox record, not provider connection or work.
type DeliveryState string

const (
	Queued     DeliveryState = "queued"
	Delivering DeliveryState = "delivering"
	Accepted   DeliveryState = "accepted"
	Uncertain  DeliveryState = "uncertain"
	Confirmed  DeliveryState = "confirmed"
	Refused    DeliveryState = "refused"
	Parked     DeliveryState = "parked"
	// Expired is an old delivered prompt that exact identity never settled:
	// it is no longer shown, nor claimed delivered. Its record and text stay.
	Expired DeliveryState = "expired"
)

// Binding is exact persisted correlation, never inferred from prompt text.
// Scope keeps a native turn id distinct from an adapter's local turn key.
// A turn-only binding may identify its submitted user row, never its replies.
// Missing item/event ids remain unknown. Conflicting action claims cannot bind.
type Binding struct {
	Scope        model.Scope `json:"scope"`
	NativeItemID string      `json:"nativeItemId,omitempty"`
	EventID      string      `json:"eventId,omitempty"`
}

// OutboxEntry is the only prompts view transcript consumes. Sequence records
// submission order, not mutable queue rank. PresentationText excludes any
// provider-only footer. Confirmed means settled: the provider's saved
// transcript holds the prompt's user row by exact identity. Payload removal is
// never a replay side effect.
type OutboxEntry struct {
	HostedThreadID   string
	ActionID         string
	Sequence         uint64
	PresentationText string
	State            DeliveryState
	// Reason is the journal's reason for State, such as why a prompt is
	// uncertain or waits for the next turn.
	Reason  string
	Binding *Binding
	// UpdatedAt is when the journal last changed the prompt's state. While
	// it is Delivering, that is when delivery claimed it.
	UpdatedAt string
}
