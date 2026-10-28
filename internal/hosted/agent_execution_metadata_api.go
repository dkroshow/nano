package hosted

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/httpapi"
)

// HandlePresentation updates the hosted presentation mode.
func (s *Controller) HandlePresentation(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	// The mode must be explicit: normalizePresentationMode("") means shared,
	// and a body that omits the field must not silently un-background a
	// thread.
	var req struct {
		PresentationMode string `json:"presentationMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.PresentationMode) == "" {
		httpapi.WriteHTTPError(w, `body must be {"presentationMode":"shared"} or {"presentationMode":"background"}`, http.StatusBadRequest)
		return
	}
	// Order (audited): control.sqlite commit + execution-cache sync inside
	// setPresentation, then the live session under stateMu, then the runtime
	// notify that re-projects the sidebar. Same-value PUTs change nothing
	// and notify nothing.
	rec, _, changed, err := s.Executions.SetPresentation(record.ID, req.PresentationMode)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if changed {
		s.Runtime.SetPresentation(record.ID, rec.PresentationMode)
		s.NotifyRuntime()
	}
	httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
}

// HandleSubscriptions updates the hosted notification choices.
func (s *Controller) HandleSubscriptions(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	var req subscriptions.Choices
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Inbox) == "" || strings.TrimSpace(req.Prompt) == "" {
		httpapi.WriteHTTPError(w, `body must explicitly include inbox and prompt, each "send" or "none"`, http.StatusBadRequest)
		return
	}
	choices, err := subscriptions.NormalizeChoices(req)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, _, _, err = s.Executions.setSubscriptions(record.ID, choices)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
}

// HandleSidebarPlacement updates the hosted sidebar placement fact.
func (s *Controller) HandleSidebarPlacement(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	var req struct {
		SidebarPlacement string `json:"sidebarPlacement"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.SidebarPlacement) == "" {
		httpapi.WriteHTTPError(w, `body must be {"sidebarPlacement":"invoker"} or {"sidebarPlacement":"standalone"}`, http.StatusBadRequest)
		return
	}
	rec, _, changed, err := s.Executions.SetSidebarPlacement(record.ID, req.SidebarPlacement)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if changed {
		s.Runtime.SetSidebarPlacement(record.ID, rec.SidebarPlacement)
		// Placement changes are structural catalog changes, not runtime
		// overlay fields. Force every open browser to re-read the sidebar
		// graph so another tab cannot retain the old invoker edge.
		s.RefreshRuntime()
	}
	httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
}

// HandleAssignment changes an orchestration assignment under the relationship lock.
func (s *Controller) HandleAssignment(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if s.Metadata == nil || s.MetadataError != "" {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		httpapi.WriteHTTPError(w, "invalid assignment request", http.StatusBadRequest)
		return
	}
	var clientActionID string
	assignmentRaw, ok := raw["assignment"]
	if !ok || json.Unmarshal(raw["clientActionId"], &clientActionID) != nil {
		httpapi.WriteHTTPError(w, "body must include clientActionId and assignment", http.StatusBadRequest)
		return
	}
	clientActionID = strings.TrimSpace(clientActionID)
	if !ValidClientID(clientActionID) {
		httpapi.WriteHTTPError(w, "clientActionId is required and must be URL-safe", http.StatusBadRequest)
		return
	}
	var requested *Assignment
	if strings.TrimSpace(string(assignmentRaw)) != "null" {
		if err := json.Unmarshal(assignmentRaw, &requested); err != nil {
			httpapi.WriteHTTPError(w, "assignment must be an object or null", http.StatusBadRequest)
			return
		}
	}
	assignment, err := s.normalizeOrchestrationAssignment(requested)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	retry, err := s.Metadata.orchestrationAssignmentActionExists(record.ID, clientActionID, assignment)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errAssignmentIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	if !retry {
		if err := s.validateAssignmentRelationship(record.ID, assignment); err != nil {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	event, err := s.Metadata.appendOrchestrationAssignment(record.ID, clientActionID, assignment)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errAssignmentIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	s.PublishRelationshipChange()
	httpapi.WriteJSON(w, assignmentMutationView{SessionView: s.Views().SessionView(record.ID), AssignmentEventID: event.ID})
}

// HandleDetach detaches the live provider connection without closing the thread.
func (s *Controller) HandleDetach(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	// Detach never invents turn evidence: with an operation still open the
	// only honest terminal claims are "caller closed" or "process died",
	// and neither is true here. (A turn starting inside this check's race
	// window is settled as interrupted by the close below — narrow, and
	// acceptable under the localhost trust boundary.)
	if s.Executions.HasNonTerminalTurns(record.ID) {
		httpapi.WriteHTTPError(w, "operations still open: interrupt or cancel them, or use close", http.StatusConflict)
		return
	}
	// Claude detach already stopped its child, but used to retain the dead
	// shim handle and hide stop failures. Use explicit close instead; the
	// hosted record and native transcript still support a later resume.
	if record.Provider == "claude" {
		if err := s.closeHostedThreadForRecord(record); err != nil {
			httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
			return
		}
		httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
		return
	}
	if sess := s.Runtime.CloseSession(record.ID); sess != nil {
		sess.Detach()
		s.NotifyRuntime()
	}
	// The attachment record survives — that is the difference from close:
	// boot reattach and click-resume both restore a detached thread.
	_ = s.Executions.UpdateSession(record.ID, "", "", agentSessionDetached, "")
	httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
}

type assignmentMutationView struct {
	SessionView
	AssignmentEventID int64 `json:"assignmentEventId"`
}
