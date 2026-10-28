package app

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

const maxIdentityRefBytes = 1024

type identityResolveResponse struct {
	DisplayName               string `json:"displayName"`
	NativeThreadID            string `json:"nativeThreadId"`
	NativeThreadIDProvisional bool   `json:"nativeThreadIdProvisional"`
	HostedThreadID            string `json:"hostedThreadId"`
	Provider                  string `json:"provider"`
	MatchCount                int    `json:"matchCount"`
}

type identityCandidate struct {
	displayName    string
	nativeThreadID string
	hostedThreadID string
	provider       string
	updatedAt      time.Time
}

// threadCatalogNamed is the exact-name lookup used by identity resolve. The
// production catalog queries the derived names table; tests may omit it and
// fall back to the in-memory list fixture.
type threadCatalogNamed interface {
	NamedThreads(ctx context.Context, name string) ([]catalogThread, error)
}

func (s *server) handleIdentityResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_ref", "ref must be a display name, native thread id, or hosted thread id")
		return
	}
	if len(ref) > maxIdentityRefBytes {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_ref", "ref exceeds 1024 bytes")
		return
	}
	response, err := s.resolveIdentity(r.Context(), ref)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	if response.MatchCount == 0 {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "No thread matches that identity")
		return
	}
	writeCatalogJSON(w, http.StatusOK, response)
}

func (s *server) resolveIdentity(ctx context.Context, ref string) (identityResolveResponse, error) {
	if hostedThreadReferencePattern.MatchString(ref) {
		return pickIdentity(s.identityFromHosted(ctx, ref)), nil
	}
	if control := s.controlStore(); control != nil {
		canonical, err := resolveCanonicalNativeIdentity(ctx, control.db, ref)
		if err != nil {
			return identityResolveResponse{}, err
		}
		if canonical.MatchCount == 1 {
			canonical.DisplayName = s.displayNameForHosted(ctx, canonical.Provider, canonical.HostedThreadID, canonical.NativeThreadID)
			if live, ok := s.liveHostedSession(canonical.HostedThreadID); ok && live.Name != "" {
				canonical.DisplayName = live.Name
			}
			return canonical, nil
		}
	}
	native, err := s.identityFromNative(ctx, ref)
	if err != nil {
		return identityResolveResponse{}, err
	}
	if len(native) > 0 {
		return pickIdentity(native), nil
	}
	named, err := s.identityFromName(ctx, ref)
	if err != nil {
		return identityResolveResponse{}, err
	}
	return pickIdentity(named), nil
}

func (s *server) identityFromHosted(ctx context.Context, hostedID string) []identityCandidate {
	if candidate, ok := s.currentHostedIdentity(ctx, hostedID); ok {
		return []identityCandidate{candidate}
	}
	return nil
}

func (s *server) identityFromNative(ctx context.Context, nativeID string) ([]identityCandidate, error) {
	seen := map[string]bool{}
	var candidates []identityCandidate
	add := func(candidate identityCandidate) {
		key := candidate.hostedThreadID
		if key == "" {
			key = candidate.provider + "\x00" + candidate.nativeThreadID
		}
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, candidate)
	}
	if live := s.liveNativeSession(nativeID); live != nil {
		if candidate, found := s.currentHostedIdentity(ctx, live.HostedThreadID); found {
			add(candidate)
		} else {
			add(identityFromLive(*live))
		}
	}
	records, err := s.nativeCurrentRecords(nativeID)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if candidate, ok := s.currentHostedIdentity(ctx, record.ID); ok {
			add(candidate)
		}
	}
	if len(records) == 1 {
		return candidates, nil
	}
	if s.catalog == nil {
		return candidates, nil
	}
	rows, err := s.catalogRowsForNative(ctx, nativeID, "")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.HostedThreadID != "" {
			if candidate, ok := s.currentHostedIdentity(ctx, row.HostedThreadID); ok {
				add(candidate)
				continue
			}
		}
		if record, ok := s.catalogHostedThreadRecord(row); ok {
			if candidate, found := s.currentHostedIdentity(ctx, record.ID); found {
				add(candidate)
				continue
			}
		}
		add(identityFromCatalog(row))
	}
	return candidates, nil
}

func (s *server) identityFromName(ctx context.Context, name string) ([]identityCandidate, error) {
	seen := map[string]bool{}
	var candidates []identityCandidate
	add := func(candidate identityCandidate) {
		key := candidate.hostedThreadID
		if key == "" {
			key = candidate.provider + "\x00" + candidate.nativeThreadID
		}
		if key == "\x00" || seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, candidate)
	}
	for _, live := range s.volatileSessionSummaries() {
		if live.Name != name {
			continue
		}
		if live.HostedThreadID != "" {
			if candidate, ok := s.currentHostedIdentity(ctx, live.HostedThreadID); ok {
				if candidate.displayName == "" {
					candidate.displayName = name
				}
				add(candidate)
				continue
			}
		}
		add(identityFromLive(live))
	}
	rows, err := s.namedCatalogThreads(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.HostedThreadID != "" {
			if candidate, ok := s.currentHostedIdentity(ctx, row.HostedThreadID); ok {
				if candidate.displayName == "" {
					candidate.displayName = name
				}
				add(candidate)
				continue
			}
		}
		if record, ok := s.catalogHostedThreadRecord(row); ok {
			if candidate, found := s.currentHostedIdentity(ctx, record.ID); found {
				if candidate.displayName == "" {
					candidate.displayName = name
				}
				add(candidate)
				continue
			}
		}
		if row.NativeID != "" && s.agentExecutions != nil {
			if record, found := s.agentExecutions.sessionForNative(row.Provider, row.NativeID); found {
				if candidate, ok := s.currentHostedIdentity(ctx, record.ID); ok {
					if candidate.displayName == "" {
						candidate.displayName = name
					}
					add(candidate)
					continue
				}
			}
			providers, err := s.agentExecutions.nativeHistoryProviders(row.NativeID)
			if err != nil {
				return nil, err
			}
			for _, provider := range providers {
				if provider != "" && provider != row.Provider {
					continue
				}
				matches, err := s.agentExecutions.sessionsForNativeHistory(provider, row.NativeID)
				if err != nil {
					return nil, err
				}
				for _, record := range matches {
					if candidate, ok := s.currentHostedIdentity(ctx, record.ID); ok {
						if candidate.displayName == "" {
							candidate.displayName = name
						}
						add(candidate)
					}
				}
			}
		}
		add(identityFromCatalog(row))
	}
	return candidates, nil
}

func (s *server) namedCatalogThreads(ctx context.Context, name string) ([]catalogThread, error) {
	if s.catalog == nil {
		return nil, nil
	}
	if named, ok := s.catalog.(threadCatalogNamed); ok {
		rows, err := named.NamedThreads(ctx, name)
		if err == nil {
			s.composeCatalogThreads(ctx, rows, s.sidebarRuntimeSnapshot(), false)
		}
		return rows, err
	}
	page, err := s.catalog.ListThreads(ctx, catalogListQuery{
		IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true,
		Archived: archivedIncludeFilter, Limit: maxThreadCatalogLimit,
	})
	if err != nil {
		return nil, err
	}
	rows := make([]catalogThread, 0)
	for _, row := range page.Items {
		if row.Name != nil && row.Name.Value == name {
			rows = append(rows, row)
		}
	}
	s.composeCatalogThreads(ctx, rows, s.sidebarRuntimeSnapshot(), false)
	return rows, nil
}

func (s *server) currentHostedIdentity(ctx context.Context, hostedID string) (identityCandidate, bool) {
	if hostedID == "" {
		return identityCandidate{}, false
	}
	if live, ok := s.liveHostedSession(hostedID); ok {
		candidate := identityFromLive(live)
		if candidate.displayName == "" {
			candidate.displayName = s.displayNameForHosted(ctx, live.Provider, hostedID, live.ThreadID)
		}
		if rec, found, err := s.hostedCanonical(hostedID); err == nil && found {
			if candidate.nativeThreadID == "" {
				candidate.nativeThreadID = rec.NativeThreadID
			}
			if recUpdated := threadvalues.ParseTimestamp(rec.UpdatedAt); recUpdated.After(candidate.updatedAt) {
				candidate.updatedAt = recUpdated
			}
			if candidate.provider == "" {
				candidate.provider = rec.Provider
			}
		}
		return candidate, true
	}
	rec, ok, err := s.hostedCanonical(hostedID)
	if err != nil || !ok {
		return identityCandidate{}, false
	}
	native := rec.NativeThreadID
	return identityCandidate{
		displayName:    s.displayNameForHosted(ctx, rec.Provider, rec.ID, native),
		nativeThreadID: native,
		hostedThreadID: rec.ID,
		provider:       rec.Provider,
		updatedAt:      threadvalues.ParseTimestamp(rec.UpdatedAt),
	}, true
}

func (s *server) hostedCanonical(hostedID string) (agentSessionRecord, bool, error) {
	if s.agentExecutions == nil {
		return agentSessionRecord{}, false, nil
	}
	return s.agentExecutions.sessionFromCanonical(hostedID)
}

func (s *server) displayNameForHosted(ctx context.Context, provider, hostedID, currentNative string) string {
	if name, ok := s.durableThreadName(provider, currentNative); ok {
		return name
	}
	if name := s.catalogNameForNative(ctx, provider, currentNative); name != "" {
		return name
	}
	if s.agentExecutions == nil || hostedID == "" {
		return ""
	}
	ids, err := s.agentExecutions.nativeIDsForHosted(hostedID)
	if err != nil {
		return ""
	}
	for _, nativeID := range ids {
		if nativeID == "" || nativeID == currentNative {
			continue
		}
		if name, ok := s.durableThreadName(provider, nativeID); ok {
			return name
		}
		if name := s.catalogNameForNative(ctx, provider, nativeID); name != "" {
			return name
		}
	}
	if s.controlMetadata != nil {
		if name, ok := s.controlMetadata.creationName(hostedID); ok {
			return name
		}
	}
	return ""
}

func (s *server) catalogNameForNative(ctx context.Context, provider, nativeID string) string {
	if s.catalog == nil || nativeID == "" {
		return ""
	}
	rows, err := s.catalogRowsForNative(ctx, nativeID, provider)
	if err != nil {
		return ""
	}
	for _, row := range rows {
		if row.Name != nil && row.Name.Value != "" {
			return row.Name.Value
		}
	}
	return ""
}

func identityFromLive(live sessions.Summary) identityCandidate {
	updated := threadvalues.ParseTimestamp(live.LastTurnEndedAt)
	if updated.IsZero() {
		updated = threadvalues.ParseTimestamp(live.CreatedAt)
	}
	return identityCandidate{
		displayName:    live.Name,
		nativeThreadID: live.ThreadID,
		hostedThreadID: live.HostedThreadID,
		provider:       live.Provider,
		updatedAt:      updated,
	}
}

func identityFromCatalog(row catalogThread) identityCandidate {
	name := ""
	if row.Name != nil {
		name = row.Name.Value
	}
	updated := ""
	if row.UpdatedAt != nil {
		updated = *row.UpdatedAt
	}
	return identityCandidate{
		displayName:    name,
		nativeThreadID: row.NativeID,
		hostedThreadID: row.HostedThreadID,
		provider:       row.Provider,
		updatedAt:      threadvalues.ParseTimestamp(updated),
	}
}

func pickIdentity(candidates []identityCandidate) identityResolveResponse {
	if len(candidates) == 0 {
		return identityResolveResponse{}
	}
	byKey := map[string]identityCandidate{}
	for _, candidate := range candidates {
		key := identityKey(candidate)
		if existing, ok := byKey[key]; ok {
			byKey[key] = mergeIdentity(existing, candidate)
			continue
		}
		byKey[key] = candidate
	}
	var best identityCandidate
	for _, candidate := range byKey {
		if best.hostedThreadID == "" && best.nativeThreadID == "" && best.provider == "" {
			best = candidate
			continue
		}
		if identityNewer(candidate, best) {
			best = candidate
		}
	}
	return identityResolveResponse{
		DisplayName:               best.displayName,
		NativeThreadID:            best.nativeThreadID,
		NativeThreadIDProvisional: sessions.NativeThreadIDProvisional(best.provider, best.nativeThreadID),
		HostedThreadID:            best.hostedThreadID,
		Provider:                  best.provider,
		MatchCount:                len(byKey),
	}
}

func identityKey(candidate identityCandidate) string {
	if candidate.hostedThreadID != "" {
		return "h:" + candidate.hostedThreadID
	}
	return "n:" + candidate.provider + "\x00" + candidate.nativeThreadID
}

func mergeIdentity(a, b identityCandidate) identityCandidate {
	newer, older := a, b
	if identityNewer(b, a) {
		newer, older = b, a
	}
	if newer.displayName == "" {
		newer.displayName = older.displayName
	}
	if newer.nativeThreadID == "" {
		newer.nativeThreadID = older.nativeThreadID
	}
	if newer.provider == "" {
		newer.provider = older.provider
	}
	if newer.hostedThreadID == "" {
		newer.hostedThreadID = older.hostedThreadID
	}
	return newer
}

func identityNewer(a, b identityCandidate) bool {
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	if a.hostedThreadID != b.hostedThreadID {
		return a.hostedThreadID > b.hostedThreadID
	}
	return a.nativeThreadID > b.nativeThreadID
}

// The native-id resolver and retirement verification share this canonical read.
// Accepting a transaction lets maintenance prove the post-delete identity before
// commit. History/name fallback stays with the public resolver; it cannot prove
// that a retired record has a current surviving owner.
func resolveCanonicalNativeIdentity(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, nativeID string) (identityResolveResponse, error) {
	rows, err := reader.QueryContext(ctx, `SELECT h.hosted_thread_id,h.provider,COALESCE(h.current_native_id,''),h.updated_at
 FROM hosted_thread_native_ids n JOIN hosted_threads h ON h.hosted_thread_id=n.hosted_thread_id AND h.provider=n.provider
 WHERE n.native_thread_id=? AND n.retired_at IS NULL
 AND (SELECT count(DISTINCT provider) FROM hosted_thread_native_ids WHERE native_thread_id=?)=1
 ORDER BY h.hosted_thread_id`, nativeID, nativeID)
	if err != nil {
		return identityResolveResponse{}, err
	}
	defer rows.Close()
	var candidates []identityCandidate
	for rows.Next() {
		var candidate identityCandidate
		var updated string
		if err := rows.Scan(&candidate.hostedThreadID, &candidate.provider, &candidate.nativeThreadID, &updated); err != nil {
			return identityResolveResponse{}, err
		}
		candidate.updatedAt = threadvalues.ParseTimestamp(updated)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return identityResolveResponse{}, err
	}
	return pickIdentity(candidates), nil
}
