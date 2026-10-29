package app

import (
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
)

func (s *server) creationNames() *hostedmodel.CreationNames {
	reader, _ := s.catalog.(hostedmodel.CreationNameReader)
	refs, _ := s.catalog.(hostedmodel.CreationNameRefReader)
	return &hostedmodel.CreationNames{Metadata: s.attachmentCoordinator().ControlStore(), Executions: s.agentExecutions, Queue: s.queuedTurns, Reader: reader, Refs: refs, Catalog: s.hostedCatalog(), Refresh: s.attachmentCoordinator().NotifyRuntimeRefresh}
}
func (s *server) creationNameForHosted(id string) string {
	if s.controlMetadata == nil {
		return ""
	}
	name, _ := s.controlMetadata.CreationName(id)
	return name
}
func (s *server) publishCreationName(provider, native, name string) {
	s.creationNames().Publish(provider, native, name)
}
