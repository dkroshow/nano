package model

import (
	"encoding/json"
	"time"
)

type Provider string

const (
	ProviderCodex  Provider = "codex"
	ProviderClaude Provider = "claude"
	ProviderGrok   Provider = "grok"
)

type ScopeKind string

const (
	ScopeConnection ScopeKind = "connection"
	ScopeThread     ScopeKind = "thread"
	ScopeTurn       ScopeKind = "turn"
	ScopeItem       ScopeKind = "item"
)

// Scope preserves native identity separately from source-local parser keys.
// Nil native IDs mean unknown. Connection ID and source are caller labels.
type Scope struct {
	Kind           ScopeKind `json:"kind"`
	Provider       Provider  `json:"provider"`
	Source         string    `json:"source"`
	Incarnation    string    `json:"incarnation"`
	ConnectionID   string    `json:"connectionId,omitempty"`
	NativeThreadID *string   `json:"nativeThreadId"`
	NativeTurnID   *string   `json:"nativeTurnId"`
	TurnKey        string    `json:"turnKey,omitempty"`
	ItemID         string    `json:"itemId,omitempty"`
	ParentNativeID *string   `json:"parentNativeId,omitempty"`
	// NativeParentCallID scopes nested provider traffic when no child session ID
	// is supplied. It is a call identity, never a fabricated native turn ID.
	NativeParentCallID *string `json:"nativeParentCallId,omitempty"`
}

type SourceRef struct {
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Incarnation string `json:"incarnation"`
	Direction   string `json:"direction,omitempty"`
	Line        int64  `json:"line,omitempty"`
	Frame       int64  `json:"frame,omitempty"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	BlockPath   string `json:"blockPath,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

type Basis string

const (
	BasisNative  Basis = "native"
	BasisDerived Basis = "derived"
	// BasisDerivedExact identifies an exact composite of retained native fields.
	BasisDerivedExact Basis = "derived-exact"
)

// Alias inherits the item's scope. Raw scalar JSON preserves RPC 1 versus "1".
type Alias struct {
	Namespace       string          `json:"namespace"`
	Value           json.RawMessage `json:"value"`
	Authority       Basis           `json:"authority"`
	ObservationRefs []string        `json:"observationRefs"`
}

type ObservationKind string

const (
	ObservationDelta    ObservationKind = "delta"
	ObservationSnapshot ObservationKind = "snapshot"
	ObservationProgress ObservationKind = "progress"
	ObservationStop     ObservationKind = "stop"
	ObservationResult   ObservationKind = "result"
	ObservationMetadata ObservationKind = "metadata"
	ObservationUnknown  ObservationKind = "unknown"
)

// Observation is source evidence beneath an item or turn, or an independent
// connection/thread fact. NativeEventID identifies evidence, not the whole item.
type Observation struct {
	ID            string          `json:"id"`
	Kind          ObservationKind `json:"kind"`
	Scope         Scope           `json:"scope"`
	Source        SourceRef       `json:"source"`
	NativeEventID string          `json:"nativeEventId,omitempty"`
	// NativeEventDigest fingerprints the provider-defined immutable event
	// content. Consumers can check identity reuse after parser state eviction
	// without retaining or decoding the native envelope. Empty means unknown.
	NativeEventDigest string            `json:"nativeEventDigest,omitempty"`
	Text              string            `json:"text,omitempty"`
	Complete          *bool             `json:"complete,omitempty"`
	Outcome           Outcome           `json:"outcome,omitempty"`
	Capture           CaptureState      `json:"capture,omitempty"`
	Times             []TimeObservation `json:"times,omitempty"`
	Metadata          Metadata          `json:"metadata,omitempty"`
	Detail            json.RawMessage   `json:"detail,omitempty"`
}

type CaptureState string

const (
	CaptureComplete  CaptureState = "complete"
	CapturePartial   CaptureState = "partial"
	CaptureTruncated CaptureState = "truncated"
	CaptureUnknown   CaptureState = "unknown"
)

type Outcome string

const (
	OutcomeRunning     Outcome = "running"
	OutcomeCompleted   Outcome = "completed"
	OutcomeFailed      Outcome = "failed"
	OutcomeInterrupted Outcome = "interrupted"
	OutcomeTruncated   Outcome = "truncated"
	OutcomeDeclined    Outcome = "declined"
	OutcomeCancelled   Outcome = "cancelled"
	OutcomeUnknown     Outcome = "unknown"
)

type FinalityState string

const (
	FinalityUnknown   FinalityState = "unknown"
	FinalityCandidate FinalityState = "candidate"
	FinalityFinal     FinalityState = "final"
	FinalityRetracted FinalityState = "retracted"
)

type Finality struct {
	State           FinalityState `json:"state"`
	Basis           Basis         `json:"basis,omitempty"`
	ObservationRefs []string      `json:"observationRefs,omitempty"`
}

type CompletionFact struct {
	Scope           Scope    `json:"scope"`
	Outcome         Outcome  `json:"outcome"`
	Basis           Basis    `json:"basis"`
	ObservationRefs []string `json:"observationRefs"`
	Reason          string   `json:"reason,omitempty"`
}

type Availability string

const (
	AvailableInline    Availability = "inline"
	AvailableReference Availability = "reference"
	AvailableOmitted   Availability = "omitted"
	AvailableRedacted  Availability = "redacted"
	AvailableTruncated Availability = "truncated"
)

// Part inherits item scope; unpromoted native content stays bounded in Detail.
type Part struct {
	Type           string          `json:"type"`
	Index          *int            `json:"index,omitempty"`
	Text           string          `json:"text,omitempty"`
	Availability   Availability    `json:"availability"`
	Source         SourceRef       `json:"source"`
	OriginalBytes  *int64          `json:"originalBytes,omitempty"`
	OmissionReason string          `json:"omissionReason,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
}

type TimeClass string
type TimeMeaning string

const (
	TimeNative   TimeClass   = "native"
	TimeCapture  TimeClass   = "capture"
	TimeObserved TimeClass   = "observed"
	TimeEvent    TimeMeaning = "event"
	TimeWrite    TimeMeaning = "write"
	TimeStart    TimeMeaning = "start"
	TimeEnd      TimeMeaning = "end"
	TimeTerminal TimeMeaning = "terminal"
	// Activity clocks distinguish source bookkeeping from conversation. A
	// thread activity clock is the native session document's reported time;
	// it does not claim that a particular user or assistant message exists.
	TimeActivity       TimeMeaning = "activity"
	TimeConversation   TimeMeaning = "conversation"
	TimeThreadActivity TimeMeaning = "thread_activity"
)

type TimeObservation struct {
	Scope          Scope           `json:"scope"`
	Value          *time.Time      `json:"value"`
	Raw            json.RawMessage `json:"raw,omitempty"`
	Class          TimeClass       `json:"class"`
	SourceField    string          `json:"sourceField"`
	Precision      string          `json:"precision"`
	Meaning        TimeMeaning     `json:"meaning"`
	ObservationRef string          `json:"observationRef"`
}

type Duration struct {
	Scope          Scope  `json:"scope"`
	Value          int64  `json:"value"`
	Unit           string `json:"unit"`
	Basis          string `json:"basis"`
	SourceField    string `json:"sourceField"`
	ObservationRef string `json:"observationRef"`
}

type RelationKind string

const (
	RelationResultOf   RelationKind = "result_of"
	RelationReplayOf   RelationKind = "replay_of"
	RelationProgressOf RelationKind = "progress_of"
	// Paired Codex file rows serialize one item through response and event shapes.
	RelationSerializationTwin RelationKind = "serialization_twin"
)

// Item endpoints and observation endpoints have separate fields so replay
// evidence cannot be mistaken for another invocation or result.
type Relation struct {
	Kind            RelationKind `json:"kind"`
	Scope           Scope        `json:"scope"`
	FromItem        string       `json:"fromItem,omitempty"`
	ToItem          string       `json:"toItem,omitempty"`
	FromObservation string       `json:"fromObservation,omitempty"`
	ToObservation   string       `json:"toObservation,omitempty"`
	// ToNativeObservation identifies an evicted target through persisted native
	// evidence. Consumers resolve it without reopening continuation state.
	ToNativeObservation *NativeObservationRef `json:"toNativeObservation,omitempty"`
	ObservationRefs     []string              `json:"observationRefs"`
}

// NativeObservationRef contains native identity only. Scope's parser-local
// ItemID and TurnKey are empty. Unknown connection/thread/turn fields stay
// unknown; they never authorize choosing one of several matching targets.
// Namespace and exact scalar JSON distinguish unrelated native identities,
// including RPC number 1 and string "1".
type NativeObservationRef struct {
	Scope     Scope           `json:"scope"`
	Namespace string          `json:"namespace"`
	Value     json.RawMessage `json:"value"`
}

// Diagnostic is also the reader error shape. Message must never contain native
// content; Source locates the retained evidence for inspection.
type Diagnostic struct {
	Code     string    `json:"code"`
	Severity string    `json:"severity"`
	Scope    Scope     `json:"scope"`
	Source   SourceRef `json:"source"`
	Message  string    `json:"message"`
}

func (d Diagnostic) Error() string { return d.Code + ": " + d.Message }
