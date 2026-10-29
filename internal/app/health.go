package app

import (
	"fmt"
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if s.agentExecutions == nil || s.controlMetadata == nil || s.mail == nil || s.executionControlErr != "" || s.controlMetadataErr != "" || s.mailErr != "" {
		reason := s.executionControlErr
		if reason == "" {
			reason = s.controlMetadataErr
		}
		if reason == "" {
			reason = s.mailErr
		}
		if reason == "" {
			reason = "required control store is unavailable"
		}
		httpapi.WriteHTTPError(w, "hosted control unavailable: "+reason, http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}
