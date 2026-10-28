package threadindex

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
)

// HandleThreadTranscriptBody serves the saved catalog thread transcript body endpoint.
func (s *Handlers) HandleThreadTranscriptBody(w http.ResponseWriter, r *http.Request, rawThreadID, rawItemID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(transcriptBodyCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	threadID, itemID := rawThreadID, rawItemID
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_body_id", "Invalid transcript body path")
		return
	}
	response, err := catalog.LoadTranscriptBody(r.Context(), threadID, itemID)
	if err != nil {
		WriteTranscriptBackendError(w, err)
		return
	}
	body, err := json.Marshal(response)
	if err != nil || len(body) > maxThreadRetainedBodyBytes {
		httpapi.WriteError(w, http.StatusInternalServerError, "body_error", "Retained transcript body request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// HandleThreadTranscriptEvidence serves the saved catalog thread transcript evidence endpoint.
func (s *Handlers) HandleThreadTranscriptEvidence(w http.ResponseWriter, r *http.Request, rawThreadID, rawItemID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(transcriptEvidenceCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	threadID, itemID := rawThreadID, rawItemID
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_evidence_id", "Invalid transcript evidence path")
		return
	}
	response, err := catalog.LoadTranscriptEvidence(r.Context(), threadID, itemID)
	if err != nil {
		WriteTranscriptBackendError(w, err)
		return
	}
	body, err := json.Marshal(response)
	if err != nil || response.RawBytes > maxThreadEvidenceBytes {
		httpapi.WriteError(w, http.StatusInternalServerError, "evidence_error", "Transcript evidence request failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// HandleThreadUsage serves the saved catalog thread usage endpoint.
func (s *Handlers) HandleThreadUsage(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(usageCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference := rawID
	if strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	afterOrdinal := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("afterOrdinal")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "afterOrdinal must be a non-negative integer")
			return
		}
		afterOrdinal = value
	}
	limit := history.DefaultThreadUsageTurnLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer")
			return
		}
		limit = value
	}
	resolver, _ := s.catalog.(ReferenceResolver)
	id, err := ResolveReference(r.Context(), resolver, reference)
	if err != nil {
		WriteThreadReferenceError(w, err)
		return
	}
	page, err := catalog.LoadThreadUsage(r.Context(), id, afterOrdinal, limit)
	if err != nil {
		WriteCatalogBackendError(w, err)
		return
	}
	if page.Turns == nil {
		page.Turns = []history.ThreadUsageTurn{}
	}
	WriteCatalogJSON(w, http.StatusOK, page)
}

// HandleThreadUsageList serves the saved catalog thread usage list endpoint.
func (s *Handlers) HandleThreadUsageList(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.catalog.(usageCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	query := history.ThreadUsageListQuery{Archived: history.ArchivedExclude}
	if provider := strings.TrimSpace(r.URL.Query().Get("provider")); provider != "" {
		query.Provider = history.Provider(provider)
		if query.Provider != history.ProviderClaude && query.Provider != history.ProviderCodex && query.Provider != history.ProviderGrok {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "provider must be claude, codex, or grok")
			return
		}
	}
	if kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind != "" {
		query.Kind = history.ThreadKind(kind)
	}
	switch archived := strings.TrimSpace(r.URL.Query().Get("archived")); archived {
	case "", "exclude":
		query.Archived = history.ArchivedExclude
	case "include":
		query.Archived = history.ArchivedInclude
	case "only":
		query.Archived = history.ArchivedOnly
	default:
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "archived must be include, exclude, or only")
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer")
			return
		}
		query.Limit = value
	}
	query.AfterID = strings.TrimSpace(r.URL.Query().Get("cursor"))
	page, err := catalog.ListThreadUsageTotals(r.Context(), query)
	if err != nil {
		WriteCatalogBackendError(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []history.ThreadUsageTotals{}
	}
	WriteCatalogJSON(w, http.StatusOK, page)
}

// HandleThreadTranscript serves the saved catalog thread transcript endpoint.
func (s *Handlers) HandleThreadTranscript(w http.ResponseWriter, r *http.Request, rawID string) {
	// Error responses must never become reusable transcript representations.
	// The successful path replaces this with its private revalidation policy.
	w.Header().Set("Cache-Control", "private, no-store")
	catalog, ok := s.catalog.(TranscriptCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference := rawID
	if strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	query, err := ParseTranscriptListQuery(r)
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	resolver, _ := s.catalog.(ReferenceResolver)
	id, err := ResolveReference(r.Context(), resolver, reference)
	if err != nil {
		WriteThreadReferenceError(w, err)
		return
	}
	response, err := catalog.LoadTranscript(r.Context(), id, query)
	if err != nil {
		WriteTranscriptBackendError(w, err)
		return
	}
	if response.Items == nil {
		response.Items = []TranscriptItem{}
	}
	if response.Turns == nil {
		response.Turns = []TranscriptTurn{}
	}
	body, err := json.Marshal(response)
	if err != nil || len(body) > MaxThreadTranscriptBytes {
		httpapi.WriteError(w, http.StatusInternalServerError, "transcript_error", "Transcript request failed")
		return
	}
	etag := transcriptETag(body)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("ETag", etag)
	w.Header().Set("Vary", "Accept-Encoding")
	if requestETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Handlers) threadNamerAndID(w http.ResponseWriter, r *http.Request) (Namer, string, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	namer, ok := s.catalog.(Namer)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return nil, "", false
	}
	id := strings.TrimSpace(r.PathValue("threadId"))
	if id == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return nil, "", false
	}
	return namer, id, true
}

// Thread names write only the truth file; the catalog worker reconciles the
// derived copy asynchronously with a names-only refresh.
func (s *Handlers) HandleThreadNameSet(w http.ResponseWriter, r *http.Request) {
	namer, id, ok := s.threadNamerAndID(w, r)
	if !ok {
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	body := http.MaxBytesReader(w, r.Body, MaxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", `Request body must be {"name": "..."}`)
		return
	}
	name, err := namer.RenameThread(r.Context(), id, payload.Name)
	if err != nil {
		WriteThreadNameError(w, err)
		return
	}
	WriteCatalogJSON(w, http.StatusOK, map[string]ThreadName{"name": name})
}

// HandleThreadNameClear serves the saved catalog thread name clear endpoint.
func (s *Handlers) HandleThreadNameClear(w http.ResponseWriter, r *http.Request) {
	namer, id, ok := s.threadNamerAndID(w, r)
	if !ok {
		return
	}
	if err := namer.ClearThreadName(r.Context(), id); err != nil {
		WriteThreadNameError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleCatalogIndexStatus serves the saved catalog catalog index status endpoint.
func (s *Handlers) HandleCatalogIndexStatus(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	status, err := s.catalog.IndexStatus(r.Context())
	if err != nil {
		WriteCatalogBackendError(w, err)
		return
	}
	if status.Roots == nil {
		status.Roots = []RootStatus{}
	}
	WriteCatalogJSON(w, http.StatusOK, status)
}

// HandleCatalogRefresh is Explorer's Refresh: the only request that walks the
// provider roots (a full recovery walk).
func (s *Handlers) HandleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	WriteCatalogJSON(w, http.StatusAccepted, map[string]bool{"accepted": s.catalog.RequestRefresh()})
}

// Index reports catalog availability and refresh status to the read handlers.
type Index interface {
	IndexStatus(context.Context) (IndexStatus, error)
	RequestRefresh() bool
}

// Handlers serves saved catalog reads. Live, hosted and sidebar joins stay at the application edge.
type Handlers struct{ catalog Index }

// NewHandlers constructs the read API over a catalog service.
func NewHandlers(catalog Index) *Handlers { return &Handlers{catalog: catalog} }
