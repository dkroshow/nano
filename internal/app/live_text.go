package app

import (
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

func (s *server) startLiveText(sess *sessions.Session) {
	source := sessions.LiveTextSources{}
	source.Catalog, _ = s.catalog.(sessions.LiveTextCatalog)
	source.References, _ = s.catalog.(threadindex.ReferenceResolver)
	source.Transcripts, _ = s.catalog.(threadindex.TranscriptCatalog)
	if deferred, ok := s.catalog.(interface{ DeferredTranscriptReread(string) bool }); ok {
		source.DeferredReread = deferred.DeferredTranscriptReread
	}
	if s.browserDiagnostics != nil {
		source.Diagnostics = s.browserDiagnostics.Store
	}
	sess.AttachLiveText(sessions.NewLiveTextLoop(source, sess))
}
