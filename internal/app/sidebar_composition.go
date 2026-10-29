package app

import "github.com/dkroshow/nano/internal/sidebar"

func (s *server) sidebarOwner() *sidebar.Controller {
	if s.sidebar == nil {
		s.sidebar = sidebar.New(sidebar.Sources{Catalog: s.hostedCatalog(), Registry: &s.Registry, Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, Attachments: s.attachments, Boot: &s.bootAttachments, Stars: s.threadStars, Mail: s.mail, MailError: s.mailErr, RuntimeEvents: s.runtimeEvents})
	}
	return s.sidebar
}
