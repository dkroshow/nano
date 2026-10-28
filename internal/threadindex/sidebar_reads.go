package threadindex

import (
	"context"

	"github.com/dkroshow/nano/internal/history"
)

// ListSidebarGraph returns saved rows for the application’s sidebar projection.
func (s *Service) ListSidebarGraph(ctx context.Context, params history.SidebarGraphParams) (history.SidebarGraph, error) {
	if s == nil || s.store == nil {
		return history.SidebarGraph{}, ErrThreadCatalogUnavailable
	}
	return s.store.ListSidebarGraph(ctx, params)
}

// LoadThreadsByIDs returns saved metadata for the requested catalog identities.
func (s *Service) LoadThreadsByIDs(ctx context.Context, ids []string) ([]history.Thread, error) {
	if s == nil || s.store == nil {
		return nil, ErrThreadCatalogUnavailable
	}
	return s.store.LoadThreadsByIDs(ctx, ids)
}
