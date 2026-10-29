package app

import (
	fileapi "github.com/dkroshow/nano/internal/files/api"
)

func (s *server) fileHandlers() *fileapi.Handlers {
	return &fileapi.Handlers{Catalog: s.catalog, Registry: &s.Registry}
}
