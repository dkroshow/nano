package main

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/httpapi"
	"io"
	"net/http"
	"strconv"

	"github.com/dkroshow/nano/internal/diagnostics"
)

const (
	defaultBrowserDiagnosticCapacity = diagnostics.DefaultCapacity
	maxBrowserDiagnosticBatch        = 200
	maxBrowserDiagnosticBodyBytes    = 256 << 10
)

type browserDiagnosticEvent = diagnostics.Event

type browserDiagnosticStore struct{ *diagnostics.Store }

func newBrowserDiagnosticStore(capacity int) *browserDiagnosticStore {
	return &browserDiagnosticStore{Store: diagnostics.NewStore(capacity, newSessionID())}
}

func (store *browserDiagnosticStore) add(events []browserDiagnosticEvent) int {
	return store.Add(events)
}

func (store *browserDiagnosticStore) recent(limit int, threadID, tabID, kind string) []browserDiagnosticEvent {
	return store.Recent(limit, threadID, tabID, kind)
}

func normalizeBrowserDiagnosticEvent(event *browserDiagnosticEvent) bool {
	return diagnostics.NormalizeEvent(event)
}

func diagnosticToken(value string, maxLength int) string {
	return diagnostics.Token(value, maxLength)
}

func diagnosticTime(value string) string { return diagnostics.Time(value) }

func (s *server) handleBrowserDiagnosticsRecord(w http.ResponseWriter, r *http.Request) {
	if s.browserDiagnostics == nil {
		httpapi.WriteHTTPError(w, "browser diagnostics unavailable", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBrowserDiagnosticBodyBytes))
	var body struct {
		Events []browserDiagnosticEvent `json:"events"`
	}
	if err := decoder.Decode(&body); err != nil {
		httpapi.WriteHTTPError(w, "invalid browser diagnostic batch", http.StatusBadRequest)
		return
	}
	if len(body.Events) > maxBrowserDiagnosticBatch {
		httpapi.WriteHTTPError(w, "browser diagnostic batch too large", http.StatusRequestEntityTooLarge)
		return
	}
	httpapi.WriteJSON(w, map[string]any{
		"accepted": s.browserDiagnostics.add(body.Events),
		"bootId":   s.browserDiagnostics.BootID(),
	})
}

func (s *server) handleBrowserDiagnosticsList(w http.ResponseWriter, r *http.Request) {
	if s.browserDiagnostics == nil {
		httpapi.WriteHTTPError(w, "browser diagnostics unavailable", http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	httpapi.WriteJSON(w, map[string]any{
		"bootId": s.browserDiagnostics.BootID(),
		"events": s.browserDiagnostics.recent(
			limit,
			r.URL.Query().Get("threadId"),
			r.URL.Query().Get("tabId"),
			r.URL.Query().Get("kind"),
		),
	})
}
