package hosted

import (
	"errors"
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// HandleOperationCancel cancels a queued or active hosted operation.
func (s *Controller) HandleOperationCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.Views().operationID(w, r)
	if ok {
		s.cancelQueuedAgentTurn(w, id)
	}
}

func (s *Controller) cancelQueuedAgentTurn(w http.ResponseWriter, operationID string) {
	rec, ok := s.Executions.Turn(operationID)
	if !ok {
		httpapi.WriteHTTPError(w, "unknown turn operation", http.StatusNotFound)
		return
	}
	if rec.Status != ownedthreads.AgentTurnQueued {
		httpapi.WriteHTTPError(w, "turn operation is not queued", http.StatusConflict)
		return
	}
	var err error
	if sess := s.sessionForHosted(rec.HostedThreadID); sess != nil {
		_, err = sess.CancelQueuedTurnAction(rec.TransportActionID, sessions.DiscardedByCaller)
	} else {
		_, err = sessions.CancelQueuedTurnStores(s.Queue, s.Executions, rec.TransportActionID, sessions.DiscardedByCaller)
	}
	var refused *sessions.PromptActionRefused
	conflict := errors.Is(err, sessions.ErrQueuedTurnNotFound) || errors.Is(err, sessions.ErrQueuedTurnDelivering) || errors.Is(err, sessions.ErrPromptEnded) || errors.As(err, &refused)
	if err != nil {
		status := http.StatusInternalServerError
		if conflict {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	latest, _ := s.Executions.Turn(rec.ID)
	httpapi.WriteJSON(w, s.Views().TurnView(latest))
}
