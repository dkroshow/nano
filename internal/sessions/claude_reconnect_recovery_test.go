package sessions

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func seedRecoveryPrompt(t *testing.T, store *prompts.Store, hosted, native, action, turn, text string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: model.ProviderClaude, NativeThreadID: native, ActionID: action, ProviderText: text, PresentationText: text, TextHash: sendTextHash(text), RequestHash: sendTextHash(text)}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Claim(ctx, prompts.ClaimRequest{HostedThreadID: hosted, Token: action, AttachmentEpoch: 1, Ready: true}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := store.Accepted(ctx, prompts.Acceptance{ActionID: action, ClaimToken: action, AttachmentEpoch: 1, Binding: prompts.Binding{EventID: SubmissionEventID(hosted, action), Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderClaude, NativeThreadID: &native, NativeTurnID: &turn}}}); err != nil {
		t.Fatal(err)
	}
}
func TestClaudeUnfinishedPromptsWaitForABusyWriterOncePerThread(t *testing.T) {
	sess, store := readyTransactionalSendTestSession(t, nil)
	hosted := sess.HostedThreadID()
	native := sess.ThreadID()
	ctx := context.Background()
	completed, err := coreturns.OpenStore(ctx, sess.delivery.queuedTurns.Database())
	if err != nil {
		t.Fatal(err)
	}
	sess.completedTurns = func(context.Context) (*coreturns.Store, error) { return completed, nil }
	prefix := "claude:" + hosted + ":"
	// The last two prompts are still owed: one turn started without an
	// outcome, and one has no turn row yet. Every other turn ended.
	const prompts = 300
	for i := 1; i <= prompts; i++ {
		turn := fmt.Sprintf("%s%d", prefix, i)
		seedRecoveryPrompt(t, store, hosted, native, fmt.Sprintf("action-%d", i), turn, fmt.Sprintf("prompt %d", i))
		var err error
		switch i {
		case prompts - 1:
			_, _, err = completed.Started(ctx, coreturns.Start{HostedThreadID: hosted, Provider: "claude", NativeThreadID: native, TurnID: turn, Source: "adapter"})
		case prompts:
		default:
			_, _, err = completed.Finished(ctx, coreturns.Finish{HostedThreadID: hosted, Provider: "claude", NativeThreadID: native, TurnID: turn, Outcome: agentTurnCompleted, Source: "adapter"})
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	// Restore writes, back to back: each holds the writer for 5 ms.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tx, err := sess.delivery.queuedTurns.Database().BeginTx(ctx, nil)
			if err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
			_ = tx.Rollback()
		}
	}()
	started := time.Now()
	wanted, _, err := sess.unfinishedClaudePrompts(ctx, prefix, native)
	took := time.Since(started)
	close(stop)
	<-done
	if err != nil || !slices.Equal(wanted, []int64{prompts - 1, prompts}) {
		t.Fatalf("unfinished = %v, %v; want exactly the two owed prompts %d and %d", wanted, err, prompts-1, prompts)
	}
	// A read per prompt waits at least 300 x 5 ms = 1.5 s here.
	if took > 500*time.Millisecond {
		t.Fatalf("checking %d prompts took %s behind a busy writer; want one outcome read for the thread", prompts, took)
	}
	t.Logf("checked %d prompts behind a busy writer in %s", prompts, took)
}
