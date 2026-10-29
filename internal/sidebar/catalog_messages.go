package sidebar

import (
	"net/http"
	"net/url"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threadindex"
)

// HandleMessages serves a native or hosted transcript tail.
func (s *CatalogViews) HandleMessages(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.Catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference, err := url.PathUnescape(rawID)
	if err == nil && s.IsHostedReference(reference) {
		// Query errors precede hosted lookup errors, as for native references.
		if _, err := threadindex.ParseMessagesQuery(r); err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		nativeID, ok := s.NativeID(reference)
		if !ok {
			httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Unknown hosted thread")
			return
		}
		if nativeID == "" {
			httpapi.WriteError(w, http.StatusNotFound, "thread_not_indexed", "Hosted thread has no native transcript yet")
			return
		}
		rawID = url.PathEscape(nativeID)
	}
	threadindex.NewHandlers(s.Catalog).HandleThreadMessages(w, r, rawID)
}
