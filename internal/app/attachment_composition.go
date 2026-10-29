package app

import (
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
)

func (s *server) attachmentCoordinator() *hostedmodel.AttachmentCoordinator {
	s.attachmentOnce.Do(func() {
		sources := hostedmodel.AttachmentSources{
			Defaults: s.defaults, Validations: s.transcriptValidations, Codex: s.codex, SpoolDir: s.spoolDir, Catalog: s.hostedCatalog(), CatalogRefresh: s.catalogRefresh, CatalogSources: s.catalogSources,
			Runtime: s.sidebarOwner(), Registry: &s.Registry, Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, ExecutionError: s.executionControlErr,
			Attachments: s.attachments, Queue: s.queuedTurns, Turns: s.turns, SendAttempts: s.sendAttempts, CompletedTurns: s.completedTurns, Skills: s.skills, Notices: s.notices, Stopping: &s.stopping,
			DeliveryExpiries: s.deliveryExpiries, Restart: s.restart, Boot: &s.bootAttachments, CreationNames: nil, ProcessStarted: processStarted, DropLiveTextDeltas: s.dropLiveTextDeltas,
			SweepSubscriptions: func(id string) { s.agentController().SweepSubscriptions(id) },
		}
		if s.browserDiagnostics != nil {
			sources.Diagnostics = s.browserDiagnostics
		}
		metadata := s.controlMetadata
		if (metadata == nil || s.controlMetadataErr != "") && s.agentExecutions != nil {
			metadata = s.agentExecutions.Control()
		}
		reader, _ := s.catalog.(hostedmodel.CreationNameReader)
		refs, _ := s.catalog.(hostedmodel.CreationNameRefReader)
		sources.CreationNames = &hostedmodel.CreationNames{Metadata: metadata, Executions: s.agentExecutions, Queue: s.queuedTurns, Reader: reader, Refs: refs, Catalog: s.hostedCatalog(), Refresh: func() { s.attachmentCoordinator().NotifyRuntimeRefresh() }}
		s.attachmentsControl = hostedmodel.NewAttachmentCoordinator(sources)
	})
	return s.attachmentsControl
}
