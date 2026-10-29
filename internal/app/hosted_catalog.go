package app

import (
	"github.com/dkroshow/nano/internal/threadindex"
)

func (s *server) hostedCatalog() *threadindex.Service {
	if s.catalog == nil {
		return nil
	}
	return s.catalog.Service
}
