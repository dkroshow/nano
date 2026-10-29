package app

import (
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/observability"
)

func (s *server) monitorHandlers() *observability.MonitorHandlers {
	return &observability.MonitorHandlers{
		Monitor: s.hostMonitor, Requests: &s.requests, Catalog: s.hostedCatalog(),
		Live:        s.Registry.MonitorSnapshot,
		WarmStreams: func() any { return s.sidebarOwner().WarmReport() },
		Restarts:    func() (any, error) { return s.restart.Recent(hosted.DefaultRestartsLimit, false) },
	}
}
