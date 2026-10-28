package threadindex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
)

// WriteThreadNameError maps a validated name-write failure to an HTTP error.
func WriteThreadNameError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidThreadName):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", err.Error())
	case errors.Is(err, history.ErrThreadNamesMalformed):
		httpapi.WriteError(w, http.StatusConflict, "names_file_malformed",
			"Thread names file is malformed; repair it by hand before renaming — nano will not overwrite broken truth")

	case errors.Is(err, ErrCatalogThreadNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, errNamesFileDisabled):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "names_unavailable", "Thread names are not configured")
	case errors.Is(err, context.Canceled):
		httpapi.WriteError(w, clientClosedRequestStatus, "request_cancelled", "Request was cancelled")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "name_error", "Thread name update failed")
	}
}

// ParseTranscriptListQuery validates a saved transcript page query.
func ParseTranscriptListQuery(r *http.Request) (TranscriptQuery, error) {
	q := r.URL.Query()
	query := TranscriptQuery{
		Limit:  defaultThreadTranscriptLimit,
		Before: strings.TrimSpace(q.Get("before")),
		View:   strings.TrimSpace(q.Get("view")),
	}
	if query.View == "" {
		query.View = DefaultThreadTranscriptView
	}
	if query.View == legacyThreadTranscriptView {
		query.View = DefaultThreadTranscriptView
	}
	if query.View != DefaultThreadTranscriptView && query.View != DevThreadTranscriptView {
		return query, errors.New("view must be enhanced or dev")
	}
	var hiddenSpecified bool
	if _, ok := q["hide"]; ok {
		hiddenSpecified = true
		query.HiddenSpecified = true
		values := make([]string, 0)
		for _, raw := range q["hide"] {
			for _, value := range strings.Split(raw, ",") {
				value = strings.TrimSpace(value)
				if value != "" {
					values = append(values, value)
				}
			}
		}
		if len(values) == 1 && values[0] == "none" {
			values = nil
		} else {
			for _, value := range values {
				if value == "none" {
					return query, errors.New("hide=none cannot be combined with categories")
				}
			}
		}
		query.HiddenCategories = values
	}
	if !hiddenSpecified && query.View == DefaultThreadTranscriptView {
		query.HiddenCategories = append([]string(nil), enhancedHiddenCategories...)
	}
	canonical, err := canonicalTranscriptCategories(query.HiddenCategories)
	if err != nil {
		return query, err
	}
	query.HiddenCategories = canonical
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			return query, errors.New("limit must be a positive integer")
		}
		query.Limit = value
	}
	if query.Limit > MaxThreadTranscriptLimit {
		query.Limit = MaxThreadTranscriptLimit
	}
	_, hasBeforeNativeTurn := q["beforeNativeTurn"]
	_, hasBeforeTurn := q["beforeTurn"]
	_, hasTurnLimit := q["turnLimit"]
	if hasBeforeNativeTurn && hasBeforeTurn {
		return query, errors.New("beforeNativeTurn and beforeTurn cannot be combined")
	}
	if (hasBeforeNativeTurn || hasBeforeTurn) != hasTurnLimit {
		return query, errors.New("turnLimit requires exactly one turn boundary")
	}
	if query.Before != "" && hasTurnLimit {
		return query, errors.New("before cursor and turn window cannot be combined")
	}
	if hasBeforeNativeTurn {
		query.BeforeNativeTurn = strings.TrimSpace(q.Get("beforeNativeTurn"))
		if query.BeforeNativeTurn == "" {
			return query, errors.New("beforeNativeTurn must not be empty")
		}
	}
	if hasBeforeTurn {
		value, err := strconv.Atoi(strings.TrimSpace(q.Get("beforeTurn")))
		if err != nil || value <= 0 {
			return query, errors.New("beforeTurn must be a positive integer")
		}
		query.BeforeTurn = value
	}
	if hasTurnLimit {
		value, err := strconv.Atoi(strings.TrimSpace(q.Get("turnLimit")))
		if err != nil || value <= 0 || value > maxThreadTranscriptTurnLimit {
			return query, fmt.Errorf("turnLimit must be an integer from 1 to %d", maxThreadTranscriptTurnLimit)
		}
		query.TurnLimit = value
	}
	return query, nil
}

func canonicalTranscriptCategories(values []string) ([]string, error) {
	order := []string{"messages", "reasoning", "toolCall", "toolResult", "subagent", "system", "error", "unclassified"}
	known := make(map[string]bool, len(order))
	for _, category := range order {
		known[category] = true
	}
	seen := make(map[string]bool, len(values))
	for _, category := range values {
		if !known[category] {
			return nil, fmt.Errorf("unknown transcript category %q", category)
		}
		seen[category] = true
	}
	canonical := make([]string, 0, len(seen))
	for _, category := range order {
		if seen[category] {
			canonical = append(canonical, category)
		}
	}
	return canonical, nil
}

// WriteTranscriptBackendError maps saved transcript failures to HTTP errors.
func WriteTranscriptBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidTranscriptCursor):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid transcript cursor")
	case errors.Is(err, errTranscriptCursorStale):
		httpapi.WriteError(w, http.StatusConflict, "transcript_changed", "Transcript changed; reload the newest page")
	case errors.Is(err, ErrCatalogThreadNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, ErrThreadCatalogUnavailable):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
	case errors.Is(err, errTranscriptEvidenceLarge):
		httpapi.WriteError(w, http.StatusRequestEntityTooLarge, "evidence_too_large", "Native transcript row exceeds the evidence response limit")
	case errors.Is(err, errTranscriptEvidenceUnavailable):
		httpapi.WriteError(w, http.StatusGone, "evidence_unavailable", "Native transcript evidence is no longer available")
	case errors.Is(err, history.ErrTranscriptBodyUnavailable):
		httpapi.WriteError(w, http.StatusGone, "body_unavailable", "Retained transcript body is no longer available")
	case errors.Is(err, context.Canceled):
		httpapi.WriteError(w, clientClosedRequestStatus, "request_cancelled", "Request was cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		httpapi.WriteError(w, http.StatusGatewayTimeout, "request_timeout", "Request timed out")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "transcript_error", "Transcript request failed")
	}
}

func transcriptETag(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("\"%x\"", digest[:16])
}

func requestETagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// ParseCatalogListQuery validates the catalog list query at the HTTP boundary.
func ParseCatalogListQuery(r *http.Request) (ListQuery, error) {
	q := r.URL.Query()
	query := ListQuery{
		Archived: strings.TrimSpace(q.Get("archived")),
		ThreadID: strings.TrimSpace(q.Get("threadId")),
		Cursor:   strings.TrimSpace(q.Get("cursor")),
		Limit:    defaultThreadCatalogLimit,
	}
	if len(query.ThreadID) > maxThreadCatalogIDBytes {
		return query, fmt.Errorf("threadId must be at most %d bytes", maxThreadCatalogIDBytes)
	}
	if query.Archived == "" {
		query.Archived = "include"
	}
	if query.Archived != "include" && query.Archived != "exclude" && query.Archived != "only" {
		return query, errors.New("archived must be include, exclude, or only")
	}
	if raw := strings.TrimSpace(q.Get("includeSubagents")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return query, errors.New("includeSubagents must be true or false")
		}
		query.IncludeSubagents = v
	}
	if raw := strings.TrimSpace(q.Get("includeGuardians")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return query, errors.New("includeGuardians must be true or false")
		}
		query.IncludeGuardians = v
	}
	if raw := strings.TrimSpace(q.Get("includeProgrammatic")); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return query, errors.New("includeProgrammatic must be true or false")
		}
		query.IncludeProgrammatic = v
	}
	timeBounds := []struct {
		param string
		at    **time.Time
	}{
		{"createdFrom", &query.CreatedFrom},
		{"createdTo", &query.CreatedTo},
		{"updatedFrom", &query.UpdatedFrom},
		{"updatedTo", &query.UpdatedTo},
	}
	for _, b := range timeBounds {
		raw := strings.TrimSpace(q.Get(b.param))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return query, fmt.Errorf("%s must be an RFC3339 timestamp", b.param)
		}
		*b.at = &parsed
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			return query, errors.New("limit must be a positive integer")
		}
		query.Limit = v
	}
	if query.Limit > MaxThreadCatalogLimit {
		query.Limit = MaxThreadCatalogLimit
	}
	return query, nil
}

// WriteCatalogBackendError maps saved catalog failures to HTTP errors.
func WriteCatalogBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidThreadCatalogCursor):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "Invalid thread catalog cursor")
	case errors.Is(err, ErrCatalogThreadNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, ErrThreadCatalogUnavailable):
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
	default:
		httpapi.WriteError(w, http.StatusInternalServerError, "catalog_error", "Thread catalog request failed")
	}
}

// WriteCatalogJSON writes a catalog response with private cache semantics.
func WriteCatalogJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "private, no-cache")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteCatalogPageJSON gives the window endpoint a stable validator. Runtime
// and fleet overlays are part of the representation, so the hash changes when
// a visible overlay changes even though catalog_version does not.
func WriteCatalogPageJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "encode_error", "Catalog response failed")
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
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
