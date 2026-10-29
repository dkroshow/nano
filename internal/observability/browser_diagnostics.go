package observability

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/httpapi"
)

const (
	defaultBrowserDiagnosticCapacity = diagnostics.DefaultCapacity
	maxBrowserDiagnosticBatch        = 200
	maxBrowserDiagnosticBodyBytes    = 256 << 10
)

func (s *BrowserHandlers) HandleBrowserDiagnosticsRecord(w http.ResponseWriter, r *http.Request) {
	if s.browserDiagnostics == nil {
		httpapi.WriteHTTPError(w, "browser diagnostics unavailable", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBrowserDiagnosticBodyBytes))
	var body struct {
		Events []diagnostics.Event `json:"events"`
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
		"accepted": s.browserDiagnostics.Add(body.Events),
		"bootId":   s.browserDiagnostics.BootID(),
	})
}

func (s *BrowserHandlers) HandleBrowserDiagnosticsList(w http.ResponseWriter, r *http.Request) {
	if s.browserDiagnostics == nil {
		httpapi.WriteHTTPError(w, "browser diagnostics unavailable", http.StatusServiceUnavailable)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	httpapi.WriteJSON(w, map[string]any{
		"bootId": s.browserDiagnostics.BootID(),
		"events": s.browserDiagnostics.Recent(
			limit,
			r.URL.Query().Get("threadId"),
			r.URL.Query().Get("tabId"),
			r.URL.Query().Get("kind"),
		),
	})
}
