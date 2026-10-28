package app

import (
	"encoding/json"
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

type transcriptRepairRequest struct {
	ClientActionID string `json:"clientActionId"`
}

func (s *server) handleTranscriptRepair(w http.ResponseWriter, r *http.Request, sess *session) {
	var request transcriptRepairRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ClientActionID == "" || len(request.ClientActionID) > 200 {
		httpapi.WriteHTTPError(w, "valid clientActionId is required", http.StatusBadRequest)
		return
	}
	response, err := sess.stopDiscardAndRepair(r.Context(), request.ClientActionID)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusConflict)
		return
	}
	httpapi.WriteJSON(w, response)
}
