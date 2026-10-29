package threadindex

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

// HandleConversationHistory reads saved history for an attached native identity.
func (s *Handlers) HandleConversationHistory(w http.ResponseWriter, r *http.Request, nativeID string) {
	catalog, ok := s.catalog.(TranscriptCatalog)
	resolver, resolves := s.catalog.(ReferenceResolver)
	if !ok || !resolves || nativeID == "" {
		httpapi.WriteHTTPError(w, "conversation history unavailable", http.StatusServiceUnavailable)
		return
	}
	nanoID, err := resolver.ResolveThreadReference(r.Context(), nativeID)
	if err != nil {
		WriteTranscriptBackendError(w, err)
		return
	}
	query, err := ParseTranscriptListQuery(r)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	response, err := catalog.LoadTranscript(r.Context(), nanoID, query)
	if err != nil {
		WriteTranscriptBackendError(w, err)
		return
	}
	httpapi.WriteJSON(w, response)
}
