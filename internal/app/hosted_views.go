package app

import (
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
)

func (s *server) hostedViews() *hostedmodel.ViewService {
	return &hostedmodel.ViewService{Executions: s.agentExecutions, Metadata: s.controlMetadata,
		ExecutionError: s.executionControlErr, MetadataError: s.controlMetadataErr,
		Sessions: &s.Registry, Queue: s.queuedTurns, Catalog: s.hostedCatalog(),
		Boot:          &s.bootAttachments,
		ResolveRecord: s.hostedController().ResolveHostedRecord}
}
