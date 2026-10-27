package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newFileOnlyClaude is newClaudeHandoff for a session wired as production
// wires every Claude session: its reads come from the saved file.
func newFileOnlyClaude(t *testing.T) *claudeHandoff {
	t.Helper()
	h := newClaudeHandoff(t)
	h.catalog.server.startLiveText(h.session)
	if h.session.liveText == nil {
		t.Fatal("a Claude session was not wired file-only")
	}
	return h
}

// saveTurn runs one prompted Claude turn live, then writes its saved copy and
// waits for the catalog to commit it.
func (h *claudeHandoff) saveTurn(t *testing.T, action, providerTurn, prompt, answer string) string {
	t.Helper()
	eventID := h.submit(t, action, providerTurn, prompt)
	h.reply(eventID, answer)
	h.finish(answer)
	var saved strings.Builder
	h.writeSavedTurn(&saved, eventID, prompt, answer)
	h.appendSaved(t, saved.String(), answer)
	return eventID
}

// TestFileOnlyClaudeFileOpensBySavedOrLiveEvidence catches a file a live pane
// names only by its base name becoming unreadable: the files API must find it
// in the thread's saved rows, and in a reply the file has not saved yet.
// Boundary: the files API, which the pane calls with only its session.
func TestFileOnlyClaudeFileOpensBySavedOrLiveEvidence(t *testing.T) {
	h := newFileOnlyClaude(t)
	dir := t.TempDir()
	saved, unsaved := filepath.Join(dir, "notes.md"), filepath.Join(dir, "draft.md")
	writeTestFile(t, saved, "notes\n")
	writeTestFile(t, unsaved, "draft\n")
	h.saveTurn(t, "action-0", "claude:turn:0", "edit "+saved, "partial edit of "+saved)
	// A reply the file has not saved yet, still streaming, names the second file.
	h.submit(t, "action-1", "claude:turn:1", "continue")
	h.stream("unsaved", "now editing "+unsaved)
	for name, want := range map[string]string{"notes.md": saved, "draft.md": unsaved} {
		rec := httptest.NewRecorder()
		h.catalog.server.handleGeneralFileRead(rec, httptest.NewRequest(http.MethodGet, "/api/v1/files/read?path="+name+"&sessionId="+h.session.id, nil))
		var file struct {
			Path       string `json:"path"`
			ResolvedBy string `json:"resolvedBy"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &file) != nil || file.Path != canonicalPath(want) || file.ResolvedBy != "thread-evidence" {
			t.Fatalf("%s: %d %s; want %q by thread-evidence", name, rec.Code, rec.Body.String(), canonicalPath(want))
		}
	}
}
