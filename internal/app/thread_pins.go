package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threadindex"
)

type threadPinsStore = hosted.PinStore
type threadPinCatalog = hosted.PinCatalog

var openThreadPinsStore = hosted.OpenPinStore

func (s *server) hostedPins() *hosted.PinService {
	catalog, _ := s.catalog.(hosted.PinCatalog)
	var read func(context.Context, string, int) ([]hosted.PinCandidate, error)
	if transcripts, ok := s.catalog.(threadindex.TranscriptCatalog); ok {
		read = func(ctx context.Context, id string, tail int) ([]hosted.PinCandidate, error) {
			messages, err := threadindex.CollectThreadMessages(ctx, transcripts, id, threadindex.MessagesQuery{Tail: tail, Roles: map[string]bool{threadindex.MessageRoleUser: true, threadindex.MessageRoleAgent: true}})
			if err != nil {
				return nil, err
			}
			out := make([]hosted.PinCandidate, 0, len(messages))
			for _, message := range messages {
				out = append(out, hosted.PinCandidate{ItemID: message.ItemID, Role: message.Role, At: message.At, Preview: message.Preview})
			}
			return out, nil
		}
	}
	return &hosted.PinService{Store: s.threadPins, Executions: s.agentExecutions, Catalog: catalog,
		CatalogErrors: hosted.CatalogErrors{NotFound: threadindex.ErrCatalogThreadNotFound, Unavailable: threadindex.ErrThreadCatalogUnavailable, InvalidCursor: threadindex.ErrInvalidThreadCatalogCursor},
		CurrentNative: s.hostedThreadNativeID, ResolveHosted: s.resolveHostedThreadRecord, ResolveIdentity: s.resolveIdentity, ResolveThread: s.resolveThreadReference,
		IsAmbiguous: func(err error) bool {
			var ambiguous *threadindex.AmbiguousReferenceError
			return errors.As(err, &ambiguous)
		},
		ReadCandidates: read, WriteReferenceError: writeThreadReferenceError, WriteTranscriptError: writeTranscriptBackendError,
		DefaultCandidateLimit: threadindex.DefaultMessagesTail, MaxCandidateLimit: threadindex.MaxMessagesTail}
}

func (s *server) handleThreadPinCreate(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedPins().HandleCreate(w, r, record)
}

func (s *server) handleThreadPinList(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedPins().HandleList(w, r, record)
}

func (s *server) handleThreadPinRead(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedPins().HandleRead(w, r, record)
}

func (s *server) handleThreadPinContext(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedPins().HandleContext(w, r, record)
}

func (s *server) handleThreadPinCandidates(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	s.hostedPins().HandleCandidates(w, r, record)
}

func (s *server) handleThreadPinMonitor(w http.ResponseWriter, r *http.Request) {
	s.hostedPins().HandleMonitor(w, r)
}
