package app

import (
	"github.com/dkroshow/nano/internal/sessions"
)

// testServerWithSessions registers sessions after the server's registry is in
// its final location, so fixture construction never copies a used mutex.
func testServerWithSessions(s *server, entries map[string]*sessions.Session) *server {
	seedTestSessionRegistry(&s.Registry, entries)
	return s
}

func seedTestSessionRegistry(registry *sessions.Registry, entries map[string]*sessions.Session) {
	*registry = newSessionRegistry()
	for id, sess := range entries {
		if sess.ID() != id {
			panic("test registry key differs from session identity")
		}
		registry.RegisterSession(sess)
	}
}
