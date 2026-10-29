package app

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threads"
)

// hostedSessionForRequest is sessionForHostedThread for a request that needs
// the thread live: a thread still starting after a restart is registered first.
func (s *server) hostedSessionForRequest(id string) *sessions.Session {
	s.attachmentCoordinator().ReattachWaitingHosted(id)
	return s.SessionForHostedThread(id)
}
func (s *server) handleHostedThreadConversation(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.HandleConversationView(w, r, conversationViewMessages)
}

func (s *server) handleHostedThreadConversationEvents(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.HandleConversationEventsView(w, r, conversationViewMessages)
}

func (s *server) handleHostedThreadInteraction(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.HandleInteraction(w, r, r.PathValue("requestId"))
}

func (s *server) handleHostedThreadInterrupt(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.HandleInterrupt(w, r)
}

// handleHostedThreadLive streams one pane of a file-only thread: its status
// and pending questions, its rows and its live text, in one ordered event
// stream. Each pane opens its own.
func (s *server) handleHostedThreadLive(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "no live session is attached to this thread", http.StatusConflict)
		return
	}
	sess.HandleLiveStream(w, r)
}
