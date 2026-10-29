package app

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/httpapi"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

func (s *server) handleSessionCreateAction(w http.ResponseWriter, r *http.Request) {
	// Share admission serialization with hosted and legacy explicit close.
	var req hostedmodel.SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		httpapi.WriteHTTPError(w, "body must be {\"provider\": \"codex\"|\"claude\"|\"grok\", ...spawn options}", http.StatusBadRequest)
		return
	}
	sess, err := s.attachmentCoordinator().Spawn(req)
	if err != nil {
		if errors.Is(err, hostedmodel.ErrHostedControlUnavailable) {
			message := err.Error()
			if s.executionControlErr != "" {
				message += ": " + s.executionControlErr
			} else if s.controlMetadataErr != "" {
				message += ": " + s.controlMetadataErr
			}
			httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
			return
		}
		// Frozen contract: a resume onto an already-attached thread is a
		// 409 whose body carries the session to jump to.
		var attached *sessionruntime.ThreadAttachedError
		if errors.As(err, &attached) {
			httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
				Code: "thread_attached", Message: "Thread is already attached", Details: map[string]string{"existingSessionId": attached.ExistingSessionID},
			}})

			return
		}
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	sum := sess.Summary() // snapshot under the state lock; claude's threadID lands async
	log.Printf("spawned %s session %s (thread %q, cwd %s)", sum.Provider, sum.ID, sum.ThreadID, sum.Cwd)
	httpapi.WriteJSON(w, sum)
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	s.hostedController().HandleLegacyCreate(w, r)
}

func (s *server) handleSessionClose(w http.ResponseWriter, r *http.Request, sess *sessionruntime.Session) {
	s.hostedController().HandleLegacyClose(w, r, sess)
}
func (s *server) handleAttachmentScoped(w http.ResponseWriter, r *http.Request) {
	s.hostedController().HandleAttachmentScoped(w, r)
}
