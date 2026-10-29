package hosted

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/liveview"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threads"
)

// hostedSessionForRequest is sessionForHostedThread for a request that needs
// the thread live: a thread still starting after a restart is registered first.
func (s *Controller) hostedSessionForRequest(id string) *sessions.Session {
	s.AttachmentsControl.reattachWaitingHosted(id)
	return s.Sessions.SessionForHostedThread(id)
}

// HandleHostedThreadConversation serves the attached hosted session.
func (s *Controller) HandleHostedThreadConversation(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.View().HandleConversationView(w, r, liveview.Messages)
}

// HandleHostedThreadConversationEvents serves the attached hosted session.
func (s *Controller) HandleHostedThreadConversationEvents(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.View().HandleConversationEventsView(w, r, liveview.Messages)
}

// HandleHostedThreadInteraction serves the attached hosted session.
func (s *Controller) HandleHostedThreadInteraction(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.Delivery().HandleInteraction(w, r, r.PathValue("requestId"))
}

// HandleHostedThreadInterrupt serves the attached hosted session.
func (s *Controller) HandleHostedThreadInterrupt(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.Delivery().HandleInterrupt(w, r)
}

// HandleHostedThreadLive streams one pane of a file-only thread: its status
// and pending questions, its rows and its live text, in one ordered event
// stream. Each pane opens its own.
// HandleHostedThreadLive serves the attached hosted session.
func (s *Controller) HandleHostedThreadLive(w http.ResponseWriter, r *http.Request, record threads.SessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "no live session is attached to this thread", http.StatusConflict)
		return
	}
	sess.View().HandleLiveStream(w, r)
}
