package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

// The app sibling checks saved-transcript settlement. This case retains the
// original mailbox order: reset queued, link lost, then stale wire evidence.
func TestCodexPromptIsResentOnlyOnProofItsTurnDroppedIt(t *testing.T) {
	t.Run("evidence from the previous attachment: reset still queued, then the link drops again", func(t *testing.T) {
		ctx := context.Background()
		sent := make(chan string, 4)
		sess, store := readyTransactionalSendTestSession(t, func(_ context.Context, text string) (json.RawMessage, error) {
			sent <- text
			return json.RawMessage(`{"turn":{"id":"next"}}`), nil
		})
		sess.savedPrompts = func(context.Context, history.SavedUserKeys) ([]history.SavedUserRow, error) { return nil, nil }
		native, turn := sess.ThreadID(), "turn-case"
		if _, _, err := store.Submit(ctx, prompts.Submission{HostedThreadID: sess.HostedThreadID(), Provider: model.ProviderCodex, NativeThreadID: native, ActionID: "prompt", ProviderText: "same prompt", PresentationText: "same prompt", TextHash: "hash", RequestHash: "hash"}); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := store.Claim(ctx, prompts.ClaimRequest{HostedThreadID: sess.HostedThreadID(), Token: "case", AttachmentEpoch: 1, Ready: true}); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if _, err := store.Accepted(ctx, prompts.Acceptance{ActionID: "prompt", ClaimToken: "case", AttachmentEpoch: 1, Binding: prompts.Binding{Scope: model.Scope{Kind: model.ScopeTurn, Provider: model.ProviderCodex, NativeThreadID: &native, NativeTurnID: &turn}}}); err != nil {
			t.Fatal(err)
		}
		sess.awaitSavedPrompt("prompt")
		play := func() {
			sess.recordWireLive(protocol.Recv, []byte(`{"method":"turn/started","params":{"threadId":"`+native+`","turn":{"id":"`+turn+`","status":"inProgress","items":[]}}}`), true)
			sess.recordWireLive(protocol.Recv, []byte(`{"method":"turn/completed","params":{"threadId":"`+native+`","turn":{"id":"`+turn+`","status":"interrupted"}}}`), true)
		}
		play()
		if err := sess.ensureCoreSession(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sess.coreSession().Close)
		busy := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-busy:
			default:
				close(busy)
			}
		})
		sess.coreSession().ObserveAsync(coresession.AdapterEvent{LegacyApply: func() error { <-busy; return nil }})
		sess.forgetCodexTurnInputs()
		sess.SettleWaitingPrompts(ctx)
		sess.observeConnection(lifecycle.EventLinkLost)
		close(busy)
		play()
		if err := sess.coreSession().Observe(ctx, coresession.AdapterEvent{}); err != nil {
			t.Fatal(err)
		}
		sess.SettleWaitingPrompts(ctx)
		select {
		case text := <-sent:
			t.Fatalf("sent %q again; want resent=false", text)
		case <-time.After(time.Second):
		}
		record, _, err := store.Get(ctx, "prompt")
		if err != nil || record.Outbox.State != prompts.Accepted {
			t.Fatalf("prompt=%+v %v; want accepted", record.Outbox, err)
		}
	})
}

// The app sibling covers late real RPC timeouts and saved settlement. This
// receiver case preserves the echo-before-outcome interleaving deterministically.
func TestCodexPromptTakenPastTheDeadlineIsBoundByItsEcho(t *testing.T) {
	var sess *session
	sess, store := readyTransactionalSendTestSession(t, func(context.Context, string) (json.RawMessage, error) {
		event := SubmissionEventID(sess.HostedThreadID(), "late")
		sess.observeCodexTurnInput([]byte(`{"method":"item/started","params":{"threadId":"` + sess.ThreadID() + `","turnId":"turn-late","item":{"id":"late-item","type":"userMessage","clientId":"` + event + `","content":[{"type":"text","text":"late prompt"}]}}}`))
		return nil, context.DeadlineExceeded
	})
	if rec := postTurn(t, sess, context.Background(), "late", "late prompt"); rec.Code != http.StatusAccepted {
		t.Fatalf("send=%d %s", rec.Code, rec.Body.String())
	}
	waitTurnSendDone(t, sess, "late")
	record, _, err := store.Get(context.Background(), "late")
	if err != nil || record.Outbox.State != prompts.Accepted || record.ProviderTurnID != "turn-late" || record.Outbox.Binding.EventID != SubmissionEventID(sess.HostedThreadID(), "late") {
		t.Fatalf("late prompt=%+v %v", record, err)
	}
}
