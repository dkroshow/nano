package sidebar

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

// StarThread applies the current operator-owned star to a response row.
func (s *Controller) StarThread(thread *Thread) {
	if s == nil || s.threadStars == nil || thread == nil {
		return
	}
	thread.Starred = s.threadStars.IsStarred(thread.Provider, thread.NativeID)
}

func (s *Controller) attachStarredOverlay(threads []Thread) {
	for i := range threads {
		s.StarThread(&threads[i])
	}
}

func (s *Controller) attachStarredOnSessions(summaries []sessions.Summary) {
	if s == nil || s.threadStars == nil {
		return
	}
	for i := range summaries {
		summaries[i].Starred = s.threadStars.IsStarred(summaries[i].Provider, summaries[i].ThreadID)
	}
}

// HandleThreadStarred persists a star change and updates connected browsers.
func (s *Controller) HandleThreadStarred(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.threadStars == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "stars_unavailable", "Thread stars store unavailable")
		return
	}
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	if rawID == "" || strings.Contains(rawID, "/") {
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
		return
	}
	reference, err := url.PathUnescape(rawID)
	if err != nil || strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	id, err := threadindex.ResolveReference(r.Context(), s.catalog, reference)
	if err != nil {
		threadindex.WriteThreadReferenceError(w, err)
		return
	}
	saved, err := s.catalog.LoadThread(r.Context(), id)
	thread := Thread{Thread: saved}
	if err != nil {
		threadindex.WriteCatalogBackendError(w, err)
		return
	}
	var payload struct {
		Starred *bool `json:"starred"`
	}
	body := http.MaxBytesReader(w, r.Body, threadindex.MaxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil || payload.Starred == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_starred", `Request body must be {"starred": true} or {"starred": false}`)
		return
	}
	if err := s.threadStars.SetStarred(thread.Provider, thread.NativeID, *payload.Starred); err != nil {
		if errors.Is(err, ErrStarIdentityMissing) {
			httpapi.WriteError(w, http.StatusConflict, "star_identity_missing", "Thread has no provider-native id to star yet")
			return
		}
		httpapi.WriteError(w, http.StatusInternalServerError, "star_error", "Thread star update failed")
		return
	}
	s.AttachRuntimeOverlay(&thread)
	s.StarThread(&thread)
	sidebarCurrent := s != nil && s.applyStar(thread.Provider, thread.NativeID, *payload.Starred)
	// Open browsers on every device share this SQLite file through this
	// Controller. Nudge them to refetch so a pin on the laptop is not stuck in
	// the iPad's in-memory snapshot until a hard refresh.
	s.PublishRuntimeDelta(true, sidebarCurrent)
	threadindex.WriteCatalogJSON(w, http.StatusOK, thread)
}
