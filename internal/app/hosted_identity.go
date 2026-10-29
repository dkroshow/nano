package app

import (
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threadindex"
)

func (s *server) hostedIdentity() *hostedmodel.IdentityService {
	if s == nil {
		return &hostedmodel.IdentityService{}
	}

	return &hostedmodel.IdentityService{Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, Catalog: s.hostedCatalog(), Sessions: &s.Registry, CatalogErrors: hostedmodel.CatalogErrors{NotFound: threadindex.ErrCatalogThreadNotFound, Unavailable: threadindex.ErrThreadCatalogUnavailable, InvalidCursor: threadindex.ErrInvalidThreadCatalogCursor}}
}
