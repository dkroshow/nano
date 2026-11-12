package catalog

import (
	"testing"
	"time"
)

func TestHubConflatesWithoutBlocking(t *testing.T) {
	hub := NewHub()
	slow := hub.Subscribe()
	fast := hub.Subscribe()
	done := make(chan struct{})
	go func() {
		hub.Notify(Change{Version: 1, ThreadIDs: []string{"a"}})
		hub.Notify(Change{Version: 2, ThreadIDs: []string{"b"}})
		hub.Notify(Change{Version: 3, ThreadIDs: []string{"c"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked on a slow subscriber")
	}
	for _, ch := range []chan Change{slow, fast} {
		change := <-ch
		if change.Version != 3 || change.Refresh || len(change.ThreadIDs) != 3 {
			t.Fatalf("change = %#v", change)
		}
	}
	hub.Unsubscribe(fast)
	hub.Unsubscribe(fast)
	hub.Notify(Change{Version: 4, Refresh: true})
	if change := <-slow; change.Version != 4 || !change.Refresh {
		t.Fatalf("refresh = %#v", change)
	}
	// A structural change names its threads. Conflation dropped them, and the
	// sidebar read its whole graph for every new thread file instead of
	// repairing the rows named.
	hub.Notify(Change{Version: 5, ThreadIDs: []string{"new"}, Refresh: true})
	hub.Notify(Change{Version: 6, ThreadIDs: []string{"d"}})
	if change := <-slow; change.Version != 6 || !change.Refresh || len(change.ThreadIDs) != 2 {
		t.Fatalf("structural refresh = %#v", change)
	}
	hub.Notify(Change{Version: 7, ThreadIDs: []string{"new"}, Refresh: true})
	hub.Notify(Change{Version: 8, Refresh: true})
	if change := <-slow; change.Version != 8 || !change.Refresh || len(change.ThreadIDs) != 0 {
		t.Fatalf("a refresh that names nothing must cover everything: %#v", change)
	}
}

func TestHubCloseDisconnectsSubscribers(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe()
	hub.Close()
	if _, ok := <-ch; ok {
		t.Fatal("subscriber remained open")
	}
	if _, ok := <-hub.Subscribe(); ok {
		t.Fatal("subscribe after close remained open")
	}
	hub.Close()
}
