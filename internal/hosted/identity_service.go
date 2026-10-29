package hosted

import (
	"context"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// CatalogIdentity contains only the saved identity facts used by hosted resolution.
type CatalogIdentity struct{ ID, Provider, NativeID, HostedThreadID, Name, UpdatedAt string }

// liveIdentity contains the current attachment facts used by hosted resolution.
type liveIdentity struct{ ID, Provider, ThreadID, HostedThreadID, Name, CreatedAt, LastTurnEndedAt string }

// IdentityCatalog supplies catalogue reads after the app composes its overlays.
type IdentityCatalog interface {
	// NativeIdentities reads exact native matches within an optional provider scope.
	NativeIdentities(context.Context, string, string) ([]CatalogIdentity, error)
	// NamedIdentities reads exact display-name matches.
	NamedIdentities(context.Context, string) ([]CatalogIdentity, error)
	// LoadIdentity reads the exact catalog identity.
	LoadIdentity(context.Context, string) (CatalogIdentity, error)
	// IsIndexing reports whether discovery can still produce an unresolved reference.
	IsIndexing(context.Context) bool
	// DurableName reads the authoritative name for a provider-scoped native identity.
	DurableName(string, string) (string, bool)
}

// IdentityService resolves hosted identities using canonical stores and bounded read inputs.
// The caller supplies a snapshot of these dependencies for each request.
type IdentityService struct {
	Executions    *hostedstore.ExecutionStore
	Metadata      *hostedstore.MetadataStore
	MetadataError string
	Catalog       IdentityCatalog
	Sessions      *sessions.Registry
	CatalogErrors CatalogErrors
}

func (s *IdentityService) controlStore() *hostedstore.MetadataStore {
	if s == nil {
		return nil
	}
	if s.Metadata != nil && s.MetadataError == "" {
		return s.Metadata
	}
	if s.Executions != nil {
		return s.Executions.Control()
	}
	return nil
}
func (s *IdentityService) liveIdentities() []liveIdentity {
	if s.Sessions == nil {
		return nil
	}
	needsAttention := map[string]bool{}
	if s.Executions != nil {
		needsAttention = s.Executions.SessionsNeedingAttention()
	}
	summaries := s.Sessions.VolatileSummaries(needsAttention)
	result := make([]liveIdentity, 0, len(summaries))
	for _, summary := range summaries {
		result = append(result, identityFromSession(summary))
	}
	return result
}
func (s *IdentityService) liveHostedSession(id string) (liveIdentity, bool) {
	if s.Sessions == nil {
		return liveIdentity{}, false
	}
	summary, ok := s.Sessions.LiveHostedSummary(id, s.Executions.SessionNeedsAttention)
	return identityFromSession(summary), ok
}
func identityFromSession(summary sessions.Summary) liveIdentity {
	return liveIdentity{ID: summary.ID, Name: summary.Name, Provider: summary.Provider, ThreadID: summary.ThreadID, HostedThreadID: summary.HostedThreadID, CreatedAt: summary.CreatedAt, LastTurnEndedAt: summary.LastTurnEndedAt}
}
func (s *IdentityService) catalogRowsForNative(ctx context.Context, native, provider string) ([]CatalogIdentity, error) {
	return s.Catalog.NativeIdentities(ctx, native, provider)
}
func (s *IdentityService) namedCatalogThreads(ctx context.Context, name string) ([]CatalogIdentity, error) {
	if s.Catalog == nil {
		return nil, nil
	}
	return s.Catalog.NamedIdentities(ctx, name)
}
func (s *IdentityService) catalogIsIndexing(ctx context.Context) bool {
	return s.Catalog.IsIndexing(ctx)
}
func (s *IdentityService) durableThreadName(provider, native string) (string, bool) {
	if s.Catalog == nil {
		return "", false
	}
	return s.Catalog.DurableName(provider, native)
}
func (s *IdentityService) catalogHostedThreadRecord(row CatalogIdentity) (ownedthreads.SessionRecord, bool) {
	if s.Executions == nil {
		return ownedthreads.SessionRecord{}, false
	}
	if row.HostedThreadID != "" {
		if record, ok := s.Executions.Session(row.HostedThreadID); ok {
			return record, true
		}
	}
	return s.Executions.SessionForNative(row.Provider, row.NativeID)
}
func (s *IdentityService) nativeCurrentRecords(nativeID string) ([]ownedthreads.SessionRecord, error) {
	if s == nil || s.Executions == nil || nativeID == "" {
		return nil, nil
	}
	providers, err := s.Executions.NativeHistoryProviders(nativeID)
	if err != nil {
		return nil, err
	}
	if len(providers) > 1 {
		var all []ownedthreads.SessionRecord
		for _, provider := range providers {
			matches, err := s.Executions.SessionsForNativeHistory(provider, nativeID)
			if err != nil {
				return nil, err
			}
			all = append(all, matches...)
		}
		return all, nil
	}
	if len(providers) == 0 {
		return nil, nil
	}
	provider := providers[0]
	if control := s.controlStore(); control != nil {
		owner, err := control.ActiveNativeOwner(provider, nativeID)
		if err != nil {
			return nil, err
		}
		if owner != "" {
			record, ok, err := s.Executions.SessionFromCanonical(owner)
			if err != nil {
				return nil, err
			}
			if ok {
				return []ownedthreads.SessionRecord{record}, nil
			}
			return []ownedthreads.SessionRecord{{ID: owner, Provider: provider, NativeThreadID: nativeID}}, nil
		}
	}
	return s.Executions.SessionsForNativeHistory(provider, nativeID)
}
