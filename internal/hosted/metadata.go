package hosted

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/threads"
)

// MetadataEvent is immutable hosted metadata evidence and its commit result.
type MetadataEvent struct {
	ID                   int64
	HostedThreadID       string
	Field                string
	Value                any
	Disposition          string
	Source               string
	Evidence             string
	NativeThreadID       string
	NanoSessionID        string
	AttachmentGeneration int64
	Freshness            string
	CausationEventID     *int64
	SourceEventKey       string
	ObservedAt           string
	CommittedAt          string
	// Committed is true only when this invocation inserted immutable evidence
	// and advanced the hosted revision. Stable retries and semantic duplicates
	// return the established event with Committed false.
	Committed bool
}

// MetadataEventPage is a bounded page of hosted metadata evidence.
type MetadataEventPage struct {
	Events     []MetadataEventView `json:"events"`
	NextCursor int64               `json:"nextCursor,omitempty"`
}

// MetadataEventView is the public evidence view without the internal retry key.
type MetadataEventView struct {
	ID                   int64           `json:"id"`
	HostedThreadID       string          `json:"hostedThreadId"`
	Field                string          `json:"field"`
	Value                json.RawMessage `json:"value"`
	Disposition          string          `json:"disposition"`
	Source               string          `json:"source"`
	Evidence             string          `json:"evidence,omitempty"`
	NativeThreadID       string          `json:"nativeThreadId,omitempty"`
	NanoSessionID        string          `json:"nanoSessionId,omitempty"`
	AttachmentGeneration int64           `json:"attachmentGeneration"`
	Freshness            string          `json:"freshness"`
	CausationEventID     *int64          `json:"causationEventId,omitempty"`
	ObservedAt           string          `json:"observedAt"`
	CommittedAt          string          `json:"committedAt"`
}

// MetadataEventFilter selects metadata evidence at the hosted read boundary.
type MetadataEventFilter struct {
	Field       string
	Source      string
	Disposition string
	Freshness   string
}

// MetadataCurrent holds desired, effective and display evidence for one field.
type MetadataCurrent struct {
	DesiredValue       json.RawMessage `json:"desiredValue,omitempty"`
	DesiredDisposition string          `json:"desiredDisposition"`
	DesiredSource      string          `json:"desiredSource,omitempty"`
	DesiredEventID     *int64          `json:"desiredEventId,omitempty"`
	// DesiredSettlementEventID is deliberately separate from DesiredEventID:
	// the latter remains the operator/creation decision that established the
	// request, while this identifies provider evidence that settled it.
	DesiredSettlementEventID *int64          `json:"desiredSettlementEventId,omitempty"`
	EffectiveValue           json.RawMessage `json:"effectiveValue,omitempty"`
	EffectiveDisposition     string          `json:"effectiveDisposition"`
	EffectiveSource          string          `json:"effectiveSource,omitempty"`
	EffectiveEventID         *int64          `json:"effectiveEventId,omitempty"`
	// Display is the single server-authored projection used by API consumers.
	// Desired intent remains separately available for audit and resume, but a
	// historical or replayed request yields to an observed effective value.
	DisplayValue       json.RawMessage `json:"displayValue,omitempty"`
	DisplayDisposition string          `json:"displayDisposition"`
	DisplaySource      string          `json:"displaySource,omitempty"`
	DisplayFreshness   string          `json:"displayFreshness,omitempty"`
	UpdatedAt          string          `json:"updatedAt"`
}

// MetadataView is the canonical hosted read model with runtime and name overlays.
type MetadataView struct {
	HostedThreadID        string                `json:"hostedThreadId"`
	Revision              int64                 `json:"revision"`
	Provider              string                `json:"provider"`
	NativeThreadID        string                `json:"nativeThreadId,omitempty"`
	AttachmentGeneration  int64                 `json:"attachmentGeneration"`
	PresentationMode      string                `json:"presentationMode"`
	SidebarPlacement      string                `json:"sidebarPlacement"`
	InvokerHostedThreadID string                `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string                `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string                `json:"invokerProvider,omitempty"`
	Subscriptions         subscriptions.Choices `json:"subscriptions"`
	CreatedAt             string                `json:"createdAt"`
	UpdatedAt             string                `json:"updatedAt"`
	NativeIDHistory       []NativeIDView        `json:"nativeIdHistory"`
	// Attachment is joined by the server from live runtime/execution control.
	// It is intentionally not a SQLite metadata fact and never changes Revision.
	Attachment AttachmentView `json:"attachment"`
	// Name is joined by the server from the durable name store.  It is not an
	// event fact because names have their own provenance and mutation surface.
	Name   MetadataName               `json:"name"`
	Fields map[string]MetadataCurrent `json:"fields"`
}

// AttachmentView describes a current attachment without changing stored metadata.
type AttachmentView struct {
	State      string `json:"state"`
	Provenance string `json:"provenance"`
	Error      string `json:"error,omitempty"`
}

// MetadataName carries a display name and its source.
type MetadataName struct {
	Value      string `json:"value,omitempty"`
	Provenance string `json:"provenance,omitempty"`
}

// NativeIDView describes a provider identity retained by a hosted thread.
type NativeIDView struct {
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId"`
	Provisional    bool   `json:"provisional"`
	RetiredAt      string `json:"retiredAt,omitempty"`
	FirstSeenAt    string `json:"firstSeenAt"`
	LastSeenAt     string `json:"lastSeenAt"`
}

// NativeResolution binds a provisional handle to the native identity its child announced.
type NativeResolution struct {
	Handle string
	Native string
}

// PostureEntry is one canonical posture decision; an empty value clears the field.
type PostureEntry struct {
	Field    string
	Value    string
	Source   string
	Evidence string
	At       string
}

// Posture is the canonical display posture shared by session and catalog views.
type Posture struct {
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	ServiceTier    string `json:"serviceTier,omitempty"`
}

// TerminalTurn is terminal provider evidence to match against open operations.
type TerminalTurn struct {
	ProviderTurnID     string
	Status             string
	Error              *threads.ExecutionError
	Response           string
	ItemID             string
	FinishOnSavedProof bool
}

// ResumedTurnMatch holds the operations matched before resume reconciliation.
type ResumedTurnMatch struct {
	OpenOperations  []threads.TurnRecord
	BoundOperations []string
	// live: a matching operation is not terminal. Only then can the turn's
	// reply text change an operation.
	Live bool
}

// ResultMeta describes the original size and truncation of a saved answer.
type ResultMeta struct {
	Bytes     int
	Truncated bool
}
