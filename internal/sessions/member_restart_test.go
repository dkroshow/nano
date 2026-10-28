package sessions

import (
	"context"
	"testing"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// The application sibling checks the saved answer and zero usage. This portion
// keeps the receiver's private restart hint assertion at its owning boundary.
func TestClaudeOwedPersonPromptFinishesAfterARestartOnALaterSavedCommit(t *testing.T) {
	ctx := context.Background()
	sess, store := readyTransactionalSendTestSession(t, nil)
	sess.provider = "claude"
	sess.agentExecutions = &unlinkedPromptExecutions{}
	completed, err := coreturns.OpenStore(ctx, sess.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	sess.completedTurns = func(context.Context) (*coreturns.Store, error) { return completed, nil }
	native, turn := "native-thread", "claude:shim:300"
	if _, _, err := store.Submit(ctx, prompts.Submission{HostedThreadID: sess.hostedThreadID, Provider: model.ProviderClaude, NativeThreadID: native, ActionID: "person-starter", ProviderText: "person prompt", PresentationText: "person prompt", TextHash: "hash", RequestHash: "hash", Origin: prompts.HumanOrigin}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Claim(ctx, prompts.ClaimRequest{HostedThreadID: sess.hostedThreadID, Token: "claim", AttachmentEpoch: 1, Ready: true}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	binding := prompts.Binding{EventID: SubmissionEventID(sess.hostedThreadID, "person-starter"), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}
	if _, err := store.Accepted(ctx, prompts.Acceptance{ActionID: "person-starter", ClaimToken: "claim", AttachmentEpoch: 1, Binding: binding}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Settle(ctx, "person-starter"); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	sess.RequestMemberStep()
	if !sess.owedHint.Load() {
		t.Fatal("attach read owed prompts but left saved commits gated out")
	}
}
