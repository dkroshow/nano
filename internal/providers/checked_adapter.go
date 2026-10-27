package providers

import (
	"context"
	"errors"
	"sync"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Operations is the provider-protocol edge consumed by CheckedAdapter. Each
// function accepts contract values and returns normalized evidence. A runtime
// binds these functions to a provider client; no hosted session belongs here.
type Operations struct {
	Start              func(context.Context, StartRequest) (Attachment, error)
	Resume             func(context.Context, ResumeRequest) (Attachment, error)
	Submit             func(context.Context, SubmissionRequest) (SubmissionResult, error)
	Interrupt          func(context.Context, InterruptRequest) (InterruptionResult, error)
	RespondInteraction func(context.Context, InteractionResponse) (model.Batch, error)
}

// CheckedAdapter implements the shared evidence boundary. Provider packages
// construct it with their immutable declaration and expose a concrete Adapter
// type. Protocol translation remains in their Operations implementation.
type CheckedAdapter struct {
	mu          sync.RWMutex
	provider    model.Provider
	declaration Declaration
	operations  Operations
	current     lifecycle.Observation
	settled     bool
}

func NewCheckedAdapter(provider model.Provider, declaration Declaration, operations Operations) (*CheckedAdapter, error) {
	if provider == "" || operations.Start == nil || operations.Resume == nil || operations.Submit == nil || operations.Interrupt == nil || operations.RespondInteraction == nil {
		return nil, errors.New("providers: adapter requires one provider and all five operations")
	}
	return &CheckedAdapter{provider: provider, declaration: declaration, operations: operations}, nil
}

func (a *CheckedAdapter) Capabilities() lifecycle.Capabilities { return a.declaration.Lifecycle }
func (a *CheckedAdapter) TurnCapabilities() Capabilities       { return a.declaration.Turns }

func (a *CheckedAdapter) Current() (lifecycle.Observation, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.current, a.settled
}

// PublishCurrent accepts a complete ready snapshot already qualified by the
// provider's protocol edge. It is used for later recovery snapshots; ordinary
// live deltas go directly to app.Observe and cannot masquerade as Current.
func (a *CheckedAdapter) PublishCurrent(observation lifecycle.Observation) error {
	if err := validateCurrent(observation); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settled && (observation.Epoch < a.current.Epoch || observation.Epoch == a.current.Epoch && observation.Sequence <= a.current.Sequence) {
		return model.Diagnostic{Code: "stale_current_evidence", Severity: "error", Message: "provider current evidence did not advance"}
	}
	a.current, a.settled = observation, true
	return nil
}

func (a *CheckedAdapter) Start(ctx context.Context, request StartRequest) (Attachment, error) {
	if err := a.declaration.Turns.CheckSettings(AtCreation, request.Settings); err != nil {
		return Attachment{}, err
	}
	attachment, err := a.operations.Start(ctx, request)
	if err != nil {
		return Attachment{}, err
	}
	if err := a.validateAttachment(attachment, ""); err != nil {
		return Attachment{}, err
	}
	if err := a.PublishCurrent(attachment.Lifecycle); err != nil {
		return Attachment{}, err
	}
	return attachment, nil
}

func (a *CheckedAdapter) Resume(ctx context.Context, request ResumeRequest) (Attachment, error) {
	if request.NativeThreadID == "" {
		return Attachment{}, model.Diagnostic{Code: "missing_native_thread", Severity: "error", Message: "resume requires an exact native thread"}
	}
	if err := a.declaration.Turns.CheckSettings(AtCreation, request.Settings); err != nil {
		return Attachment{}, err
	}
	attachment, err := a.operations.Resume(ctx, request)
	if err != nil {
		return Attachment{}, err
	}
	if err := a.validateAttachment(attachment, request.NativeThreadID); err != nil {
		return Attachment{}, err
	}
	if err := a.PublishCurrent(attachment.Lifecycle); err != nil {
		return Attachment{}, err
	}
	return attachment, nil
}

func (a *CheckedAdapter) Submit(ctx context.Context, request SubmissionRequest) (SubmissionResult, error) {
	if err := a.validateScope(request.Scope); err != nil {
		return SubmissionResult{}, err
	}
	if request.Outbox.ActionID == "" {
		return SubmissionResult{}, model.Diagnostic{Code: "missing_action_id", Severity: "error", Message: "submission requires its durable action id"}
	}
	if err := a.declaration.Turns.CheckSubmissionSettings(request.Settings); err != nil {
		return SubmissionResult{}, err
	}
	result, err := a.operations.Submit(ctx, request)
	if err != nil {
		return SubmissionResult{}, err
	}
	if err := validateCompletedFinalItems(result.Evidence); err != nil {
		return SubmissionResult{}, err
	}
	switch result.State {
	case turns.Accepted:
		if result.Binding == nil {
			result.State = turns.Uncertain
			if result.Reason == "" {
				result.Reason = "provider accepted the turn without returning an exact turn binding"
			}
			result.Evidence.Diagnostics = append(result.Evidence.Diagnostics, model.Diagnostic{
				Code: "accepted_without_binding", Severity: "warning",
				Message: "provider acknowledgement lacked an exact submission binding",
			})
		}
	case turns.Uncertain, turns.Refused, turns.Parked:
	default:
		return SubmissionResult{}, model.Diagnostic{Code: "invalid_submission_state", Severity: "error", Message: "provider returned an invalid submission state"}
	}
	if result.Binding != nil {
		if err := a.validateScope(result.Binding.Scope); err != nil {
			return SubmissionResult{}, err
		}
		if !sameNativeThread(request.Scope, result.Binding.Scope) && !exactNativeThreadSuccession(request.Scope, result) {
			return SubmissionResult{}, model.Diagnostic{Code: "binding_scope_conflict", Severity: "error", Message: "submission binding contradicts the requested native thread"}
		}
	}
	return result, nil
}

func (a *CheckedAdapter) Interrupt(ctx context.Context, request InterruptRequest) (InterruptionResult, error) {
	if err := a.validateScope(request.Scope); err != nil {
		return InterruptionResult{}, err
	}
	result, err := a.operations.Interrupt(ctx, request)
	if err != nil {
		return InterruptionResult{}, err
	}
	if err := validateCompletedFinalItems(result.Evidence); err != nil {
		return InterruptionResult{}, err
	}
	return result, nil
}

func (a *CheckedAdapter) RespondInteraction(ctx context.Context, response InteractionResponse) (model.Batch, error) {
	if err := a.declaration.Turns.CheckInteraction(response.Kind); err != nil {
		return model.Batch{}, err
	}
	if err := a.validateScope(response.Request.Scope); err != nil {
		return model.Batch{}, err
	}
	batch, err := a.operations.RespondInteraction(ctx, response)
	if err != nil {
		return model.Batch{}, err
	}
	if err := validateCompletedFinalItems(batch); err != nil {
		return model.Batch{}, err
	}
	return batch, nil
}

func (a *CheckedAdapter) validateAttachment(attachment Attachment, nativeThreadID string) error {
	if err := a.validateScope(attachment.Scope); err != nil {
		return err
	}
	if err := validateCurrent(attachment.Lifecycle); err != nil {
		return err
	}
	if err := validateCompletedFinalItems(attachment.Evidence); err != nil {
		return err
	}
	if nativeThreadID != "" && (attachment.Scope.NativeThreadID == nil || *attachment.Scope.NativeThreadID != nativeThreadID) {
		return model.Diagnostic{Code: "resume_identity_conflict", Severity: "error", Message: "provider attached a different native thread"}
	}
	return nil
}

// validateCompletedFinalItems keeps a provider completion and its visible
// assistant result in one adapter result. A later incremental completion may
// refer to an item already owned by app, but an operation result cannot claim
// a self-contained completed turn while omitting its named final item.
func validateCompletedFinalItems(batch model.Batch) error {
	items := make(map[string]model.Item)
	for _, change := range batch.Changes {
		if change.Item != nil && change.Item.ID != "" {
			items[change.Item.ID] = *change.Item
		}
	}
	for _, change := range batch.Changes {
		if change.Turn == nil || change.Turn.Status != model.TurnCompleted {
			continue
		}
		item, ok := items[change.Turn.FinalItemID]
		if !ok || item.Type != model.ItemAgentMessage || item.Role != model.RoleAssistant || item.Sidechain || item.Scope.NativeParentCallID != nil {
			return model.Diagnostic{
				Code: "completed_final_item_unavailable", Severity: "error",
				Message: "completed final item is unavailable from provider evidence",
			}
		}
	}
	return nil
}

func (a *CheckedAdapter) validateScope(scope model.Scope) error {
	if scope.Provider != a.provider {
		return model.Diagnostic{Code: "provider_scope_conflict", Severity: "error", Message: "provider evidence crossed adapter scope"}
	}
	return nil
}

func validateCurrent(observation lifecycle.Observation) error {
	if observation.Epoch == 0 || observation.Sequence == 0 {
		return model.Diagnostic{Code: "incomplete_current_evidence", Severity: "error", Message: "provider current evidence requires epoch and sequence"}
	}
	switch observation.Event {
	case lifecycle.EventReadyIdle, lifecycle.EventReadyAccepted, lifecycle.EventReadyWorking, lifecycle.EventReadyWaiting:
		return nil
	default:
		return model.Diagnostic{Code: "incomplete_current_evidence", Severity: "error", Message: "provider current evidence must be a complete ready snapshot"}
	}
}

func sameNativeThread(left, right model.Scope) bool {
	if left.NativeThreadID == nil || right.NativeThreadID == nil {
		return true
	}
	return *left.NativeThreadID == *right.NativeThreadID
}

func exactNativeThreadSuccession(request model.Scope, result SubmissionResult) bool {
	return request.NativeThreadID != nil && result.Binding != nil && result.Binding.Scope.NativeThreadID != nil &&
		result.SupersedesNativeThreadID != "" && result.SupersedesNativeThreadID == *request.NativeThreadID &&
		*result.Binding.Scope.NativeThreadID != "" && *result.Binding.Scope.NativeThreadID != result.SupersedesNativeThreadID
}
