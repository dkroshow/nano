package app

import (
	"context"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These helpers write schema-20 rows only for migration and compatibility
// tests. Production queuedTurnStore and sendAttemptStore are read proxies;
// internal/core/prompts.Store is the only shipped writer.
func seedAcceptedTransactionalTurn(t *testing.T, store *prompts.Store, hosted string, provider model.Provider, native, action, providerTurnID, prompt string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Submit(ctx, prompts.Submission{
		HostedThreadID: hosted, Provider: provider, NativeThreadID: native, ActionID: action,
		ProviderText: prompt, PresentationText: prompt, TextHash: sendTextHash(prompt), RequestHash: sendTextHash(prompt), Origin: prompts.HumanOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	token := "fixture-claim-" + action
	record, claimed, err := store.Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: token, AttachmentEpoch: 1, Ready: true})
	if err != nil || !claimed || record.Outbox.ActionID != action {
		t.Fatalf("claim fixture %s: record=%+v claimed=%v err=%v", action, record, claimed, err)
	}
	binding := prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: provider, NativeThreadID: &native, NativeTurnID: &providerTurnID}}
	if provider == model.ProviderClaude {
		// Claude's acceptance records the submission id stamped on its line.
		binding.EventID = submissionEventID(hosted, action)
	}
	if _, err := store.Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: token, AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
}
