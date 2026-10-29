package app

import (
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
)

func (s *server) attachmentCoordinator() *hostedmodel.AttachmentCoordinator {
	if s.attachmentsControl == nil {
		panic("attachment coordinator used before construction")
	}
	return s.attachmentsControl
}

func newAttachmentCoordinator(s *server) *hostedmodel.AttachmentCoordinator {
	sources := hostedmodel.AttachmentSources{
		Defaults: s.defaults, Validations: s.transcriptValidations, Codex: s.codex, SpoolDir: s.spoolDir, Catalog: s.hostedCatalog(), CatalogRefresh: s.catalogRefresh, CatalogSources: s.catalogSources,
		Runtime: s.sidebarOwner(), Registry: &s.Registry, Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, ExecutionError: s.executionControlErr,
		Attachments: s.attachments, Queue: s.queuedTurns, Turns: s.turns, SendAttempts: s.sendAttempts, CompletedTurns: s.completedTurns, Skills: s.skills, Notices: s.notices, Stopping: &s.stopping,
		DeliveryExpiries: s.deliveryExpiries, Restart: s.restart, Boot: &s.bootAttachments, ProcessStarted: processStarted, DropLiveTextDeltas: s.dropLiveTextDeltas,
		SweepSubscriptions: func(id string) { s.agentController().SweepSubscriptions(id) },
	}
	if s.browserDiagnostics != nil {
		sources.Diagnostics = s.browserDiagnostics
	}
	return hostedmodel.NewAttachmentCoordinator(sources)
}
