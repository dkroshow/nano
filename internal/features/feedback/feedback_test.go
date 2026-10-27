package feedback

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func sampleNote() Note {
	return Note{
		ID: NewID(), CreatedAt: "2026-10-09T01:07:51Z", Text: "the reply vanished after I sent it",
		Thread: Thread{HostedThreadID: "nht_target", NativeThreadID: "native-1", DisplayName: "Sample thread", Provider: "claude"},
		Build:  Build{Stamp: "stamp-1", Bundle: "index-a.js", TabBundle: "index-old.js"},
		View:   "enhanced", Pane: Pane{Working: true, PendingPrompts: 2, LastSavedRowID: "row-9"},
		Browser:    Browser{TabID: "tab-1", PageID: "page-1", PaneID: "pane-1"},
		EventCount: 1, Events: json.RawMessage(`[{"eventId":"tab-1:1","kind":"send.post.start"}]`),
	}
}

// A restart wipes Nano's in-memory diagnostic ring; the note must keep its
// own copy of that evidence and every context field.
func TestNoteAndEvidenceSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sampleNote()
	if _, created, err := store.Create(want, "action-1", "hash-1"); err != nil || !created {
		t.Fatalf("create = %v, %v", created, err)
	}
	store.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Get(want.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("reopened note\n got %s\nwant %s", gotJSON, wantJSON)
	}
	listed, err := reopened.List("native-1", 0)
	if err != nil || len(listed) != 1 || listed[0].ID != want.ID || listed[0].Events != nil {
		t.Fatalf("list = %+v, %v; want the note without its evidence", listed, err)
	}
	if _, created, err := reopened.Create(sampleNote(), "action-1", "hash-1"); err != nil || created {
		t.Fatalf("retried save created=%v err=%v; want the original note", created, err)
	}
	if _, _, err := reopened.Create(sampleNote(), "action-1", "other"); !errors.Is(err, ErrClientActionReused) {
		t.Fatalf("reused action err = %v", err)
	}
}
