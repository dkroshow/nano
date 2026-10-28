package hosted

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The bookmark contract mirrors the fleet feed: empty, foreign, and expired
// cursors resnapshot with a reason, and every response carries a usable
// bookmark for the next pull.
func TestAgentChangeLogBookmarkContract(t *testing.T) {
	log := newAgentChangeLog()

	first := log.since("", nil)
	if !first.Resnapshot || first.Reason != agentResnapshotNoBookmark || first.Bookmark == "" {
		t.Fatalf("empty bookmark = %+v, want resnapshot no_bookmark with a fresh bookmark", first)
	}
	if foreign := log.since("deadbeef.3", nil); !foreign.Resnapshot || foreign.Reason != agentResnapshotUnknown {
		t.Fatalf("foreign bookmark = %+v, want unknown_bookmark", foreign)
	}

	log.append(AgentOperationTransition{OperationID: "op-1", From: "", To: agentTurnQueued})
	log.append(AgentOperationTransition{OperationID: "op-1", From: agentTurnQueued, To: agentTurnRunning})
	page := log.since(first.Bookmark, nil)
	if page.Resnapshot || len(page.Transitions) != 2 ||
		page.Transitions[0].To != agentTurnQueued || page.Transitions[1].To != agentTurnRunning {
		t.Fatalf("page = %+v, want both transitions in order", page)
	}
	if drained := log.since(page.Bookmark, nil); len(drained.Transitions) != 0 || drained.Resnapshot {
		t.Fatalf("drained = %+v, want an empty page", drained)
	}

	for i := 0; i < agentChangeRingSize+8; i++ {
		log.append(AgentOperationTransition{OperationID: fmt.Sprintf("op-%d", i)})
	}
	if expired := log.since(page.Bookmark, nil); !expired.Resnapshot || expired.Reason != agentResnapshotExpired {
		t.Fatalf("overrun cursor = %+v, want bookmark_expired", expired)
	}
}

// Only semantic status changes reach the ring. Reattachment bulk-touches
// (bindSession, updateSession) bump every non-terminal turn's revision, and a
// same-status delivery mark changes nothing — neither may emit.
func TestAgentChangesEmitSemanticTransitionsOnly(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := store.beginSession("caller", "hash", "claude", presentationBackground)
	op, _, _ := store.beginTurn(hosted.ID, "action-1", "prompt-hash")

	base := store.changes.since("", nil)
	if err := store.BindSession(hosted.ID, "nano-2", "native-1", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSession(hosted.ID, "nano-3", "", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnDelivery(op.ID, "", agentTurnDeliveryPending, nil); err != nil {
		t.Fatal(err)
	}
	if page := store.changes.since(base.Bookmark, nil); len(page.Transitions) != 0 {
		t.Fatalf("bulk-touch and same-status marks emitted %+v, want silence", page.Transitions)
	}

	if err := store.MarkTurnDelivery(op.ID, "", agentTurnQueued, nil); err != nil {
		t.Fatal(err)
	}
	page := store.changes.since(base.Bookmark, nil)
	if len(page.Transitions) != 1 || page.Transitions[0].From != agentTurnDeliveryPending ||
		page.Transitions[0].To != agentTurnQueued || page.Transitions[0].OperationID != op.ID {
		t.Fatalf("semantic change page = %+v, want exactly the delivery_pending→queued transition", page.Transitions)
	}
}

// Transitions carry the invoker edge stamped from the session record, so a
// parent can filter the feed to its own workers without a join.
func TestAgentChangesCarryInvokerEdge(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	parent, _, _ := store.beginSession("parent", "hash-p", "codex", presentationShared)
	child, _, err := store.beginSessionWithInvoker("child", "hash-c", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	base := store.changes.since("", nil)
	_, _, _ = store.beginTurn(child.ID, "action-1", "prompt-hash")

	mine := store.changes.since(base.Bookmark, func(tr AgentOperationTransition) bool {
		return tr.InvokerHostedThreadID == parent.ID
	})
	if len(mine.Transitions) != 1 || mine.Transitions[0].HostedThreadID != child.ID {
		t.Fatalf("filtered page = %+v, want the child's creation transition", mine.Transitions)
	}
	others := store.changes.since(base.Bookmark, func(tr AgentOperationTransition) bool {
		return tr.InvokerHostedThreadID == "someone-else"
	})
	if len(others.Transitions) != 0 {
		t.Fatalf("foreign filter matched %+v", others.Transitions)
	}
}

// waitChanges blocks on the store broadcast and wakes when a transition
// lands — the composite "any worker changed" primitive.
func TestAgentChangesLongPollWakes(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := store.beginSession("caller", "hash", "claude", presentationBackground)
	op, _, _ := store.beginTurn(hosted.ID, "action-1", "prompt-hash")
	base := store.changes.since("", nil)

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = store.MarkTurnDelivery(op.ID, "", agentTurnRunning, nil)
	}()
	start := time.Now()
	page := store.waitChanges(context.Background(), base.Bookmark, 3*time.Second, nil)
	if len(page.Transitions) != 1 || page.Transitions[0].To != agentTurnRunning {
		t.Fatalf("woken page = %+v, want the running transition", page.Transitions)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waitChanges slept to its deadline instead of waking on the transition")
	}
}
