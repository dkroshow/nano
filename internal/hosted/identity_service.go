package hosted

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// catalogIdentity contains only the saved identity facts used by hosted resolution.
type catalogIdentity struct{ ID, Provider, NativeID, HostedThreadID, Name, UpdatedAt string }

// liveIdentity contains the current attachment facts used by hosted resolution.
type liveIdentity struct{ ID, Provider, ThreadID, HostedThreadID, Name, CreatedAt, LastTurnEndedAt string }

// IdentityService resolves hosted identities through their concrete owners:
// canonical hosted stores, saved catalog rows and the live session registry.
type IdentityService struct {
	Executions    *hostedstore.ExecutionStore
	Metadata      *hostedstore.MetadataStore
	MetadataError string
	Catalog       *threadindex.Service
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
func (s *IdentityService) catalogRowsForNative(ctx context.Context, native, provider string) ([]catalogIdentity, error) {
	page, err := s.Catalog.ListThreads(ctx, threadindex.ListQuery{ThreadID: native, IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: string(history.ArchivedInclude), Limit: threadindex.MaxThreadCatalogLimit})
	if err != nil {
		return nil, err
	}
	rows := make([]catalogIdentity, 0, len(page.Items))
	for _, row := range page.Items {
		if row.NativeID == native && (provider == "" || row.Provider == provider) {
			rows = append(rows, s.catalogIdentity(row))
		}
	}
	return rows, nil
}
func (s *IdentityService) namedCatalogThreads(ctx context.Context, name string) ([]catalogIdentity, error) {
	if s.Catalog == nil {
		return nil, nil
	}
	saved, err := s.Catalog.NamedThreads(ctx, name)
	rows := make([]catalogIdentity, 0, len(saved))
	for _, row := range saved {
		rows = append(rows, s.catalogIdentity(row))
	}
	return rows, err
}
func (s *IdentityService) catalogIsIndexing(ctx context.Context) bool {
	status, err := s.Catalog.IndexStatus(ctx)
	return err == nil && (status.State == "starting" || status.State == "scanning")
}
func (s *IdentityService) durableThreadName(provider, native string) (string, bool) {
	if s.Catalog == nil || native == "" {
		return "", false
	}
	return s.Catalog.ThreadNameFor(context.Background(), provider, native)
}
func (s *IdentityService) catalogIdentity(row threadindex.Thread) catalogIdentity {
	result := catalogIdentity{ID: row.ID, Provider: row.Provider, NativeID: row.NativeID}
	if s.Executions != nil {
		if record, ok := s.Executions.SessionForNative(row.Provider, row.NativeID); ok {
			result.HostedThreadID = record.ID
		}
	}
	if row.Name != nil {
		result.Name = row.Name.Value
	}
	if row.UpdatedAt != nil {
		result.UpdatedAt = *row.UpdatedAt
	}
	return result
}
func (s *IdentityService) catalogHostedThreadRecord(row catalogIdentity) (ownedthreads.SessionRecord, bool) {
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
