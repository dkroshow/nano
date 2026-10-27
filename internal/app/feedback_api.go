package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/features/feedback"
	"github.com/dkroshow/nano/internal/httpapi"
)

const (
	maxFeedbackNoteBytes    = 4000
	maxFeedbackRequestBytes = 16 << 10
	// A tab's ring holds at most this many events, so a snapshot of that tab
	// never needs more.
	maxFeedbackEvidenceEvents = 600
)

type feedbackCreateRequest struct {
	ClientActionID string `json:"clientActionId"`
	Note           string `json:"note"`
	// BootID is the diagnostics boot that acknowledged the tab's ring upload.
	BootID         string `json:"bootId"`
	HostedThreadID string `json:"hostedThreadId"`
	ThreadID       string `json:"threadId"`
	Provider       string `json:"provider"`
	View           string `json:"view"`
	TabID          string `json:"tabId"`
	PageID         string `json:"pageId"`
	PaneID         string `json:"paneId"`
	Build          string `json:"build"`
	Pane           struct {
		Working        bool   `json:"working"`
		PendingPrompts int    `json:"pendingPrompts"`
		LastSavedRowID string `json:"lastSavedRowId"`
	} `json:"pane"`
}

func (s *server) feedbackAvailable(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.feedback == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "feedback_unavailable", "Feedback store unavailable")
		return false
	}
	return true
}

// handleFeedbackCreate saves the operator's note with the evidence of the
// moment. The browser uploads its diagnostic ring first, so the snapshot taken
// here from the diagnostics store holds that tab's retained events.
func (s *server) handleFeedbackCreate(w http.ResponseWriter, r *http.Request) {
	if !s.feedbackAvailable(w) {
		return
	}
	var request feedbackCreateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFeedbackRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_feedback", "Request body must be a JSON feedback note: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_feedback", "Request body must contain one JSON object")
		return
	}
	request.Note = strings.TrimSpace(request.Note)
	clientActionID := diagnosticToken(request.ClientActionID, 160)
	thread := feedback.Thread{
		HostedThreadID: diagnosticToken(request.HostedThreadID, 160),
		NativeThreadID: diagnosticToken(request.ThreadID, 160),
		Provider:       diagnosticToken(request.Provider, 32),
	}
	switch {
	case request.Note == "":
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_feedback", "note is required")
		return
	case len(request.Note) > maxFeedbackNoteBytes || !utf8.ValidString(request.Note):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_feedback", fmt.Sprintf("note must be valid UTF-8 of at most %d bytes", maxFeedbackNoteBytes))
		return
	case clientActionID == "":
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_feedback", "clientActionId is required")
		return
	case thread.HostedThreadID == "" && thread.NativeThreadID == "":
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_feedback", "hostedThreadId or threadId is required")
		return
	}
	// A restart after the browser's upload emptied the ring this note would
	// copy; saving now would keep a note without its evidence.
	if s.browserDiagnostics == nil || request.BootID != s.browserDiagnostics.BootID() {
		httpapi.WriteError(w, http.StatusConflict, "evidence_lost", "Nano restarted after the browser logs were sent; save again")
		return
	}
	note := feedback.Note{
		Text: request.Note, Thread: thread,
		Build: feedback.Build{Stamp: buildStamp, Bundle: embeddedBundleName(), TabBundle: diagnosticToken(request.Build, 100)},
		Pane: feedback.Pane{
			Working: request.Pane.Working, PendingPrompts: max(request.Pane.PendingPrompts, 0),
			LastSavedRowID: diagnosticToken(request.Pane.LastSavedRowID, 160),
		},
		Browser: feedback.Browser{
			TabID: diagnosticToken(request.TabID, 100), PageID: diagnosticToken(request.PageID, 100),
			PaneID: diagnosticToken(request.PaneID, 100),
		},
	}
	switch request.View {
	case "enhanced", "dev":
		note.View = request.View
	}
	// A retry may carry a newer pane state; it is still the same note.
	hash := sha256.Sum256([]byte(strings.Join([]string{note.Text, thread.HostedThreadID, thread.NativeThreadID}, "\x00")))

	ref := thread.HostedThreadID
	if ref == "" {
		ref = thread.NativeThreadID
	}
	if identity, err := s.resolveIdentity(r.Context(), ref); err == nil && identity.MatchCount == 1 {
		note.Thread = feedback.Thread{HostedThreadID: identity.HostedThreadID, NativeThreadID: identity.NativeThreadID,
			DisplayName: identity.DisplayName, Provider: identity.Provider}
	}
	if note.Browser.TabID != "" {
		events := []browserDiagnosticEvent{}
		for _, event := range s.browserDiagnostics.recent(maxFeedbackEvidenceEvents, "", note.Browser.TabID, "") {
			ownThread := (event.ThreadID == "" || event.ThreadID == note.Thread.NativeThreadID || event.ThreadID == thread.NativeThreadID) &&
				(event.HostedThreadID == "" || event.HostedThreadID == note.Thread.HostedThreadID)
			if ownThread {
				events = append(events, event)
			}
		}
		// A Save replays events Nano had evicted, which land after newer ones.
		sort.SliceStable(events, func(i, j int) bool { return events[i].ClientSeq < events[j].ClientSeq })
		raw, err := json.Marshal(events)
		if err != nil {
			httpapi.WriteError(w, http.StatusInternalServerError, "feedback_error", "Feedback evidence could not be encoded")
			return
		}
		note.Events, note.EventCount = raw, len(events)
	}
	note.ID, note.CreatedAt = feedback.NewID(), time.Now().UTC().Format(time.RFC3339Nano)

	stored, created, err := s.feedback.Create(note, clientActionID, hex.EncodeToString(hash[:]))
	if errors.Is(err, feedback.ErrClientActionReused) {
		httpapi.WriteError(w, http.StatusConflict, "client_action_reused", err.Error())
		return
	}
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "feedback_error", "Feedback write failed")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	stored.Events = nil
	writeCatalogJSON(w, status, stored)
}

func (s *server) handleFeedbackList(w http.ResponseWriter, r *http.Request) {
	if !s.feedbackAvailable(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	notes, err := s.feedback.List(strings.TrimSpace(r.URL.Query().Get("threadId")), limit)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "feedback_error", "Feedback read failed")
		return
	}
	writeCatalogJSON(w, http.StatusOK, map[string]any{"items": notes})
}

func (s *server) handleFeedbackRead(w http.ResponseWriter, r *http.Request) {
	if !s.feedbackAvailable(w) {
		return
	}
	note, err := s.feedback.Get(r.PathValue("feedbackId"))
	if errors.Is(err, feedback.ErrNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "feedback_error", "Feedback read failed")
		return
	}
	writeCatalogJSON(w, http.StatusOK, note)
}
