package hosted

import (
	"net/http"

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
	for _, id := range s.Sessions.HostedIDs() {
		attached[id] = true
	}
	records := s.Executions.listSessions()
	items := make([]SessionView, 0, len(records))
	for _, record := range records {
		if waiting := s.Waiting(record.ID); record.Status == StatusExited && !attached[record.ID] && waiting == "" {
			continue
		}
		view := s.SessionView(record.ID)
		if view.Status != StatusExited {
			items = append(items, view)
		}
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}
