package app

import (
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func (s *server) hostedCatalog() *threadindex.Service {
	catalog, ok := s.catalog.(*sidebar.CatalogSource)
	if !ok {
		return nil
	}
	return catalog.Service
}
