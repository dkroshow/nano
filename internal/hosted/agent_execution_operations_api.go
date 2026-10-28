package hosted

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
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
	if rec.Status != agentTurnQueued {
		httpapi.WriteHTTPError(w, "turn operation is not queued", http.StatusConflict)
		return
	}
	conflict, err := s.Runtime.CancelPrompt(rec.HostedThreadID, rec.TransportActionID)
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
