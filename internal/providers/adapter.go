package providers

import (
	"context"
	"encoding/json"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Adapter adds exactly five turn verbs to lifecycle's evidence contract.
// A returned observation is evidence, never authority to write another layer.
// Implementations preserve provider errors and uncertainty; they do not retry
// an uncertain submission or turn a transport acknowledgement into completion.
type Adapter interface {
	lifecycle.Adapter
	TurnCapabilities() Capabilities
	Start(context.Context, StartRequest) (Attachment, error)
	Resume(context.Context, ResumeRequest) (Attachment, error)
	Submit(context.Context, SubmissionRequest) (SubmissionResult, error)
	Interrupt(context.Context, InterruptRequest) (InterruptionResult, error)
	RespondInteraction(context.Context, InteractionResponse) (model.Batch, error)
}

// Settings preserves absent versus explicitly cleared overrides.
type Settings struct{ Model, Effort, ServiceTier *string }

// Posture carries already-authorized launch choices. Capability support grants
// no permission to widen these values or replace provider-owned state.
type Posture struct{ PermissionMode, Sandbox, Approval string }

type StartRequest struct {
	HostedThreadID string
	Directory      string
	Settings       Settings
	Posture        Posture
}

type ResumeRequest struct {
	StartRequest
	NativeThreadID string
}

type Attachment struct {
	Scope     model.Scope
	Lifecycle lifecycle.Observation
	Evidence  model.Batch
}

type SubmissionRequest struct {
	Scope            model.Scope
	Outbox           turns.OutboxEntry
	Text             string
	Settings         Settings
	PreviouslyQueued bool
	// RunningTurnID, when set, delivers into that running turn instead of
	// starting one. The provider must refuse rather than join another turn.
	RunningTurnID string
}

// FailureTurnEnded marks a refused running-turn delivery whose turn had
// already ended. Nothing was delivered, so the same prompt waits for the next
// dispatch instead of failing.
const FailureTurnEnded = "turn_ended"

type SubmissionResult struct {
	State                    turns.DeliveryState
	Binding                  *turns.Binding
	SupersedesNativeThreadID string
	Lifecycle                *lifecycle.Observation
	Evidence                 model.Batch
	Receipt                  json.RawMessage
	Reason                   string
	FailureCode              string
}

type InterruptRequest struct {
	Scope  model.Scope
	Active lifecycle.Observation
}

type InterruptionResult struct {
	Interrupted bool
	Reason      string
	Evidence    model.Batch
}

type InteractionResponse struct {
	Request   model.Item
	RequestID string
	Method    string
	Params    json.RawMessage
	Kind      InteractionKind
	Decision  string
	Answers   json.RawMessage
}
