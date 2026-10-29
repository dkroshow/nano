package sidebar

import (
	"fmt"
	"slices"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

// TestRuntimeHubConflatesAndNeverBlocks mirrors the catalog hub guarantees for
// the runtime hub: a never-draining subscriber cannot stall notify, repeated
// nudges conflate into the single pending slot, and unsubscribe is idempotent.
func TestRuntimeHubConflatesAndNeverBlocks(t *testing.T) {
	hub := NewRuntimeHub()
	slow := hub.Subscribe()

	done := make(chan struct{})
	go func() {
		hub.Notify()
		hub.Notify()
		hub.Notify()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked on a slow subscriber")
	}
	if len(slow) != 1 {
		t.Fatalf("pending nudges = %d, want 1 (conflated)", len(slow))
	}
	first := <-slow
	hub.Notify(RuntimeDelta{Rows: []RuntimeRow{{Provider: "codex", NativeID: "old"}}})
	hub.Notify(RuntimeDelta{Rows: []RuntimeRow{{Provider: "codex", NativeID: "latest"}}})
	latest := <-slow
	if len(first.Rows) != 0 || len(latest.Rows) != 2 || latest.Rows[0].NativeID != "latest" || latest.Rows[1].NativeID != "old" {
		t.Fatalf("runtime deltas = first:%#v latest:%#v, want the pending rows merged", first, latest)
	}

	hub.Unsubscribe(slow)
	hub.Unsubscribe(slow) // idempotent, no double-close
	hub.Notify()          // no subscribers, no panic

}

func TestRuntimeHubPreservesPendingRefresh(t *testing.T) {
	hub := NewRuntimeHub()
	slow := hub.Subscribe()
	defer hub.Unsubscribe(slow)

	hub.Notify(RuntimeDelta{Refresh: true})
	hub.Notify(RuntimeDelta{Rows: []RuntimeRow{{Provider: "codex", NativeID: "latest"}}})
	latest := <-slow
	if !latest.Refresh || len(latest.Rows) != 1 || latest.Rows[0].NativeID != "latest" {
		t.Fatalf("conflated runtime delta = %#v, want latest rows with pending refresh", latest)
	}
}

// A slow subscriber's pending deltas merge; small changes that add up past
// the bound reach it as one refresh marker, not as an unbounded event.
func TestRuntimeHubBoundsAMergedBacklog(t *testing.T) {
	hub := NewRuntimeHub()
	slow := hub.Subscribe()
	defer hub.Unsubscribe(slow)
	for i := range runtimeChangeLimit + 1 {
		hub.Notify(RuntimeDelta{Rows: []RuntimeRow{{Provider: "codex", NativeID: fmt.Sprintf("t-%02d", i)}}})
	}
	merged := <-slow
	if !merged.Refresh || merged.Rows == nil || len(merged.Rows) != 0 {
		t.Fatalf("merged backlog = refresh:%v rows %d, want an empty refresh marker", merged.Refresh, len(merged.Rows))
	}
}

// A page reads the session list at a stream position and connects from it.
// Before, runtime events were not replayed, so every page load read the list
// again when its stream opened. A connect from a position this process still
// holds must replay exactly what came after it; one it cannot replay must
// refresh, or the page keeps a stale list with nothing to correct it.
func TestRuntimeHubReplaysFromAListReadPosition(t *testing.T) {
	hub := NewRuntimeHub()
	row := func(id string) RuntimeDelta {
		return RuntimeDelta{Rows: []RuntimeRow{{Provider: "codex", NativeID: id}}}
	}
	hub.Notify(row("before-read"))
	read := hub.Position()
	hub.Notify(row("missed-1"))
	hub.Notify(row("missed-2"))
	connect := func(from RuntimePosition) (RuntimeDelta, bool) {
		ch := hub.SubscribeFrom(from)
		defer hub.Unsubscribe(ch)
		select {
		case delta := <-ch:
			return delta, true
		default:
			return RuntimeDelta{}, false
		}
	}

	replayed, ok := connect(read)
	ids := []string{}
	for _, r := range replayed.Rows {
		ids = append(ids, r.NativeID)
	}
	if !ok || replayed.Refresh || !slices.Equal(ids, []string{"missed-1", "missed-2"}) || replayed.Seq != read.Seq+2 {
		t.Fatalf("connect from the read = %v %+v, want missed-1 and missed-2 at seq %d", ok, replayed, read.Seq+2)
	}
	if delta, ok := connect(hub.Position()); ok {
		t.Fatalf("connect from the current position replayed %+v, want nothing", delta)
	}
	for name, from := range map[string]RuntimePosition{
		"another process": {Generation: "restarted", Seq: read.Seq},
		"no read yet":     {},
	} {
		if delta, ok := connect(from); !ok || !delta.Refresh {
			t.Fatalf("connect from %s = %v %+v, want a refresh", name, ok, delta)
		}
	}
	for i := range runtimeReplayLimit {
		hub.Notify(row(fmt.Sprintf("later-%02d", i)))
	}
	if delta, ok := connect(read); !ok || !delta.Refresh {
		t.Fatalf("connect from a position older than the kept deltas = %v %+v, want a refresh", ok, delta)
	}
}

// A slow subscriber reads merged deltas; merging session changes wrongly
// would leave its page holding a session that left, or missing one that
// came back, with no list read to correct it.
func TestRuntimeHubMergesSessionChanges(t *testing.T) {
	hub := NewRuntimeHub()
	slow := hub.Subscribe()
	defer hub.Unsubscribe(slow)

	row := func(id string) ListedSession { return ListedSession{Summary: sessions.Summary{ID: id}} }
	detached := func(native string) DetachedSessionView {
		return DetachedSessionView{Attachment: hostedstore.Attachment{Provider: "codex", NativeThreadID: native}}
	}
	hub.Notify(RuntimeDelta{
		Sessions: []ListedSession{row("came-and-left")}, RemovedSessions: []string{"left-and-came"},
		DetachedRows: []DetachedSessionView{detached("parked-then-attached")}, RemovedDetached: []string{"codex:attached-then-parked"},
	})
	hub.Notify(RuntimeDelta{
		Sessions: []ListedSession{row("left-and-came")}, RemovedSessions: []string{"came-and-left"},
		DetachedRows: []DetachedSessionView{detached("attached-then-parked")}, RemovedDetached: []string{"codex:parked-then-attached"},
	})
	merged := <-slow
	if len(merged.Sessions) != 1 || merged.Sessions[0].ID != "left-and-came" ||
		!slices.Equal(merged.RemovedSessions, []string{"came-and-left"}) {
		t.Fatalf("merged session changes = sessions %+v removed %v, want left-and-came present and came-and-left removed",
			merged.Sessions, merged.RemovedSessions)
	}
	if len(merged.DetachedRows) != 1 || merged.DetachedRows[0].NativeThreadID != "attached-then-parked" ||
		!slices.Equal(merged.RemovedDetached, []string{"codex:parked-then-attached"}) {
		t.Fatalf("merged detached changes = rows %+v removed %v, want attached-then-parked present and parked-then-attached removed",
			merged.DetachedRows, merged.RemovedDetached)
	}
}

// A refresh that needs the sidebar rebuilt, merged with a later star update
// that applied cleanly, must still rebuild: the materializer reads one flag.
func TestRuntimeHubMergeKeepsRebuildNeed(t *testing.T) {
	hub := NewRuntimeHub()
	slow := hub.Subscribe()
	defer hub.Unsubscribe(slow)

	hub.Notify(RuntimeDelta{Refresh: true})
	hub.Notify(RuntimeDelta{Refresh: true, SidebarCurrent: true, Rows: []RuntimeRow{}})
	merged := <-slow
	if !merged.Refresh || merged.SidebarCurrent {
		t.Fatalf("merged delta = %+v, want refresh with the rebuild still needed", merged)
	}
	if merged.Rows == nil {
		t.Fatal("merged delta lost its rows field; browsers read a missing one as a whole-list read")
	}
}
