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
	var read func(context.Context, string, int) ([]hostedmodel.PinCandidate, error)
	if transcripts, ok := s.catalog.(threadindex.TranscriptCatalog); ok {
		read = func(ctx context.Context, id string, tail int) ([]hostedmodel.PinCandidate, error) {
			messages, err := threadindex.CollectThreadMessages(ctx, transcripts, id, threadindex.MessagesQuery{Tail: tail, Roles: map[string]bool{threadindex.MessageRoleUser: true, threadindex.MessageRoleAgent: true}})
			if err != nil {
				return nil, err
			}
			out := make([]hostedmodel.PinCandidate, 0, len(messages))
			for _, message := range messages {
				out = append(out, hostedmodel.PinCandidate{ItemID: message.ItemID, Role: message.Role, At: message.At, Preview: message.Preview})
			}
			return out, nil
		}
	}
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
		ReadCandidates: read, WriteReferenceError: threadindex.WriteThreadReferenceError, WriteTranscriptError: threadindex.WriteTranscriptBackendError,
		DefaultCandidateLimit: threadindex.DefaultMessagesTail, MaxCandidateLimit: threadindex.MaxMessagesTail}
}
