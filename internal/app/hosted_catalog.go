package app

import (
	"github.com/dkroshow/nano/internal/threadindex"
)

func (s *server) hostedCatalog() *threadindex.Service {
	catalog, ok := s.catalog.(*threadCatalogService)
	if !ok {
		return nil
	}
	return catalog.Service
}
