package app

import (
	"context"

	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threadindex"
)

// threadCatalogNamed is the exact-name lookup used by identity resolve. The
// production catalog queries the derived names table; tests may omit it and
// fall back to the in-memory list fixture.
type threadCatalogNamed interface {
	NamedThreads(ctx context.Context, name string) ([]catalogThread, error)
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
	page, err := s.catalog.ListThreads(ctx, threadindex.ListQuery{
		IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true,
		Archived: archivedIncludeFilter, Limit: threadindex.MaxThreadCatalogLimit,
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
func (s *server) catalogRowsForNative(ctx context.Context, nativeID, provider string) ([]catalogThread, error) {
	page, err := s.catalog.ListThreads(ctx, threadindex.ListQuery{
		ThreadID: nativeID, IncludeSubagents: true, IncludeGuardians: true,
		IncludeProgrammatic: true, Archived: archivedIncludeFilter, Limit: threadindex.MaxThreadCatalogLimit,
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

type hostedIdentityCatalog struct{ server *server }

func (a hostedIdentityCatalog) NativeIdentities(ctx context.Context, nativeID, provider string) ([]hosted.CatalogIdentity, error) {
	rows, err := a.server.catalogRowsForNative(ctx, nativeID, provider)
	return hostedCatalogIdentities(rows), err
}
func (a hostedIdentityCatalog) NamedIdentities(ctx context.Context, name string) ([]hosted.CatalogIdentity, error) {
	rows, err := a.server.namedCatalogThreads(ctx, name)
	return hostedCatalogIdentities(rows), err
}
func (a hostedIdentityCatalog) LoadIdentity(ctx context.Context, id string) (hosted.CatalogIdentity, error) {
	row, err := a.server.catalog.LoadThread(ctx, id)
	return catalogIdentity(row), err
}
func (a hostedIdentityCatalog) IsIndexing(ctx context.Context) bool {
	return a.server.catalogIsIndexing(ctx)
}
func (a hostedIdentityCatalog) DurableName(provider, nativeID string) (string, bool) {
	return a.server.durableThreadName(provider, nativeID)
}
func hostedCatalogIdentities(rows []catalogThread) []hosted.CatalogIdentity {
	result := make([]hosted.CatalogIdentity, 0, len(rows))
	for _, row := range rows {
		result = append(result, catalogIdentity(row))
	}
	return result
}
func catalogIdentity(row catalogThread) hosted.CatalogIdentity {
	result := hosted.CatalogIdentity{ID: row.ID, Provider: row.Provider, NativeID: row.NativeID, HostedThreadID: row.HostedThreadID}
	if row.Name != nil {
		result.Name = row.Name.Value
	}
	if row.UpdatedAt != nil {
		result.UpdatedAt = *row.UpdatedAt
	}
	return result
}
