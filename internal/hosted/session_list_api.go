package hosted

import (
	"net/http"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
)

// HandleSessionsList serves non-exited hosted sessions with current live overlays.
func (s *ViewService) HandleSessionsList(w http.ResponseWriter, _ *http.Request) {
	if s.Executions == nil {
		message := "execution control unavailable; durable hosted mutations are disabled"
		if s.ExecutionError != "" {
			message += ": " + s.ExecutionError
		}
		httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
		return
	}
	// A view's status is its record's unless a live session is attached or
	// boot is still reattaching the thread (starting), so any other exited
	// record is left out before its view costs any reads; the store keeps
	// every record a thread ever had.
	attached := make(map[string]bool)
	if s.Sessions != nil {
		for _, live := range s.Sessions.SnapshotSessions() {
			attached[live.HostedThreadID()] = true
		}
	}
	records := s.Executions.ListSessions()
	items := make([]SessionView, 0, len(records))
	for _, record := range records {
		if waiting := s.Waiting(record.ID); record.Status == hostedstore.StatusExited && !attached[record.ID] && waiting == "" {
			continue
		}
		view := s.SessionView(record.ID)
		if view.Status != hostedstore.StatusExited {
			items = append(items, view)
		}
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}
