package hosted

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

// CatalogErrors supplies the catalog owner's sentinel errors at the hosted HTTP boundary.
type CatalogErrors struct {
	NotFound      error
	Unavailable   error
	InvalidCursor error
}

func (s CatalogErrors) writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, s.InvalidCursor):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid thread catalog cursor")
	case errors.Is(err, s.NotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, s.Unavailable):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "catalog_error", "Thread catalog request failed")
	}
}

func writeCatalogJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
