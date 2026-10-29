package app

import "github.com/dkroshow/nano/internal/sidebar"

func (s *server) catalogViews() *sidebar.CatalogViews {
	return &sidebar.CatalogViews{Catalog: s.catalog, Runtime: s.sidebarOwner(), HistoricalCodex: s.historicalCodex, HistoricalClaude: s.historicalClaude, MailEvents: s.mailEvents, Audits: s.transcriptAuditEvents}
}
