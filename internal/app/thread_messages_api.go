package app

import (
	"context"
	"net/http"
	"net/url"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threadindex"
)

// thread_messages_api.go — the agent-ergonomic tail of a thread: GET
// /api/v1/threads/{id}/messages?tail=N&roles=user,agent&format=text returns
// clean chronological plain text, so "show me the last messages of thread X" is
// one curl with no cursors and no jq.
//
// It is a thin projection over the existing transcript service — no second
// parser, no direct SQLite read — and resolves ids the way an agent types them:
// an exact nano or native id, or an unambiguous prefix of either.

// hostedThreadNativeID returns the current native thread id for a hosted id,
// preferring the live session, then the durable execution record. The second
// return is false when no hosted record exists at all.
func (s *server) hostedThreadNativeID(hostedID string) (string, bool) {
	if live, ok := s.LiveHostedSummary(hostedID, s.agentExecutions.SessionNeedsAttention); ok {
		return live.ThreadID, true
	}
	if s.agentExecutions == nil {
		return "", false
	}
	record, ok := s.agentExecutions.Session(hostedID)
	if !ok {
		return "", false
	}
	return record.NativeThreadID, true
}

func (s *server) handleThreadMessages(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if _, ok := s.catalog.(threadindex.TranscriptCatalog); s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	reference, err := url.PathUnescape(rawID)
	if err == nil && hostedmodel.IsHostedReference(reference) {
		// Query errors precede hosted lookup errors, as for native references.
		if _, err := threadindex.ParseMessagesQuery(r); err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		nativeID, ok := s.hostedThreadNativeID(reference)
		if !ok {
			httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Unknown hosted thread")
			return
		}
		if nativeID == "" {
			httpapi.WriteError(w, http.StatusNotFound, "thread_not_indexed", "Hosted thread has no native transcript yet")
			return
		}
		rawID = url.PathEscape(nativeID)
	}
	threadindex.NewHandlers(s.catalog).HandleThreadMessages(w, r, rawID)
}
func (s *server) resolveThreadReference(ctx context.Context, reference string) (string, error) {
	resolver, _ := s.catalog.(threadindex.ReferenceResolver)
	return threadindex.ResolveReference(ctx, resolver, reference)
}
func writeThreadReferenceError(w http.ResponseWriter, err error) {
	threadindex.WriteThreadReferenceError(w, err)
}
