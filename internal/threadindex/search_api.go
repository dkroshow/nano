package threadindex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"

	"github.com/dkroshow/nano/internal/history"
)

type conversationSearchCatalog interface {
	SearchConversationItems(context.Context, history.ConversationSearchQuery) (history.ConversationSearchPage, error)
	ConversationSearchStatus(context.Context) ([]history.ConversationSearchStatus, error)
}

func parseConversationSearchQuery(r *http.Request) (history.ConversationSearchQuery, error) {
	q := r.URL.Query()
	query := history.ConversationSearchQuery{Query: q.Get("q"), Cursor: strings.TrimSpace(q.Get("cursor")), Group: strings.TrimSpace(q.Get("group"))}
	query.Mode = history.ConversationSearchMode(strings.TrimSpace(q.Get("mode")))
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return query, errors.New("limit must be an integer")
		}
		query.Limit = value
	}
	for _, bound := range []struct {
		name   string
		target **time.Time
	}{{"since", &query.Since}, {"until", &query.Until}} {
		if raw := strings.TrimSpace(q.Get(bound.name)); raw != "" {
			value, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return query, fmt.Errorf("%s must be an RFC3339 timestamp", bound.name)
			}
			*bound.target = &value
		}
	}
	for _, raw := range q["role"] {
		query.Roles = append(query.Roles, history.TranscriptRole(strings.TrimSpace(raw)))
	}
	for _, raw := range q["provider"] {
		query.Providers = append(query.Providers, history.Provider(strings.TrimSpace(raw)))
	}
	for _, raw := range q["kind"] {
		query.Kinds = append(query.Kinds, history.ThreadKind(strings.TrimSpace(raw)))
	}
	return history.NormalizeConversationSearchQuery(query)
}

// HandleConversationSearchMessages serves the saved catalog conversation search messages endpoint.
func (s *Handlers) HandleConversationSearchMessages(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.catalog.(conversationSearchCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	query, err := parseConversationSearchQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := catalog.SearchConversationItems(r.Context(), query)
	if err != nil {
		writeConversationSearchError(w, err)
		return
	}
	WriteCatalogJSON(w, http.StatusOK, page)
}

// HandleConversationSearchStatus serves the saved catalog conversation search status endpoint.
func (s *Handlers) HandleConversationSearchStatus(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.catalog.(conversationSearchCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	statuses, err := catalog.ConversationSearchStatus(r.Context())
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "search_error", "Search status request failed")
		return
	}
	if statuses == nil {
		statuses = []history.ConversationSearchStatus{}
	}
	WriteCatalogJSON(w, http.StatusOK, map[string]any{"modes": statuses})
}

func writeConversationSearchError(w http.ResponseWriter, err error) {
	var unavailable *history.ConversationSearchUnavailableError
	switch {
	case errors.As(err, &unavailable):
		httpapi.WriteErrorEnvelope(w, http.StatusServiceUnavailable, httpapi.ErrorEnvelope{Error: httpapi.Error{Code: "search_index_building", Message: "Requested search index is not ready"}, Status: unavailable.Status})
	case errors.Is(err, history.ErrConversationSearchInvalidCursor):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid search cursor")
	case errors.Is(err, history.ErrConversationSearchChanged):
		httpapi.WriteError(w, http.StatusConflict, "search_changed", "Search index changed; restart the query")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "search_error", "Search request failed")
	}
}
