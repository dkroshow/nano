package app

import (
	"context"
	"net/http"

	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threadindex"
)

type identityResolveResponse = hosted.IdentityResolution

func (s *server) hostedIdentity() *hosted.IdentityService {
	if s == nil {
		return &hosted.IdentityService{}
	}

	return &hosted.IdentityService{Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, Catalog: s.hostedCatalog(), Sessions: &s.Registry, CatalogErrors: hosted.CatalogErrors{NotFound: threadindex.ErrCatalogThreadNotFound, Unavailable: threadindex.ErrThreadCatalogUnavailable, InvalidCursor: threadindex.ErrInvalidThreadCatalogCursor}}
}
func (s *server) handleIdentityResolve(w http.ResponseWriter, r *http.Request) {
	s.hostedIdentity().HandleIdentityResolve(w, r)
}
func (s *server) handleThreadReference(w http.ResponseWriter, r *http.Request) {
	s.hostedIdentity().HandleThreadReference(w, r)
}
func (s *server) resolveIdentity(ctx context.Context, ref string) (identityResolveResponse, error) {
	return s.hostedIdentity().ResolveIdentity(ctx, ref)
}
func (s *server) displayNameForHosted(ctx context.Context, provider, id, native string) string {
	return s.hostedIdentity().DisplayNameForHosted(ctx, provider, id, native)
}
func (s *server) agentDisplay(ctx context.Context, id string) string {
	return s.hostedIdentity().DisplayAgent(ctx, id)
}
