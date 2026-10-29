package app

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/diagnostics"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/features/feedback"
)

// What an agent reads back for a saved note: the resolved thread, the build,
// the pane state, and only the saving tab's events for that thread (plus its
// thread-less page events), in the tab's order. Another tab or another
// thread's pane in the same tab is not this note's evidence, and a save after
// Nano restarted since the browser's upload is refused, not kept without it.
func TestFeedbackKeepsTheSavingTabsEvidenceForTheThread(t *testing.T) {
	control, executions := newTestHostedControl(t)
	hosted := seedHostedNative(t, executions, "claude", pinNativeA, "ready")
	store, err := feedback.Open(filepath.Join(t.TempDir(), "feedback.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := &server{
		Registry: newSessionRegistry(), agentExecutions: executions, controlMetadata: control,
		browserDiagnostics: newBrowserDiagnosticStore(100), feedback: store,
	}
	f := &pinFixture{server: s, mux: http.NewServeMux()}
	mountRoutes(f.mux, applicationRoutes(fixtureOwners(s)))

	code, raw := f.do(t, http.MethodPost, "/api/v1/browser-diagnostics", map[string]any{"events": []map[string]any{
		// A replay re-adds an evicted older event after a newer one.
		{"eventId": "tab-1:2", "clientSeq": 2, "kind": "send.post.start", "tabId": "tab-1", "threadId": pinNativeA, "hostedThreadId": hosted.ID},
		{"eventId": "tab-1:1", "clientSeq": 1, "kind": "page.load", "tabId": "tab-1"},
		{"eventId": "tab-1:3", "kind": "send.post.start", "tabId": "tab-1", "threadId": "other-native"},
		{"eventId": "tab-2:1", "kind": "send.post.start", "tabId": "tab-2", "threadId": pinNativeA},
	}})
	if code != http.StatusOK {
		t.Fatalf("diagnostics upload = %d %s", code, raw)
	}
	request := map[string]any{
		"clientActionId": "feedback-1", "note": "  the reply vanished  ", "bootId": "an-earlier-boot", "hostedThreadId": hosted.ID, "threadId": pinNativeA,
		"provider": "claude", "view": "dev", "tabId": "tab-1", "pageId": "page-1", "paneId": "pane-1", "build": "index-old.js",
		"pane": map[string]any{"working": true, "pendingPrompts": 1, "lastSavedRowId": "row-7"},
	}
	// The ring went to a Nano that has since restarted: its copy is gone.
	if code, raw = f.do(t, http.MethodPost, "/api/v1/feedback", request); code != http.StatusConflict {
		t.Fatalf("save after a restart = %d %s, want 409", code, raw)
	}
	request["bootId"] = s.browserDiagnostics.BootID()
	code, raw = f.do(t, http.MethodPost, "/api/v1/feedback", request)
	if code != http.StatusCreated {
		t.Fatalf("save = %d %s", code, raw)
	}
	saved := decodePinJSON[feedback.Note](t, raw)
	if saved.Events != nil || saved.EventCount != 2 {
		t.Fatalf("save response = %+v", saved)
	}

	code, raw = f.do(t, http.MethodGet, "/api/v1/feedback/"+saved.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("read = %d %s", code, raw)
	}
	note := decodePinJSON[feedback.Note](t, raw)
	var events []diagnostics.Event
	if err := json.Unmarshal(note.Events, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].EventID != "tab-1:1" || events[1].EventID != "tab-1:2" {
		t.Fatalf("evidence = %+v", events)
	}
	want := feedback.Note{
		ID: saved.ID, CreatedAt: saved.CreatedAt, Text: "the reply vanished",
		Thread: feedback.Thread{HostedThreadID: hosted.ID, NativeThreadID: pinNativeA, DisplayName: note.Thread.DisplayName, Provider: "claude"},
		Build:  feedback.Build{Stamp: buildStamp, Bundle: embeddedBundleName(), TabBundle: "index-old.js"},
		View:   "dev", Pane: feedback.Pane{Working: true, PendingPrompts: 1, LastSavedRowID: "row-7"},
		Browser:    feedback.Browser{TabID: "tab-1", PageID: "page-1", PaneID: "pane-1"},
		EventCount: 2,
	}
	note.Events = nil
	gotJSON, _ := json.Marshal(note)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("note\n got %s\nwant %s", gotJSON, wantJSON)
	}

	code, raw = f.do(t, http.MethodGet, "/api/v1/feedback?threadId="+hosted.ID, nil)
	list := decodePinJSON[struct{ Items []feedback.Note }](t, raw)
	if code != http.StatusOK || len(list.Items) != 1 || list.Items[0].ID != saved.ID || list.Items[0].Events != nil {
		t.Fatalf("list = %d %s", code, raw)
	}
}
