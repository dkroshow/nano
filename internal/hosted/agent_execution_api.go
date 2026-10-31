package hosted

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	hostedprompts "github.com/dkroshow/nano/internal/core/prompts"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// HostedOperation resolves the route reference before invoking a hosted handler.
func (s *Controller) HostedOperation(handler func(http.ResponseWriter, *http.Request, ownedthreads.SessionRecord)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Executions == nil {
			message := "execution control unavailable; durable hosted mutations are disabled"
			if s.ExecutionError != "" {
				message += ": " + s.ExecutionError
			}
			httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
			return
		}
		ref := strings.TrimSpace(r.PathValue("threadRef"))
		if ref == "" {
			httpapi.WriteNotFound(w)
			return
		}
		record, ok := s.ResolveHostedRecord(ref)
		if !ok {
			httpapi.WriteHTTPError(w, "unknown hosted thread", http.StatusNotFound)
			return
		}
		handler(w, r, record)
	}
}

// HandleRead serves the current hosted thread view.
func (s *Controller) HandleRead(w http.ResponseWriter, _ *http.Request, record ownedthreads.SessionRecord) {
	httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
}

// HandleDelete removes a closed hosted thread and its durable execution records.
func (s *Controller) HandleDelete(w http.ResponseWriter, _ *http.Request, record ownedthreads.SessionRecord) {
	// Never call code that can re-enter this server while relationshipMu is held.
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	// Destroy, matching the contract the docs have always published: close if
	// live, then erase the durable record and its operations. Callers that mean
	// "end the attachment, keep the record" use POST {threadRef}/close.
	if err := s.closeHostedThreadForRecord(record); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.dropQueuedTurns(record); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Executions.RemoveSession(record.ID); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.Metadata != nil {
		if err := s.Metadata.Destroy(record.ID); err != nil {
			log.Printf("destroy canonical metadata %s: %v", record.ID, err)
		}
	}
	httpapi.WriteJSON(w, map[string]any{"id": record.ID, "deleted": true})
}

// HandleClose closes the provider attachment and settles hosted operations.
func (s *Controller) HandleClose(w http.ResponseWriter, _ *http.Request, record ownedthreads.SessionRecord) {
	// Never call code that can re-enter this server while relationshipMu is held.
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	if err := s.closeHostedThreadForRecord(record); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.dropQueuedTurns(record); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Executions.UpdateSession(record.ID, "", "", hostedstore.StatusExited, "closed"); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
}

// ValidClientID reports whether a caller id is safe as one URL path segment.
func ValidClientID(value string) bool {
	if value == "" || len(value) > 200 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '.', '_', ':', '@', '-':
			continue
		default:
			return false
		}
	}
	return true
}

// HandleTurnSubmit admits and delivers an idempotent hosted turn.
func (s *Controller) HandleTurnSubmit(w http.ResponseWriter, r *http.Request, hostedThreadID string) {
	var req struct {
		Prompt          string `json:"prompt"`
		ClientActionID  string `json:"clientActionId"`
		Origin          string `json:"origin,omitempty"`
		RequireIdle     bool   `json:"requireIdle,omitempty"`
		NextTurn        bool   `json:"nextTurn,omitempty"`
		SenderThreadRef string `json:"senderThreadRef,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Prompt) == "" {
		httpapi.WriteHTTPError(w, "body must be {\"prompt\":\"...\",\"clientActionId\":\"...\"}", http.StatusBadRequest)
		return
	}
	req.ClientActionID = strings.TrimSpace(req.ClientActionID)
	if req.ClientActionID == "" || len(req.ClientActionID) > 200 {
		httpapi.WriteHTTPError(w, "clientActionId is required and must be at most 200 characters", http.StatusBadRequest)
		return
	}
	origin := strings.TrimSpace(req.Origin)
	if origin == "" {
		origin = string(hostedprompts.AgentOrigin)
	}
	if origin != string(hostedprompts.AgentOrigin) && origin != string(hostedprompts.SystemOrigin) {
		httpapi.WriteHTTPError(w, "origin must be agent or system", http.StatusBadRequest)
		return
	}
	if origin == string(hostedprompts.AgentOrigin) && strings.HasPrefix(req.ClientActionID, "nano:") {
		httpapi.WriteHTTPError(w, "clientActionId prefix nano: is reserved for Nano", http.StatusBadRequest)
		return
	}
	if origin == string(hostedprompts.SystemOrigin) && !strings.HasPrefix(req.ClientActionID, "nano:") {
		httpapi.WriteHTTPError(w, "system-origin clientActionId must use the reserved nano: prefix", http.StatusBadRequest)
		return
	}
	if req.RequireIdle && req.NextTurn {
		httpapi.WriteHTTPError(w, "requireIdle and nextTurn cannot be combined", http.StatusBadRequest)
		return
	}
	submission := hostedTurnSubmission{Prompt: req.Prompt, RequireIdle: req.RequireIdle, NextTurn: req.NextTurn, Origin: origin}
	ref := strings.TrimSpace(req.SenderThreadRef)
	if origin == string(hostedprompts.SystemOrigin) {
		if ref != "" {
			writeSenderAdmissionError(w, "sender_forbidden", "senderThreadRef is not used for system-origin turn submissions")
			return
		}
		submission.Attribution = hostedstore.TurnAttribution{Origin: string(hostedprompts.SystemOrigin)}
		footer := senderFooter(submission.Attribution)
		if feature, ok := s.FeatureFooter(req.ClientActionID); ok {
			footer = feature
		}
		submission.Delivered = req.Prompt + "\n\n" + footer
	} else {
		if ref == "" {
			writeSenderAdmissionError(w, "sender_required", "senderThreadRef is required for agent-origin turn submissions")
			return
		}
		sender, ok := s.ResolveHostedRecord(ref)
		if !ok {
			writeSenderAdmissionError(w, "sender_not_hosted", "senderThreadRef does not resolve to a hosted thread")
			return
		}
		attribution, err := s.TurnAttributionFor(sender)
		if err != nil {
			writeSenderAdmissionError(w, "sender_identity_incomplete", err.Error())
			return
		}
		submission.Attribution = attribution
		submission.Delivered = req.Prompt + "\n\n" + senderFooter(submission.Attribution)
	}
	record, existing, err := s.submitHostedTurnFull(hostedThreadID, req.ClientActionID, submission)
	if err != nil {
		if errors.Is(err, s.UnreadFooterUnavailable) {
			httpapi.WriteError(w, http.StatusServiceUnavailable, "unread_footer_unavailable", err.Error())
			return
		}
		var unavailable *hostedTurnAdmissionError
		if errors.As(err, &unavailable) {
			httpapi.WriteErrorEnvelope(w, http.StatusConflict, httpapi.ErrorEnvelope{Error: httpapi.Error{
				Code: "target_not_idle", Message: "Target thread is not idle", Details: map[string]string{"busyReason": unavailable.Reason},
			}})

			return
		}
		httpapi.WriteHTTPError(w, err.Error(), http.StatusConflict)
		return
	}
	if existing {
		httpapi.WriteJSON(w, s.Views().TurnView(record))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	latest, _ := s.Executions.Turn(record.ID)
	_ = json.NewEncoder(w).Encode(s.Views().TurnView(latest))
}

// writeSenderAdmissionError returns before submitHostedTurnFull. That boundary
// is intentional: rejected public agent submissions create no operation, queue
// entry, send attempt, or provider delivery.
func writeSenderAdmissionError(w http.ResponseWriter, code, detail string) {
	httpapi.WriteError(w, http.StatusBadRequest, code, detail)
}
