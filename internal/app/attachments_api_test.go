package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

func serverWithAttachments(t *testing.T) (*server, *hostedstore.AttachmentStore) {
	t.Helper()
	store, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		attachments: store,
		Registry:    newSessionRegistry(),
	}, store
}

// liveSession makes a minimal ready session bound to a native thread id, enough
// for handleSessionsList and the DELETE path (no provider/ spool to tear down).
func liveSession(t *testing.T, id, threadID string) *session {
	return configuredFixtureSession(t, sessionConfig{ID: id, Provider: "codex", CreatedAt: time.Now()}, threadID, "ready", "")
}

// GET /api/sessions surfaces detached attachments, and a thread with a live
// session is attached (absent from detached) — acceptance criterion 2's
// server half: no runtime overlay for detached because they're never in the
// sessions map.
func TestSessionsGETSurfacesDetached(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("t-live", "/a", "2026-07-25T10:00:00Z"))
	store.Remember(attach("t-detached", "/b", "2026-07-25T11:00:00Z"))
	s.RegisterSession(liveSession(t, "sess1", "t-live"))

	rec := httptest.NewRecorder()
	s.handleSessionsList(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	var body struct {
		Sessions []sessionSummary   `json:"sessions"`
		Detached []attachmentRecord `json:"detached"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].ThreadID != "t-live" {
		t.Fatalf("sessions = %+v", body.Sessions)
	}
	if len(body.Detached) != 1 || body.Detached[0].NativeThreadID != "t-detached" {
		t.Fatalf("detached = %+v, want only t-detached", body.Detached)
	}
}

// An exited session is only an error record, not a live attachment. It must
// not hide the durable attachment's retry row after a failed resume.
func TestSessionsGETExitedSessionDoesNotHideDetached(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("t-retry", "/a", "2026-07-25T10:00:00Z"))
	exited := liveSession(t, "sess-exited", "t-retry")
	exited.Close()
	s.RegisterSession(exited)

	rec := httptest.NewRecorder()
	s.handleSessionsList(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	var body struct {
		Sessions []sessionSummary   `json:"sessions"`
		Detached []attachmentRecord `json:"detached"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].Status != sessionStatusExited {
		t.Fatalf("sessions = %+v, want the exited error record", body.Sessions)
	}
	if len(body.Detached) != 1 || body.Detached[0].NativeThreadID != "t-retry" {
		t.Fatalf("detached = %+v, want the exited thread to remain retryable", body.Detached)
	}
}

// DELETE /api/sessions/{id} forgets the attachment — the remove-on-explicit-
// close half of acceptance criterion 1. After close, the thread is gone from
// detached, not resurrected as a ghost.
func TestSessionDeleteForgetsAttachment(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("t-1", "/a", "2026-07-25T10:00:00Z"))
	s.RegisterSession(liveSession(t, "sess1", "t-1"))

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/sess1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code = %d", rec.Code)
	}
	if got := store.DetachedExcept(nil); len(got) != 0 {
		t.Fatalf("attachment survived explicit close: %+v", got)
	}
}

// A claude session and a codex session can hold the same native id without
// hiding each other's detached row: the live map is keyed by provider too.
func TestSessionsGETDetachedIsProviderScoped(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("shared", "/a", "2026-07-25T10:00:00Z"))
	store.Remember(attachmentRecord{
		Provider:       "claude",
		NativeThreadID: "shared",
		Cwd:            "/b",
		AttachedAt:     "2026-07-25T11:00:00Z",
	})
	s.RegisterSession(liveSession(t, "sess1", "shared")) // codex

	rec := httptest.NewRecorder()
	s.handleSessionsList(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	var body struct {
		Detached []attachmentRecord `json:"detached"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Detached) != 1 || body.Detached[0].Provider != "claude" {
		t.Fatalf("detached = %+v, want only the claude record", body.Detached)
	}
}

// DELETE /api/sessions/{id} forgets only the closing session's provider — a
// same-id record under another provider is somebody else's session.
func TestSessionDeleteForgetsOwnProviderOnly(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("shared", "/a", "2026-07-25T10:00:00Z"))
	store.Remember(attachmentRecord{
		Provider:       "claude",
		NativeThreadID: "shared",
		AttachedAt:     "2026-07-25T11:00:00Z",
	})
	s.RegisterSession(liveSession(t, "sess1", "shared")) // codex

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/sessions/sess1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE code = %d", rec.Code)
	}
	got := store.DetachedExcept(nil)
	if len(got) != 1 || got[0].Provider != "claude" {
		t.Fatalf("after close = %+v, want the claude record untouched", got)
	}
}

// A bare DELETE cannot widen a close to multiple providers.
func TestAttachmentDismissProviderParam(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("shared", "/a", "2026-07-25T10:00:00Z"))
	claudeRec := attachmentRecord{
		Provider:       "claude",
		NativeThreadID: "shared",
		AttachedAt:     "2026-07-25T11:00:00Z",
	}
	store.Remember(claudeRec)

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/attachments/shared?provider=codex", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped dismiss code = %d", rec.Code)
	}
	got := store.DetachedExcept(nil)
	if len(got) != 1 || got[0].Provider != "claude" {
		t.Fatalf("scoped dismiss = %+v, want only the claude record left", got)
	}

	// Restore the codex record; ambiguous dismissal must preserve both.
	store.Remember(attach("shared", "/a", "2026-07-25T10:00:00Z"))
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/attachments/shared", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("bare dismiss code = %d", rec.Code)
	}
	if got := store.DetachedExcept(nil); len(got) != 2 {
		t.Fatalf("ambiguous dismiss changed records: %+v", got)
	}
}

// DELETE /api/attachments/{id} dismisses a detached entry without resuming it,
// and is idempotent for an unknown id.
func TestAttachmentDismissEndpoint(t *testing.T) {
	s, store := serverWithAttachments(t)
	store.Remember(attach("t-1", "/a", "2026-07-25T10:00:00Z"))

	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/attachments/t-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss code = %d", rec.Code)
	}
	if got := store.DetachedExcept(nil); len(got) != 0 {
		t.Fatalf("dismiss left record: %+v", got)
	}

	// Unknown id is a no-op success, not a 404 — a stale client mustn't error.
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/attachments/unknown", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss-unknown code = %d, want 200", rec.Code)
	}

	// Non-DELETE is rejected.
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/attachments/t-1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d, want 405", rec.Code)
	}
}

// With no store wired (feature off), GET still succeeds with an empty detached
// array and never panics.
func TestSessionsGETNoStore(t *testing.T) {
	s := &server{Registry: newSessionRegistry()}
	rec := httptest.NewRecorder()
	s.handleSessionsList(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	var body struct {
		Detached []attachmentRecord `json:"detached"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Detached) != 0 {
		t.Fatalf("detached = %+v, want empty", body.Detached)
	}
}
