package app

import hostedstore "github.com/dkroshow/nano/internal/hosted"

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/interact"
)

type recordingSubscriptionClient struct {
	mu        sync.Mutex
	messages  []interact.SendMessageRequest
	prompts   []interact.SubmitPromptRequest
	pages     map[string]interact.CompletedTurnPage
	reads     []interact.PageRequest
	sendErr   error
	promptErr error
}

func subscriptionDispatcherForTest(executions *agentExecutionStore, client subscriptions.Client) *subscriptionDispatcher {
	return subscriptions.NewDispatcher(executions.Control().CanonicalDatabase(), client, nil, nil)
}

func (c *recordingSubscriptionClient) SubscribeCompletedTurns(_ context.Context, request interact.PageRequest) (interact.CompletedTurnPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads = append(c.reads, request)
	return c.pages[request.ThreadRef+"\x00"+request.Cursor], nil
}

func (c *recordingSubscriptionClient) SendMessage(_ context.Context, request interact.SendMessageRequest) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, request)
	return json.RawMessage(`{"messageId":"message"}`), c.sendErr
}

func (c *recordingSubscriptionClient) SubmitPrompt(_ context.Context, request interact.SubmitPromptRequest) (interact.Turn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prompts = append(c.prompts, request)
	return interact.Turn{ID: "turn"}, c.promptErr
}

func TestCompletionSubscriptionUsesInteractForIndependentChannels(t *testing.T) {
	_, executions := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(executions, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithSubscriptions(executions, "child", "child-hash", "claude", presentationBackground, parent, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.Send})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{
		child.ID + "\x00": {
			Items:      []interact.CompletedTurn{{Provider: "claude", TurnID: "provider-turn", OperationID: "aop_1", Outcome: agentTurnFailed}},
			NextCursor: "provider-turn",
		},
	}}
	dispatcher := subscriptionDispatcherForTest(executions, client)
	dispatcher.Sweep("")
	if len(client.reads) != 1 || client.reads[0].ThreadRef != child.ID || client.reads[0].Cursor != "" {
		t.Fatalf("completed-turn reads = %+v", client.reads)
	}
	if len(client.messages) != 1 || len(client.prompts) != 1 {
		t.Fatalf("deliveries = %d inbox, %d prompt", len(client.messages), len(client.prompts))
	}
	if client.messages[0].RecipientThreadRef != parent.ID || client.messages[0].SenderThreadRef != child.ID {
		t.Fatalf("inbox delivery = %+v", client.messages[0])
	}
	if client.prompts[0].ThreadRef != parent.ID || client.prompts[0].Origin != interact.SystemOrigin {
		t.Fatalf("prompt delivery = %+v", client.prompts[0])
	}
	cursor, initialized, err := subscriptions.Cursor(executions.Control().CanonicalDatabase(), child.ID)
	if err != nil || !initialized || cursor != "provider-turn" {
		t.Fatalf("cursor = %q, initialized=%v, err=%v", cursor, initialized, err)
	}

	restartedClient := &recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{}}
	subscriptionDispatcherForTest(executions, restartedClient).Sweep("")
	if len(restartedClient.messages) != 0 || len(restartedClient.prompts) != 0 || len(restartedClient.reads) != 1 || restartedClient.reads[0].Cursor != "provider-turn" {
		t.Fatalf("restart = reads:%+v messages:%d prompts:%d", restartedClient.reads, len(restartedClient.messages), len(restartedClient.prompts))
	}
}

func TestCompletionSubscriptionInitializesLegacyCursorAtLatestWithoutReplay(t *testing.T) {
	_, executions := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(executions, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithInvoker(executions, "child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executions.Control().CanonicalDatabase().Exec(`DELETE FROM subscription_completed_cursors WHERE hosted_thread_id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	client := &recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{
		child.ID + "\x00": {Items: []interact.CompletedTurn{{TurnID: "historical-turn", Outcome: agentTurnCompleted}}, NextCursor: "historical-turn"},
	}}
	dispatcher := subscriptionDispatcherForTest(executions, client)
	dispatcher.Sweep("")
	if len(client.messages) != 0 || len(client.prompts) != 0 {
		t.Fatalf("legacy history replayed: messages=%d prompts=%d", len(client.messages), len(client.prompts))
	}
	cursor, initialized, err := subscriptions.Cursor(executions.Control().CanonicalDatabase(), child.ID)
	if err != nil || !initialized || cursor != "historical-turn" {
		t.Fatalf("initialized cursor = %q, %v, %v", cursor, initialized, err)
	}
	restarted := &recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{}}
	subscriptionDispatcherForTest(executions, restarted).Sweep("")
	if len(restarted.messages) != 0 || len(restarted.prompts) != 0 || len(restarted.reads) != 1 || restarted.reads[0].Cursor != "historical-turn" {
		t.Fatalf("restart redelivery = reads:%+v messages:%d prompts:%d", restarted.reads, len(restarted.messages), len(restarted.prompts))
	}
}

func TestCompletionSubscriptionSkipsExitedAndAdvancesPermanentFailure(t *testing.T) {
	_, executions := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(executions, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithInvoker(executions, "child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executions.Control().CanonicalDatabase().Exec(`UPDATE hosted_execution_sessions SET status='exited' WHERE hosted_thread_id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	if targets, err := subscriptions.Targets(executions.Control().CanonicalDatabase(), ""); err != nil || len(targets) != 0 {
		t.Fatalf("exited child remained subscribed: %+v", targets)
	}
	if _, err := executions.Control().CanonicalDatabase().Exec(`UPDATE hosted_execution_sessions SET status='creating' WHERE hosted_thread_id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := executions.Control().CanonicalDatabase().Exec(`UPDATE hosted_execution_sessions SET status='exited' WHERE hosted_thread_id=?`, parent.ID); err != nil {
		t.Fatal(err)
	}
	client := &recordingSubscriptionClient{sendErr: errors.New("recipient gone"), pages: map[string]interact.CompletedTurnPage{
		child.ID + "\x00": {Items: []interact.CompletedTurn{{TurnID: "gone-turn", Outcome: agentTurnCompleted}}, NextCursor: "gone-turn"},
	}}
	subscriptionDispatcherForTest(executions, client).SweepTarget(subscriptionTarget{
		ChildHostedID: child.ID, InvokerHostedID: parent.ID,
	})
	cursor, _, err := subscriptions.Cursor(executions.Control().CanonicalDatabase(), child.ID)
	if err != nil || cursor != "gone-turn" {
		t.Fatalf("permanent failure cursor = %q, %v", cursor, err)
	}
}

func TestCompletionSubscriptionDefaultsPersistAcrossReload(t *testing.T) {
	control, executions := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(executions, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithInvoker(executions, "child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := newControlAgentExecutionStore(control, "reloaded")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Session(child.ID)
	if !ok || !got.SubscriptionInbox || got.SubscriptionPrompt {
		t.Fatalf("reloaded subscriptions = %+v, present=%v", got, ok)
	}
}

func TestStoredLegacyNotificationModesLoseCoalescingAndAcknowledgement(t *testing.T) {
	for _, legacy := range []string{"individual", "coalesced"} {
		choices := subscriptions.FromStored(legacy, false)
		if choices.Inbox != subscriptions.None || choices.Prompt != subscriptions.Send {
			t.Fatalf("legacy %s = %+v, want one prompt subscription", legacy, choices)
		}
	}
}

// The dispatcher used to read every subscribed child's completed-turn feed
// once a second. It must read only when a child's turn is saved as finished,
// and the invoker must still hear about that turn. Boundary: a live hosted
// session, the core turn store and the running dispatcher.
func TestCompletionSubscriptionReactsToFinishedTurnsWithoutPolling(t *testing.T) {
	s, daemon, rec, client, dispatcher := subscribedCodexServer(t, `[{"id":"turn-live","status":"inProgress","startedAt":1787600000}]`)
	s.subscriptions = dispatcher
	dispatcher.Start()
	s.reattachWaitingHosted(rec.HostedThreadID)
	waitForClaude(t, "the live turn", func() bool {
		sess := s.SessionForHostedThread(rec.HostedThreadID)
		if sess == nil {
			return false
		}
		return sess.Summary().TurnActive
	})

	settled, _ := client.counts()
	time.Sleep(1200 * time.Millisecond)
	if reads, _ := client.counts(); reads != settled {
		t.Fatalf("completed-turn feed read %d times while no turn finished", reads-settled)
	}

	daemon.EmitThreadNotification(rec.NativeThreadID, map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": rec.NativeThreadID, "turn": map[string]any{"id": "turn-live", "status": "completed", "completedAt": 1787600100},
	}})
	waitForClaude(t, "the parent's inbox notice", func() bool {
		_, messages := client.counts()
		return messages == 1
	})
}

// preparingSubscriptionClient delivers a prompt notice the way the hosted
// turn route does: an invoker still starting after a restart is prepared
// first.
type preparingSubscriptionClient struct {
	feedSubscriptionClient
}

func (c *preparingSubscriptionClient) SubmitPrompt(ctx context.Context, request interact.SubmitPromptRequest) (interact.Turn, error) {
	c.s.reattachWaitingHosted(request.ThreadRef)
	return c.feedSubscriptionClient.SubmitPrompt(ctx, request)
}

// A prompt notice to an invoker that is still starting prepares that invoker,
// and the invoker's registration sweeps its own subscriptions. That sweep
// used to run on the registration path while the notice held the
// dispatcher, so delivery and registration waited on each other forever.
func TestCompletionNoticeToStartingInvokerDoesNotBlockItsRegistration(t *testing.T) {
	daemon := newFakeAppServer(t)
	daemon.SetResumeTurns(json.RawMessage(`[{"id":"turn-live","status":"inProgress","startedAt":1787600000}]`))
	s, store := bootReattachServer(t, "", daemon.WSURL())
	s.completedTurns = hostedstore.NewCompletedTurnSource(hostedstore.NewCompletedTurnStore(s.queuedTurns.Database()))
	parent, _, err := fixtureBeginSession(s.agentExecutions, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithSubscriptions(s.agentExecutions, "child", "child-hash", "codex", presentationBackground, parent, subscriptions.Choices{Inbox: subscriptions.None, Prompt: subscriptions.Send})
	if err != nil {
		t.Fatal(err)
	}
	parentRec := bootCodexRecord(t)
	parentRec.NativeThreadID, parentRec.HostedThreadID = "codex-parent", parent.ID
	childRec := bootCodexRecord(t)
	childRec.HostedThreadID = child.ID
	store.Remember(parentRec)
	store.Remember(childRec)
	seedBootAttachments(t, s)
	s.prepareBootReattach()
	client := &preparingSubscriptionClient{feedSubscriptionClient{s: s}}
	s.subscriptions = subscriptions.NewDispatcher(s.agentExecutions.Control().CanonicalDatabase(), client, nil, nil)
	s.subscriptions.Sweep("") // the child's cursor starts where a running Nano would have it

	s.reattachWaitingHosted(child.ID)
	waitForClaude(t, "the child's live turn", func() bool {
		sess := s.SessionForHostedThread(child.ID)
		if sess == nil {
			return false
		}
		return sess.Summary().TurnActive
	})
	daemon.EmitThreadNotification(childRec.NativeThreadID, map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": childRec.NativeThreadID, "turn": map[string]any{"id": "turn-live", "status": "completed", "completedAt": 1787600100},
	}})
	waitForClaude(t, "the notice to the prepared parent", func() bool {
		client.mu.Lock()
		delivered := len(client.prompts) == 1
		client.mu.Unlock()
		return delivered && s.SessionForHostedThread(parent.ID) != nil
	})
}

// blockingSubscriptionClient holds every delivery to one recipient until
// released, as a recipient still replaying after a restart did for 20 minutes.
type blockingSubscriptionClient struct {
	recordingSubscriptionClient
	blocked string
	entered chan struct{}
	release chan struct{}
}

func (c *blockingSubscriptionClient) SendMessage(ctx context.Context, request interact.SendMessageRequest) (json.RawMessage, error) {
	if request.RecipientThreadRef == c.blocked {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-c.release
	}
	return c.recordingSubscriptionClient.SendMessage(ctx, request)
}

// One parent that is slow to accept a notice used to hold every other
// parent's notices: the dispatcher kept its one lock across the delivery, and
// after a restart 83 sweeps queued behind it for 20 minutes. Boundary: the
// dispatcher over the control store, with the recipient's API held.
func TestCompletionNoticeToASlowParentDoesNotHoldAnotherParents(t *testing.T) {
	_, executions := newTestHostedControl(t)
	slow, _, err := fixtureBeginSession(executions, "slow-parent", "slow-hash", "claude", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := fixtureBeginSession(executions, "other-parent", "other-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	inbox := subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None}
	slowChild, _, err := fixtureBeginSessionWithSubscriptions(executions, "slow-child", "slow-child-hash", "codex", presentationBackground, slow, inbox)
	if err != nil {
		t.Fatal(err)
	}
	otherChild, _, err := fixtureBeginSessionWithSubscriptions(executions, "other-child", "other-child-hash", "codex", presentationBackground, other, inbox)
	if err != nil {
		t.Fatal(err)
	}
	finished := func(turn string) interact.CompletedTurnPage {
		return interact.CompletedTurnPage{Items: []interact.CompletedTurn{{TurnID: turn, Outcome: agentTurnCompleted}}, NextCursor: turn}
	}
	client := &blockingSubscriptionClient{
		recordingSubscriptionClient: recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{
			slowChild.ID + "\x00": finished("slow-turn"), otherChild.ID + "\x00": finished("other-turn"),
		}},
		blocked: slow.ID, entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	defer close(client.release)
	dispatcher := subscriptionDispatcherForTest(executions, client)
	for _, child := range []string{slowChild.ID, otherChild.ID} {
		if err := subscriptions.AdvanceCursor(executions.Control().CanonicalDatabase(), child, ""); err != nil {
			t.Fatal(err)
		}
	}
	go dispatcher.Sweep(slowChild.ID)
	<-client.entered
	done := make(chan struct{})
	go func() {
		dispatcher.Sweep(otherChild.ID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the other parent's notice waited for the slow parent")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.messages) != 1 || client.messages[0].RecipientThreadRef != other.ID {
		t.Fatalf("delivered = %+v, want one notice to the other parent", client.messages)
	}
}

// With no poll behind it, a sweep whose target lookup fails must try again
// by itself, or the child's completion notice is never delivered. Boundary:
// the dispatcher over the control store.
func TestCompletionSubscriptionRetriesAfterAFailedTargetLookup(t *testing.T) {
	_, executions := newTestHostedControl(t)
	parent, _, err := fixtureBeginSession(executions, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithSubscriptions(executions, "child", "child-hash", "claude", presentationBackground, parent, subscriptions.Choices{Inbox: subscriptions.Send, Prompt: subscriptions.None})
	if err != nil {
		t.Fatal(err)
	}
	client := &recordingSubscriptionClient{pages: map[string]interact.CompletedTurnPage{
		child.ID + "\x00": {Items: []interact.CompletedTurn{{TurnID: "turn-1", Outcome: agentTurnCompleted}}, NextCursor: "turn-1"},
	}}
	dispatcher := subscriptionDispatcherForTest(executions, client)
	db := executions.Control().CanonicalDatabase()
	if _, err := db.Exec(`ALTER TABLE hosted_threads RENAME TO hosted_threads_unavailable`); err != nil {
		t.Fatal(err)
	}
	dispatcher.Sweep(child.ID)
	if _, err := db.Exec(`ALTER TABLE hosted_threads_unavailable RENAME TO hosted_threads`); err != nil {
		t.Fatal(err)
	}
	waitForClaudeWithin(t, 20*time.Second, "the notice after the lookup recovers", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return len(client.messages) == 1
	})
}
