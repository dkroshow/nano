package providers_test

import (
	"context"
	"strings"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/providers/claude"
	"github.com/dkroshow/nano/internal/providers/codex"
	"github.com/dkroshow/nano/internal/providers/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type adapterFactory func(providers.Operations) (providers.Adapter, error)

func TestConcreteAdaptersImplementFiveVerbContract(t *testing.T) {
	for _, test := range []struct {
		provider model.Provider
		factory  adapterFactory
	}{
		{model.ProviderCodex, func(o providers.Operations) (providers.Adapter, error) { return codex.New(o) }},
		{model.ProviderClaude, func(o providers.Operations) (providers.Adapter, error) { return claude.New(o) }},
		{model.ProviderGrok, func(o providers.Operations) (providers.Adapter, error) { return grok.New(o) }},
	} {
		t.Run(string(test.provider), func(t *testing.T) {
			adapter, err := test.factory(passingOperations(test.provider))
			if err != nil {
				t.Fatal(err)
			}
			attachment, err := adapter.Start(context.Background(), providers.StartRequest{HostedThreadID: "hosted"})
			if err != nil || attachment.Scope.Provider != test.provider {
				t.Fatalf("start = %+v, %v", attachment, err)
			}
			if current, ok := adapter.Current(); !ok || current != attachment.Lifecycle {
				t.Fatalf("current = %+v, %t", current, ok)
			}
		})
	}
}

func TestConcreteAdaptersPreserveLaunchPermissionEvidence(t *testing.T) {
	for _, test := range []struct {
		provider model.Provider
		factory  adapterFactory
		want     string
	}{
		{model.ProviderCodex, func(o providers.Operations) (providers.Adapter, error) { return codex.New(o) }, "never"},
		{model.ProviderClaude, func(o providers.Operations) (providers.Adapter, error) { return claude.New(o) }, "bypassPermissions"},
		{model.ProviderGrok, func(o providers.Operations) (providers.Adapter, error) { return grok.New(o) }, "bypassPermissions"},
	} {
		t.Run(string(test.provider), func(t *testing.T) {
			operations := passingOperations(test.provider)
			var got providers.StartRequest
			operations.Start = func(_ context.Context, request providers.StartRequest) (providers.Attachment, error) {
				got = request
				return readyAttachment(test.provider, 1), nil
			}
			adapter, err := test.factory(operations)
			if err != nil {
				t.Fatal(err)
			}
			request := providers.StartRequest{HostedThreadID: "hosted", Posture: providers.Posture{Approval: "never", PermissionMode: "bypassPermissions"}}
			if _, err := adapter.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if got.Posture != request.Posture || adapter.TurnCapabilities().PermissionValue(got.Posture) != test.want {
				t.Fatalf("posture changed: got %+v, want %+v", got.Posture, request.Posture)
			}
		})
	}
}

func TestSubmissionSettingsFollowCapabilitiesWithoutProviderBranch(t *testing.T) {
	modelName, effort := "next", "high"
	for _, test := range []struct {
		name     string
		provider model.Provider
		factory  adapterFactory
		settings providers.Settings
		allowed  bool
	}{
		{"codex next-turn model and effort", model.ProviderCodex, func(o providers.Operations) (providers.Adapter, error) { return codex.New(o) }, providers.Settings{Model: &modelName, Effort: &effort}, true},
		{"claude live model", model.ProviderClaude, func(o providers.Operations) (providers.Adapter, error) { return claude.New(o) }, providers.Settings{Model: &modelName}, true},
		{"claude creation-only effort", model.ProviderClaude, func(o providers.Operations) (providers.Adapter, error) { return claude.New(o) }, providers.Settings{Effort: &effort}, false},
		{"grok creation-only model", model.ProviderGrok, func(o providers.Operations) (providers.Adapter, error) { return grok.New(o) }, providers.Settings{Model: &modelName}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			operations := passingOperations(test.provider)
			operations.Submit = func(_ context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
				called = true
				return acceptedResult(request.Scope), nil
			}
			adapter, err := test.factory(operations)
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Submit(context.Background(), submission(test.provider, test.settings))
			if (err == nil) != test.allowed || called != test.allowed {
				t.Fatalf("allowed=%t called=%t err=%v", test.allowed, called, err)
			}
		})
	}
}

func TestAcceptedWithoutExactBindingBecomesExplicitUncertainty(t *testing.T) {
	operations := passingOperations(model.ProviderCodex)
	operations.Submit = func(context.Context, providers.SubmissionRequest) (providers.SubmissionResult, error) {
		return providers.SubmissionResult{State: turns.Accepted}, nil
	}
	adapter, err := codex.New(operations)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Submit(context.Background(), submission(model.ProviderCodex, providers.Settings{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != turns.Uncertain || len(result.Evidence.Diagnostics) != 1 || result.Evidence.Diagnostics[0].Code != "accepted_without_binding" {
		t.Fatalf("missing explicit evidence gap: %+v", result)
	}
}

func TestSubmissionAcceptsExplicitProviderRevealedNativeThreadSuccession(t *testing.T) {
	operations := passingOperations(model.ProviderClaude)
	operations.Submit = func(_ context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
		result := acceptedResult(scopeWithNative(model.ProviderClaude, "native-after-init"))
		result.SupersedesNativeThreadID = *request.Scope.NativeThreadID
		return result, nil
	}
	adapter, err := claude.New(operations)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Submit(context.Background(), providers.SubmissionRequest{
		Scope: scopeWithNative(model.ProviderClaude, "provisional-shim"), Outbox: turns.OutboxEntry{ActionID: "action"}, Text: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Binding == nil || result.Binding.Scope.NativeThreadID == nil || *result.Binding.Scope.NativeThreadID != "native-after-init" {
		t.Fatalf("provider-revealed binding = %+v", result.Binding)
	}
}

func TestSubmissionRejectsUnprovedNativeThreadReplacement(t *testing.T) {
	operations := passingOperations(model.ProviderClaude)
	operations.Submit = func(context.Context, providers.SubmissionRequest) (providers.SubmissionResult, error) {
		return acceptedResult(scopeWithNative(model.ProviderClaude, "unproved-replacement")), nil
	}
	adapter, err := claude.New(operations)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Submit(context.Background(), submission(model.ProviderClaude, providers.Settings{}))
	if err == nil || !strings.Contains(err.Error(), "binding_scope_conflict") {
		t.Fatalf("unproved native replacement error = %v", err)
	}
}

func TestAdapterRejectsCrossProviderBindingAndUnsupportedInteractionBeforeIO(t *testing.T) {
	operations := passingOperations(model.ProviderCodex)
	operations.Submit = func(_ context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
		result := acceptedResult(request.Scope)
		result.Binding.Scope.Provider = model.ProviderGrok
		return result, nil
	}
	interactionCalled := false
	operations.RespondInteraction = func(context.Context, providers.InteractionResponse) (model.Batch, error) {
		interactionCalled = true
		return model.Batch{}, nil
	}
	adapter, err := codex.New(operations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Submit(context.Background(), submission(model.ProviderCodex, providers.Settings{})); err == nil {
		t.Fatal("cross-provider binding accepted")
	}
	_, err = adapter.RespondInteraction(context.Background(), providers.InteractionResponse{
		Request: model.Item{Scope: scope(model.ProviderCodex)}, Kind: providers.ToolPermission,
	})
	if err == nil || interactionCalled {
		t.Fatalf("unsupported interaction reached provider: called=%t err=%v", interactionCalled, err)
	}
}

func TestResumeRequiresAndPreservesExactNativeIdentity(t *testing.T) {
	operations := passingOperations(model.ProviderGrok)
	operations.Resume = func(_ context.Context, request providers.ResumeRequest) (providers.Attachment, error) {
		attachment := readyAttachment(model.ProviderGrok, 2)
		other := "other"
		attachment.Scope.NativeThreadID = &other
		return attachment, nil
	}
	adapter, err := grok.New(operations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Resume(context.Background(), providers.ResumeRequest{}); err == nil {
		t.Fatal("identity-less resume accepted")
	}
	if _, err := adapter.Resume(context.Background(), providers.ResumeRequest{StartRequest: providers.StartRequest{HostedThreadID: "hosted"}, NativeThreadID: "wanted"}); err == nil {
		t.Fatal("contradictory resume identity accepted")
	}
}

func TestConcreteAdaptersRejectIntermittentProviderTranscriptFinalItemRace(t *testing.T) {
	for _, test := range []struct {
		provider model.Provider
		factory  adapterFactory
	}{
		{model.ProviderCodex, func(o providers.Operations) (providers.Adapter, error) { return codex.New(o) }},
		{model.ProviderClaude, func(o providers.Operations) (providers.Adapter, error) { return claude.New(o) }},
		{model.ProviderGrok, func(o providers.Operations) (providers.Adapter, error) { return grok.New(o) }},
	} {
		t.Run(string(test.provider), func(t *testing.T) {
			for _, finalItemID := range []string{"", "final-item"} {
				operations := passingOperations(test.provider)
				operations.Resume = func(_ context.Context, request providers.ResumeRequest) (providers.Attachment, error) {
					attachment := readyAttachment(test.provider, 2)
					attachment.Scope.NativeThreadID = &request.NativeThreadID
					attachment.Evidence.Changes = []model.Change{{Turn: &model.Turn{
						Key: "turn", NativeTurnID: "turn", Status: model.TurnCompleted,
						Outcome: model.OutcomeCompleted, FinalItemID: finalItemID,
					}}}
					return attachment, nil
				}
				adapter, err := test.factory(operations)
				if err != nil {
					t.Fatal(err)
				}
				_, err = adapter.Resume(context.Background(), providers.ResumeRequest{
					StartRequest: providers.StartRequest{HostedThreadID: "hosted"}, NativeThreadID: "native",
				})
				if err == nil || !strings.Contains(err.Error(), "completed final item is unavailable") {
					t.Fatalf("completion without final assistant item %q was accepted: %v", finalItemID, err)
				}
			}
		})
	}
}

func TestConcreteAdaptersAcceptCompletedTurnWithFinalAssistantItem(t *testing.T) {
	for _, test := range []struct {
		provider model.Provider
		factory  adapterFactory
	}{
		{model.ProviderCodex, func(o providers.Operations) (providers.Adapter, error) { return codex.New(o) }},
		{model.ProviderClaude, func(o providers.Operations) (providers.Adapter, error) { return claude.New(o) }},
		{model.ProviderGrok, func(o providers.Operations) (providers.Adapter, error) { return grok.New(o) }},
	} {
		t.Run(string(test.provider), func(t *testing.T) {
			operations := passingOperations(test.provider)
			operations.Resume = func(_ context.Context, request providers.ResumeRequest) (providers.Attachment, error) {
				attachment := readyAttachment(test.provider, 2)
				attachment.Scope.NativeThreadID = &request.NativeThreadID
				attachment.Evidence.Changes = []model.Change{
					{Item: &model.Item{ID: "final-item", Type: model.ItemAgentMessage, Role: model.RoleAssistant}},
					{Turn: &model.Turn{Key: "turn", NativeTurnID: "turn", Status: model.TurnCompleted, Outcome: model.OutcomeCompleted, FinalItemID: "final-item"}},
				}
				return attachment, nil
			}
			adapter, err := test.factory(operations)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Resume(context.Background(), providers.ResumeRequest{
				StartRequest: providers.StartRequest{HostedThreadID: "hosted"}, NativeThreadID: "native",
			}); err != nil {
				t.Fatalf("complete provider evidence rejected: %v", err)
			}
		})
	}
}

func passingOperations(provider model.Provider) providers.Operations {
	return providers.Operations{
		Start: func(context.Context, providers.StartRequest) (providers.Attachment, error) {
			return readyAttachment(provider, 1), nil
		},
		Resume: func(_ context.Context, request providers.ResumeRequest) (providers.Attachment, error) {
			attachment := readyAttachment(provider, 2)
			attachment.Scope.NativeThreadID = &request.NativeThreadID
			return attachment, nil
		},
		Submit: func(_ context.Context, request providers.SubmissionRequest) (providers.SubmissionResult, error) {
			return acceptedResult(request.Scope), nil
		},
		Interrupt: func(context.Context, providers.InterruptRequest) (providers.InterruptionResult, error) {
			return providers.InterruptionResult{Interrupted: true}, nil
		},
		RespondInteraction: func(context.Context, providers.InteractionResponse) (model.Batch, error) { return model.Batch{}, nil },
	}
}

func readyAttachment(provider model.Provider, epoch uint64) providers.Attachment {
	native := "native"
	return providers.Attachment{
		Scope:     scopeWithNative(provider, native),
		Lifecycle: lifecycle.Observation{Event: lifecycle.EventReadyIdle, Epoch: epoch, Sequence: 1},
	}
}

func submission(provider model.Provider, settings providers.Settings) providers.SubmissionRequest {
	return providers.SubmissionRequest{Scope: scopeWithNative(provider, "native"), Outbox: turns.OutboxEntry{ActionID: "action"}, Text: "hello", Settings: settings}
}

func acceptedResult(requestScope model.Scope) providers.SubmissionResult {
	return providers.SubmissionResult{State: turns.Accepted, Binding: &turns.Binding{Scope: requestScope, NativeItemID: "item"}}
}

func scope(provider model.Provider) model.Scope {
	return model.Scope{Provider: provider, Kind: model.ScopeThread, Source: "wire", Incarnation: "connection"}
}

func scopeWithNative(provider model.Provider, native string) model.Scope {
	s := scope(provider)
	s.NativeThreadID = &native
	return s
}
