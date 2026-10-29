package hosted

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

func (s *Controller) handleSessionCreateAction(w http.ResponseWriter, r *http.Request) {
	// Share admission serialization with hosted and legacy explicit close.
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Provider == "" {
		httpapi.WriteHTTPError(w, "body must be {\"provider\": \"codex\"|\"claude\"|\"grok\", ...spawn options}", http.StatusBadRequest)
		return
	}
	sess, err := s.AttachmentsControl.Spawn(req)
	if err != nil {
		if errors.Is(err, errHostedControlUnavailable) {
			message := err.Error()
			if s.ExecutionError != "" {
				message += ": " + s.ExecutionError
			} else if s.MetadataError != "" {
				message += ": " + s.MetadataError
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
