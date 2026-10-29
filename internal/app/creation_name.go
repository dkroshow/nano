package app

import (
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
)

func (s *server) creationNames() *hostedmodel.CreationNames {
	var reader hostedmodel.CreationNameReader
	var refs hostedmodel.CreationNameRefReader
	if s.catalog != nil {
		reader, refs = s.catalog, s.catalog
	}
	return &hostedmodel.CreationNames{Metadata: s.attachmentCoordinator().ControlStore(), Executions: s.agentExecutions, Queue: s.queuedTurns, Reader: reader, Refs: refs, Catalog: s.hostedCatalog(), Refresh: s.attachmentCoordinator().NotifyRuntimeRefresh}
}
