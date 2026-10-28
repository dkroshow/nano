package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/dkroshow/nano/internal/httpapi"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

var nativeThreadReferencePattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hostedThreadReferencePattern = regexp.MustCompile(`^nht_[A-Za-z0-9_-]+$`)
var catalogThreadReferencePattern = regexp.MustCompile(`^nth_[A-Za-z0-9_-]+$`)

type threadReferenceTarget struct {
	Route           string `json:"route"`
	HostedThreadID  string `json:"hostedThreadId,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	CatalogThreadID string `json:"catalogThreadId,omitempty"`
	Provider        string `json:"provider,omitempty"`
	NativeThreadID  string `json:"nativeThreadId,omitempty"`
}

// threadReferenceResponse is intentionally a result envelope, not an HTTP
// error for absence. A browser can distinguish a scanner that has not indexed
// a real live thread yet from a reference Nano has never seen, without trying
// a prefix or a "closest" identifier.
type threadReferenceResponse struct {
	Reference string                 `json:"reference"`
	State     string                 `json:"state"`            // resolved | unresolved
	Reason    string                 `json:"reason,omitempty"` // not_indexed | unknown | ambiguous
	Target    *threadReferenceTarget `json:"target,omitempty"`
}

func (s *server) handleThreadReference(w http.ResponseWriter, r *http.Request) {
	// Resolution combines live registry state with durable catalog/control
	// metadata. Neither result is safe to cache across those changes.
	w.Header().Set("Cache-Control", "no-store")
	raw := r.PathValue("reference")
	if raw == "" || strings.Contains(raw, "/") {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_reference", "Thread reference must be an exact native UUID, hosted nht id, or catalog nth id")
		return
	}
	reference, err := url.PathUnescape(raw)
	if err != nil || !validThreadReference(reference) {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_reference", "Thread reference must be an exact native UUID, hosted nht id, or catalog nth id")
		return
	}
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	response, err := s.resolveThreadReferenceTarget(r.Context(), reference)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	writeCatalogJSON(w, http.StatusOK, response)
}

func validThreadReference(reference string) bool {
	return hostedThreadReferencePattern.MatchString(reference) || nativeThreadReferencePattern.MatchString(reference) || catalogThreadReferencePattern.MatchString(reference)
}

func (s *server) resolveThreadReferenceTarget(ctx context.Context, reference string) (threadReferenceResponse, error) {
	if hostedThreadReferencePattern.MatchString(reference) {
		return s.resolveHostedThreadReference(ctx, reference)
	}
	if catalogThreadReferencePattern.MatchString(reference) {
		return s.resolveCatalogThreadReference(ctx, reference)
	}
	return s.resolveNativeThreadReference(ctx, reference)
}

// resolveCatalogThreadReference keeps older exact catalog deep links working.
// Catalog ids are already Nano-owned identities, so a direct detail lookup is
// both exact and more reliable than trying to reconstruct a native lineage.
func (s *server) resolveCatalogThreadReference(ctx context.Context, catalogID string) (threadReferenceResponse, error) {
	row, err := s.catalog.LoadThread(ctx, catalogID)
	if errors.Is(err, errCatalogThreadNotFound) {
		return threadReferenceResponse{Reference: catalogID, State: "unresolved", Reason: "unknown"}, nil
	}
	if err != nil {
		return threadReferenceResponse{}, err
	}
	if row.HostedThreadID != "" {
		if live, ok := s.liveHostedSession(row.HostedThreadID); ok {
			return resolvedThreadReference(catalogID, threadReferenceTarget{
				Route: "live", HostedThreadID: row.HostedThreadID, SessionID: live.ID,
				Provider: live.Provider, NativeThreadID: live.ThreadID,
			}), nil
		}
	}
	return resolvedThreadReference(catalogID, historicalReferenceTarget(row, row.HostedThreadID)), nil
}

func (s *server) resolveHostedThreadReference(ctx context.Context, hostedID string) (threadReferenceResponse, error) {
	response := threadReferenceResponse{Reference: hostedID, State: "unresolved", Reason: "unknown"}
	if live, ok := s.liveHostedSession(hostedID); ok {
		return resolvedThreadReference(hostedID, threadReferenceTarget{
			Route: "live", HostedThreadID: hostedID, SessionID: live.ID,
			Provider: live.Provider, NativeThreadID: live.ThreadID,
		}), nil
	}
	if s.agentExecutions == nil {
		return response, nil
	}
	record, ok, err := s.agentExecutions.sessionFromCanonical(hostedID)
	if err != nil {
		return response, err
	}
	if !ok {
		return response, nil
	}
	rows, err := s.catalogRowsForNative(ctx, record.NativeThreadID, record.Provider)
	if err != nil {
		return response, err
	}
	if len(rows) == 1 {
		return resolvedThreadReference(hostedID, historicalReferenceTarget(rows[0], hostedID)), nil
	}
	if len(rows) > 1 {
		return threadReferenceResponse{Reference: hostedID, State: "unresolved", Reason: "ambiguous"}, nil
	}
	return threadReferenceResponse{Reference: hostedID, State: "unresolved", Reason: "not_indexed"}, nil
}

func (s *server) resolveNativeThreadReference(ctx context.Context, nativeID string) (threadReferenceResponse, error) {
	response := threadReferenceResponse{Reference: nativeID, State: "unresolved", Reason: "unknown"}
	// Native ids are provider-owned UUIDs. First find the exact provider key,
	// then consult hosted history within that scope. A collision across providers
	// is ambiguous, and retired rows deliberately remain candidates.
	if s.agentExecutions != nil {
		providers, err := s.agentExecutions.nativeHistoryProviders(nativeID)
		if err != nil {
			return response, err
		}
		if len(providers) > 1 {
			return threadReferenceResponse{Reference: nativeID, State: "unresolved", Reason: "ambiguous"}, nil
		}
		matches, err := s.nativeCurrentRecords(nativeID)
		if err != nil {
			return response, err
		}
		if len(matches) > 1 {
			return threadReferenceResponse{Reference: nativeID, State: "unresolved", Reason: "ambiguous"}, nil
		}
		if len(matches) == 1 {
			record := matches[0]
			retiredRecoveryAlias := false
			if control := s.controlStore(); control != nil {
				retiredRecoveryAlias, err = control.retiredNativeRecoveryAlias(ctx, record.ID, nativeID)
				if err != nil {
					return response, err
				}
			}
			if live, ok := s.liveHostedSession(record.ID); ok && !retiredRecoveryAlias {
				return resolvedThreadReference(nativeID, threadReferenceTarget{
					Route: "live", HostedThreadID: record.ID, SessionID: live.ID,
					Provider: live.Provider, NativeThreadID: live.ThreadID,
				}), nil
			}
			rows, err := s.catalogRowsForNative(ctx, nativeID, record.Provider)
			if err != nil {
				return response, err
			}
			if len(rows) >= 1 {
				return resolvedThreadReference(nativeID, historicalReferenceTarget(rows[0], record.ID)), nil
			}
			return resolvedThreadReference(nativeID, threadReferenceTarget{
				Route: "historical", HostedThreadID: record.ID,
				Provider: record.Provider, NativeThreadID: nativeID,
			}), nil
		}
	}
	rows, err := s.catalogRowsForNative(ctx, nativeID, "")
	if err != nil {
		return response, err
	}
	if len(rows) == 1 {
		return resolvedThreadReference(nativeID, historicalReferenceTarget(rows[0], "")), nil
	}
	if len(rows) > 1 {
		return threadReferenceResponse{Reference: nativeID, State: "unresolved", Reason: "ambiguous"}, nil
	}
	if s.liveNativeSession(nativeID) != nil || s.catalogIsIndexing(ctx) {
		response.Reason = "not_indexed"
	}
	return response, nil
}

func resolvedThreadReference(reference string, target threadReferenceTarget) threadReferenceResponse {
	return threadReferenceResponse{Reference: reference, State: "resolved", Target: &target}
}

func historicalReferenceTarget(row catalogThread, hostedID string) threadReferenceTarget {
	return threadReferenceTarget{
		Route: "historical", HostedThreadID: hostedID, CatalogThreadID: row.ID,
		Provider: row.Provider, NativeThreadID: row.NativeID,
	}
}

func (s *server) liveHostedSession(hostedID string) (sessionruntime.Summary, bool) {
	return s.LiveHostedSummary(hostedID, s.agentExecutions.SessionNeedsAttention)
}

func (s *server) liveNativeSession(nativeID string) *sessionruntime.Summary {
	for _, summary := range s.volatileSessionSummaries() {
		if summary.ThreadID == nativeID {
			return &summary
		}
	}
	return nil
}

func (s *server) catalogRowsForNative(ctx context.Context, nativeID, provider string) ([]catalogThread, error) {
	page, err := s.catalog.ListThreads(ctx, catalogListQuery{
		ThreadID: nativeID, IncludeSubagents: true, IncludeGuardians: true,
		IncludeProgrammatic: true, Archived: archivedIncludeFilter, Limit: maxThreadCatalogLimit,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]catalogThread, 0, len(page.Items))
	for _, row := range page.Items {
		if row.NativeID == nativeID && (provider == "" || row.Provider == provider) {
			rows = append(rows, row)
		}
	}
	s.composeCatalogThreads(ctx, rows, s.sidebarRuntimeSnapshot(), false)
	return rows, nil
}

func (s *server) catalogIsIndexing(ctx context.Context) bool {
	status, err := s.catalog.IndexStatus(ctx)
	if err != nil {
		return false
	}
	return status.State == "starting" || status.State == "scanning"
}
