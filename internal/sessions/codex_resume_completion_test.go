package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/testsupport"
)

type resumeCompletionClient struct {
	store    *coreturns.Store
	messages []interact.SendMessageRequest
}

func (c *resumeCompletionClient) SubscribeCompletedTurns(ctx context.Context, request interact.PageRequest) (interact.CompletedTurnPage, error) {
	rows, err := c.store.After(ctx, request.ThreadRef, request.Cursor, request.Limit)
	page := interact.CompletedTurnPage{}
	for _, row := range rows {
		page.Items = append(page.Items, interact.CompletedTurn{TurnID: row.TurnID, Outcome: row.Outcome})
		page.NextCursor = row.TurnID
	}
	return page, err
}

func (c *resumeCompletionClient) SendMessage(_ context.Context, request interact.SendMessageRequest) (json.RawMessage, error) {
	c.messages = append(c.messages, request)
	return json.RawMessage(`{"messageId":"message"}`), nil
}

func (c *resumeCompletionClient) SubmitPrompt(context.Context, interact.SubmitPromptRequest) (interact.Turn, error) {
	return interact.Turn{}, fmt.Errorf("unexpected prompt delivery for an inbox subscription")
}

// The app sibling exercises live completion followed by a real reconnect.
// Force the narrower race here: resume re-records a turn already committed by
// the live path, without refusing it or delivering its subscription twice.
func TestCodexForcedResumeRerecordKeepsCompletionIdempotent(t *testing.T) {
	ctx := context.Background()
	sess, _ := readyTransactionalSendTestSession(t, nil)
	t.Cleanup(sess.teardown.Close)
	db := sess.delivery.queuedTurns.Database()
	if _, err := db.Exec(`INSERT INTO hosted_threads (hosted_thread_id,provider,presentation_mode,terminal_notification_policy,revision,created_at,updated_at) VALUES (?,'codex','background','inbox',1,'now','now')`, sess.identity.hostedThreadID); err != nil {
		t.Fatal(err)
	}
	completed, err := coreturns.OpenStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	client := &resumeCompletionClient{store: completed}
	dispatcher := subscriptions.NewDispatcher(db, client, nil, nil)
	target := subscriptions.Target{ChildHostedID: sess.identity.hostedThreadID, InvokerHostedID: "parent"}
	if !dispatcher.SweepTarget(target) {
		t.Fatal("initialize subscription cursor")
	}
	sess.state.subscriptionSweep = func(string) {
		if !dispatcher.SweepTarget(target) {
			t.Error("deliver completed-turn subscription")
		}
	}
	if err := sess.runtime.ensureCoreSession(); err != nil {
		t.Fatal(err)
	}
	finish := coreturns.Finish{HostedThreadID: sess.identity.hostedThreadID, Provider: "codex", NativeThreadID: sess.state.threadID,
		TurnID: "turn-live", Outcome: "completed", FinalMessageID: "final-live", At: time.Unix(1787600100, 0), Source: "adapter"}
	if err := sess.runtime.core.Observe(ctx, coresession.AdapterEvent{ProviderTurnID: finish.TurnID, TurnEvents: []coresession.TurnEvent{{Finished: &finish}}}); err != nil {
		t.Fatal(err)
	}
	live, err := completed.After(ctx, sess.identity.hostedThreadID, "", 10)
	if err != nil || len(live) != 1 || len(client.messages) != 1 {
		t.Fatalf("live completion: feed=%+v err=%v deliveries=%d", live, err, len(client.messages))
	}

	var logs testsupport.LogBuffer
	priorLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(priorLog) })
	completedAt := int64(1787600100)
	sess.delivery.observeCodexResumedTurnFinish(sess.state.threadID, codexResumeProjectionTurn{ID: "turn-live", Status: "completed", CompletedAt: &completedAt, Items: []any{
		map[string]any{"id": "final-live", "type": "agentMessage", "phase": "final_answer", "text": "done live"},
	}})
	feed, err := completed.After(ctx, sess.identity.hostedThreadID, "", 10)
	if err != nil || len(feed) != 1 || feed[0] != live[0] {
		t.Fatalf("feed after forced resume = %+v (%v), want the one live record %+v", feed, err, live)
	}
	if strings.Contains(logs.String(), "ended while Nano was down") {
		t.Fatalf("recording the same turn again was refused:\n%s", logs.String())
	}
	if len(client.messages) != 1 {
		t.Fatalf("parent inbox deliveries = %d, want 1", len(client.messages))
	}
}
