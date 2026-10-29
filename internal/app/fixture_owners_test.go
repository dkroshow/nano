package app

import "sync"

var fixtureOwnersMu sync.Mutex

// fixtureOwners assembles the concrete owners for a manually built test server.
// Production constructs them eagerly after opening every store.
func fixtureOwners(s *server) *server {
	fixtureOwnersMu.Lock()
	defer fixtureOwnersMu.Unlock()
	if s.sidebar == nil {
		s.sidebar = newSidebarController(s)
	}
	if s.attachmentsControl == nil {
		s.attachmentsControl = newAttachmentCoordinator(s)
	}
	if s.hostedControl == nil {
		s.hostedControl = newHostedController(s)
	}
	if s.agent == nil {
		s.agent = newAgentController(s)
	}
	return s
}
