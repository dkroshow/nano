package diagnostics

import "testing"

func TestStoreRejectsContentTokensAndRetainsNewestEvents(t *testing.T) {
	store := NewStore(2, "boot-1")
	accepted := store.Add([]Event{
		{EventID: "bad event with spaces", Kind: "projection.open"},
		{EventID: "tab:1", Kind: "projection.open", ThreadID: "thread-1"},
		{EventID: "tab:2", Kind: "projection.message", ThreadID: "thread-1"},
		{EventID: "tab:3", Kind: "pane.render", ThreadID: "thread-1"},
	})
	if accepted != 3 {
		t.Fatalf("accepted = %d, want 3", accepted)
	}
	events := store.Recent(10, "thread-1", "", "")
	if store.BootID() != "boot-1" || len(events) != 2 || events[0].EventID != "tab:2" || events[1].EventID != "tab:3" {
		t.Fatalf("store = boot %q, events %#v", store.BootID(), events)
	}
}
