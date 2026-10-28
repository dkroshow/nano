package app

import "github.com/dkroshow/nano/internal/hosted"

type creationNameProjectionRecord = hosted.CreationNameProjection

func (s *server) creationNames() *hosted.CreationNames {
	reader, _ := s.catalog.(hosted.CreationNameReader)
	refs, _ := s.catalog.(hosted.CreationNameRefReader)
	projector, _ := s.catalog.(hosted.CreationNameProjector)
	return &hosted.CreationNames{Metadata: s.controlStore(), Executions: s.agentExecutions, Queue: s.queuedTurns, Reader: reader, Refs: refs, Projector: projector, Refresh: s.notifyRuntimeRefresh}
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
