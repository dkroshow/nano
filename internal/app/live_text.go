package app

import (
	"github.com/dkroshow/nano/internal/sessions"
)

func (s *server) startLiveText(sess *sessions.Session) {
	source := sessions.LiveTextSources{Catalog: s.hostedCatalog()}
	if s.browserDiagnostics != nil {
		source.Diagnostics = s.browserDiagnostics
	}
	sess.AttachLiveText(sessions.NewLiveTextLoop(source, sess.State()))
}
