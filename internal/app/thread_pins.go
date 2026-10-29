package app

import (
	"context"
	"errors"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threadindex"
)

var openThreadPinsStore = hostedmodel.OpenPinStore

func (s *server) hostedPins() *hostedmodel.PinService {
	catalog, _ := s.catalog.(hostedmodel.PinCatalog)
	transcripts, _ := s.catalog.(threadindex.TranscriptCatalog)
	return &hostedmodel.PinService{Store: s.threadPins, Executions: s.agentExecutions, Catalog: catalog,
		CatalogErrors: hostedmodel.CatalogErrors{NotFound: threadindex.ErrCatalogThreadNotFound, Unavailable: threadindex.ErrThreadCatalogUnavailable, InvalidCursor: threadindex.ErrInvalidThreadCatalogCursor},
		CurrentNative: s.hostedIdentity().NativeID, ResolveHosted: s.hostedController().ResolveHostedRecord, ResolveIdentity: s.hostedIdentity().ResolveIdentity, ResolveThread: func(ctx context.Context, ref string) (string, error) {
			resolver, _ := s.catalog.(threadindex.ReferenceResolver)
			return threadindex.ResolveReference(ctx, resolver, ref)
		},
		IsAmbiguous: func(err error) bool {
			var ambiguous *threadindex.AmbiguousReferenceError
			return errors.As(err, &ambiguous)
		},
		Transcripts: transcripts, WriteReferenceError: threadindex.WriteThreadReferenceError, WriteTranscriptError: threadindex.WriteTranscriptBackendError,
		DefaultCandidateLimit: threadindex.DefaultMessagesTail, MaxCandidateLimit: threadindex.MaxMessagesTail}
}
