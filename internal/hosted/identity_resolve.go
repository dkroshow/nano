package hosted

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

const maxIdentityRefBytes = 1024

// IdentityResolution is the selected current identity and the number of exact matches.
type IdentityResolution struct {
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

// HandleIdentityResolve serves an exact hosted, native or display-name lookup.
func (s *IdentityService) HandleIdentityResolve(w http.ResponseWriter, r *http.Request) {
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
	response, err := s.ResolveIdentity(r.Context(), ref)
	if err != nil {
		s.CatalogErrors.writeBackendError(w, err)
		return
	}
	if response.MatchCount == 0 {
		httpapi.WriteError(w, http.StatusNotFound, "not_found", "No thread matches that identity")
		return
	}
	writeCatalogJSON(w, http.StatusOK, response)
}

// ResolveIdentity prefers current canonical ownership before live and catalog identity.
func (s *IdentityService) ResolveIdentity(ctx context.Context, ref string) (IdentityResolution, error) {
	if hostedThreadReferencePattern.MatchString(ref) {
		return pickIdentity(s.identityFromHosted(ctx, ref)), nil
	}
	if control := s.controlStore(); control != nil {
		canonical, err := resolveCanonicalNativeIdentity(ctx, control.CanonicalDatabase(), ref)
		if err != nil {
			return IdentityResolution{}, err
		}
		if canonical.MatchCount == 1 {
			canonical.DisplayName = s.DisplayNameForHosted(ctx, canonical.Provider, canonical.HostedThreadID, canonical.NativeThreadID)
			if live, ok := s.liveHostedSession(canonical.HostedThreadID); ok && live.Name != "" {
				canonical.DisplayName = live.Name
			}
			return canonical, nil
		}
	}
	native, err := s.identityFromNative(ctx, ref)
	if err != nil {
		return IdentityResolution{}, err
	}
	if len(native) > 0 {
		return pickIdentity(native), nil
	}
	named, err := s.identityFromName(ctx, ref)
	if err != nil {
		return IdentityResolution{}, err
	}
	return pickIdentity(named), nil
}

func (s *IdentityService) identityFromHosted(ctx context.Context, hostedID string) []identityCandidate {
	if candidate, ok := s.currentHostedIdentity(ctx, hostedID); ok {
		return []identityCandidate{candidate}
	}
	return nil
}

func (s *IdentityService) identityFromNative(ctx context.Context, nativeID string) ([]identityCandidate, error) {
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
	if s.Catalog == nil {
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

func (s *IdentityService) identityFromName(ctx context.Context, name string) ([]identityCandidate, error) {
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
	for _, live := range s.liveIdentities() {
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
		if row.NativeID != "" && s.Executions != nil {
			if record, found := s.Executions.SessionForNative(row.Provider, row.NativeID); found {
				if candidate, ok := s.currentHostedIdentity(ctx, record.ID); ok {
					if candidate.displayName == "" {
						candidate.displayName = name
					}
					add(candidate)
					continue
				}
			}
			providers, err := s.Executions.NativeHistoryProviders(row.NativeID)
			if err != nil {
				return nil, err
			}
			for _, provider := range providers {
				if provider != "" && provider != row.Provider {
					continue
				}
				matches, err := s.Executions.SessionsForNativeHistory(provider, row.NativeID)
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

func (s *IdentityService) currentHostedIdentity(ctx context.Context, hostedID string) (identityCandidate, bool) {
	if hostedID == "" {
		return identityCandidate{}, false
	}
	if live, ok := s.liveHostedSession(hostedID); ok {
		candidate := identityFromLive(live)
		if candidate.displayName == "" {
			candidate.displayName = s.DisplayNameForHosted(ctx, live.Provider, hostedID, live.ThreadID)
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
		displayName:    s.DisplayNameForHosted(ctx, rec.Provider, rec.ID, native),
		nativeThreadID: native,
		hostedThreadID: rec.ID,
		provider:       rec.Provider,
		updatedAt:      threadvalues.ParseTimestamp(rec.UpdatedAt),
	}, true
}

func (s *IdentityService) hostedCanonical(hostedID string) (ownedthreads.SessionRecord, bool, error) {
	if s.Executions == nil {
		return ownedthreads.SessionRecord{}, false, nil
	}
	return s.Executions.SessionFromCanonical(hostedID)
}

// DisplayNameForHosted reads the current or historical durable name, then the creation name.
func (s *IdentityService) DisplayNameForHosted(ctx context.Context, provider, hostedID, currentNative string) string {
	if name, ok := s.durableThreadName(provider, currentNative); ok {
		return name
	}
	if name := s.catalogNameForNative(ctx, provider, currentNative); name != "" {
		return name
	}
	if s.Executions == nil || hostedID == "" {
		return ""
	}
	ids, err := s.Executions.NativeIDsForHosted(hostedID)
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
	if s.Metadata != nil {
		if name, ok := s.Metadata.CreationName(hostedID); ok {
			return name
		}
	}
	return ""
}

func (s *IdentityService) catalogNameForNative(ctx context.Context, provider, nativeID string) string {
	if s.Catalog == nil || nativeID == "" {
		return ""
	}
	rows, err := s.catalogRowsForNative(ctx, nativeID, provider)
	if err != nil {
		return ""
	}
	for _, row := range rows {
		if row.Name != "" {
			return row.Name
		}
	}
	return ""
}

func identityFromLive(live LiveIdentity) identityCandidate {
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

func identityFromCatalog(row CatalogIdentity) identityCandidate {
	name := row.Name
	updated := row.UpdatedAt
	return identityCandidate{
		displayName:    name,
		nativeThreadID: row.NativeID,
		hostedThreadID: row.HostedThreadID,
		provider:       row.Provider,
		updatedAt:      threadvalues.ParseTimestamp(updated),
	}
}

func pickIdentity(candidates []identityCandidate) IdentityResolution {
	if len(candidates) == 0 {
		return IdentityResolution{}
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
	return IdentityResolution{
		DisplayName:               best.displayName,
		NativeThreadID:            best.nativeThreadID,
		NativeThreadIDProvisional: hostedstore.NativeThreadIDProvisional(best.provider, best.nativeThreadID),
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

// resolveCanonicalNativeIdentity reads unique current ownership within a database transaction.
// The native-id resolver and retirement verification share this canonical read.
// Accepting a transaction lets maintenance prove the post-delete identity before
// commit. History/name fallback stays with the public resolver; it cannot prove
// that a retired record has a current surviving owner.
func resolveCanonicalNativeIdentity(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, nativeID string) (IdentityResolution, error) {
	rows, err := reader.QueryContext(ctx, `SELECT h.hosted_thread_id,h.provider,COALESCE(h.current_native_id,''),h.updated_at
 FROM hosted_thread_native_ids n JOIN hosted_threads h ON h.hosted_thread_id=n.hosted_thread_id AND h.provider=n.provider
 WHERE n.native_thread_id=? AND n.retired_at IS NULL
 AND (SELECT count(DISTINCT provider) FROM hosted_thread_native_ids WHERE native_thread_id=?)=1
 ORDER BY h.hosted_thread_id`, nativeID, nativeID)
	if err != nil {
		return IdentityResolution{}, err
	}
	defer rows.Close()
	var candidates []identityCandidate
	for rows.Next() {
		var candidate identityCandidate
		var updated string
		if err := rows.Scan(&candidate.hostedThreadID, &candidate.provider, &candidate.nativeThreadID, &updated); err != nil {
			return IdentityResolution{}, err
		}
		candidate.updatedAt = threadvalues.ParseTimestamp(updated)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return IdentityResolution{}, err
	}
	return pickIdentity(candidates), nil
}
