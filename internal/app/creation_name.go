package app

import "github.com/dkroshow/nano/internal/hosted"

func (s *server) creationNames() *hosted.CreationNames {
	reader, _ := s.catalog.(hosted.CreationNameReader)
	refs, _ := s.catalog.(hosted.CreationNameRefReader)
	return &hosted.CreationNames{Metadata: s.attachmentCoordinator().ControlStore(), Executions: s.agentExecutions, Queue: s.queuedTurns, Reader: reader, Refs: refs, Catalog: s.hostedCatalog(), Refresh: s.attachmentCoordinator().NotifyRuntimeRefresh}
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
