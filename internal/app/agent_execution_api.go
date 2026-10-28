package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/lifecycle"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type agentSessionView struct {
	ID               string `json:"id"`
	ClientSessionID  string `json:"clientSessionId,omitempty"`
	Provider         string `json:"provider"`
	PresentationMode string `json:"presentationMode"`
	SidebarPlacement string `json:"sidebarPlacement"`
	NanoSessionID    string `json:"nanoSessionId,omitempty"`
	NativeThreadID   string `json:"nativeThreadId,omitempty"`
	// NativeThreadIDProvisional marks a Claude shim stand-in (cs_…) that has not
	// been rekeyed to a real provider id: the thread never completed its first
	// prompt. Consumers must not resume or cite a provisional value.
	NativeThreadIDProvisional bool                         `json:"nativeThreadIdProvisional,omitempty"`
	InvokerHostedThreadID     string                       `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID     string                       `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider           string                       `json:"invokerProvider,omitempty"`
	Subscriptions             subscriptions.Choices        `json:"subscriptions"`
	Status                    string                       `json:"status"`
	Error                     string                       `json:"error,omitempty"`
	ThreadDiagnostics         []threadDiagnostic           `json:"threadDiagnostics,omitempty"`
	Attached                  bool                         `json:"attached"`
	Ready                     bool                         `json:"ready"`
	Working                   bool                         `json:"working"`
	TurnActive                bool                         `json:"turnActive"`
	Waiting                   bool                         `json:"waiting,omitempty"`
	Availability              string                       `json:"availability"`
	AvailabilityReasons       []string                     `json:"availabilityReasons"`
	LastHumanMessageAt        string                       `json:"lastHumanMessageAt,omitempty"`
	AttachmentState           string                       `json:"attachmentState"`
	CreatedAt                 string                       `json:"createdAt"`
	UpdatedAt                 string                       `json:"updatedAt"`
	Revision                  int64                        `json:"revision"`
	Attachment                hostedAttachmentView         `json:"attachment"`
	Metadata                  *hostedMetadataView          `json:"metadata,omitempty"`
	Assignment                *orchestrationAssignmentView `json:"assignment,omitempty"`
	AssignmentUnavailable     bool                         `json:"assignmentUnavailable,omitempty"`
	MetadataUnavailable       bool                         `json:"metadataUnavailable,omitempty"`
	MetadataDegraded          bool                         `json:"metadataDegraded,omitempty"`
}

type agentTurnView struct {
	ID                     string                       `json:"id"`
	HostedThreadID         string                       `json:"hostedThreadId"`
	ClientActionID         string                       `json:"clientActionId"`
	Provider               string                       `json:"provider"`
	NanoSessionID          string                       `json:"nanoSessionId,omitempty"`
	NativeThreadID         string                       `json:"nativeThreadId,omitempty"`
	ProviderTurnID         string                       `json:"providerTurnId,omitempty"`
	Status                 string                       `json:"status"`
	Terminal               bool                         `json:"terminal"`
	Error                  *agentExecutionError         `json:"error,omitempty"`
	FinalItemID            string                       `json:"finalItemId,omitempty"`
	FinalResponse          string                       `json:"finalResponse,omitempty"`
	FinalResponseTruncated bool                         `json:"finalResponseTruncated"`
	FinalResponseBytes     int                          `json:"finalResponseBytes,omitempty"`
	QueuePosition          int                          `json:"queuePosition,omitempty"`
	Origin                 string                       `json:"origin,omitempty"`
	SenderHostedThreadID   string                       `json:"senderHostedThreadId,omitempty"`
	SenderNativeThreadID   string                       `json:"senderNativeThreadId,omitempty"`
	SenderProvider         string                       `json:"senderProvider,omitempty"`
	SenderName             string                       `json:"senderName,omitempty"`
	SenderSubmittedAt      string                       `json:"senderSubmittedAt,omitempty"`
	Interactions           []pendingInteraction         `json:"interactions"`
	Assignment             *orchestrationAssignmentView `json:"assignment,omitempty"`
	AssignmentUnavailable  bool                         `json:"assignmentUnavailable,omitempty"`
	CreatedAt              string                       `json:"createdAt"`
	UpdatedAt              string                       `json:"updatedAt"`
	Revision               int64                        `json:"revision"`
}

type assignmentMutationView struct {
	agentSessionView
	AssignmentEventID int64 `json:"assignmentEventId"`
}

type executionFamilyMemberView struct {
	HostedThreadID        string   `json:"hostedThreadId"`
	Provider              string   `json:"provider"`
	NativeThreadID        string   `json:"nativeThreadId,omitempty"`
	InvokerHostedThreadID string   `json:"invokerHostedThreadId,omitempty"`
	Status                string   `json:"status"`
	OperationIDs          []string `json:"operationIds"`
}

func (s *server) handleAgentSessionsList(w http.ResponseWriter, _ *http.Request) {
	if s.agentExecutions == nil {
		message := "execution control unavailable; durable hosted mutations are disabled"
		if s.executionControlErr != "" {
			message += ": " + s.executionControlErr
		}
		httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
		return
	}
	// A view's status is its record's unless a live session is attached or
	// boot is still reattaching the thread (starting), so any other exited
	// record is left out before its view costs any reads; the store keeps
	// every record a thread ever had.
	attached := make(map[string]bool)
	for _, sess := range s.snapshotSessions() {
		attached[sess.hostedThreadID] = true
	}
	records := s.agentExecutions.listSessions()
	items := make([]agentSessionView, 0, len(records))
	for _, record := range records {
		if _, waiting := s.boot.waitingHosted(record.ID); record.Status == sessionStatusExited && !attached[record.ID] && waiting == "" {
			continue
		}
		view := s.agentSessionView(record.ID)
		if view.Status != sessionStatusExited {
			items = append(items, view)
		}
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}

func (s *server) handleAgentSessionCreate(w http.ResponseWriter, r *http.Request) {
	if s.agentExecutions == nil {
		message := "execution control unavailable; durable hosted mutations are disabled"
		if s.executionControlErr != "" {
			message += ": " + s.executionControlErr
		}
		httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		httpapi.WriteHTTPError(w, "invalid session request", http.StatusBadRequest)
		return
	}
	var req CreateHostedThreadRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		httpapi.WriteHTTPError(w, "invalid session request", http.StatusBadRequest)
		return
	}
	choices, err := subscriptions.NormalizeChoices(req.Subscriptions)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Subscriptions = choices
	req.ClientSessionID = strings.TrimSpace(req.ClientSessionID)
	legacyRoute := r.URL.Path == "/api/v1/agent-sessions"
	if legacyRoute && req.ClientSessionID == "" {
		httpapi.WriteHTTPError(w, "clientSessionId is required on the compatibility agent-sessions route", http.StatusBadRequest)
		return
	}
	if req.ClientSessionID != "" && !validAgentClientID(req.ClientSessionID) {
		httpapi.WriteHTTPError(w, "clientSessionId must be 1-200 URL-safe letters, numbers, or . _ : @ -", http.StatusBadRequest)
		return
	}
	resolved, err := s.resolveSpawn(req)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if resolved.Mode != "start" {
		httpapi.WriteHTTPError(w, "hosted thread creation requires mode start", http.StatusBadRequest)
		return
	}
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	req.InvokerThreadRef = strings.TrimSpace(req.InvokerThreadRef)
	if req.InvokerThreadRef == "" {
		httpapi.WriteHTTPError(w, "invokerThreadRef is required when an agent creates a hosted thread", http.StatusBadRequest)
		return
	}
	invoker, ok := s.resolveHostedThreadRecord(req.InvokerThreadRef)
	if !ok {
		httpapi.WriteHTTPError(w, "invokerThreadRef does not resolve to a hosted thread", http.StatusBadRequest)
		return
	}
	if invoker.Status == sessionStatusExited {
		httpapi.WriteHTTPError(w, "invokerThreadRef resolves to a closed hosted thread", http.StatusConflict)
		return
	}
	resolved.InvokerHostedThreadID = invoker.ID
	resolved.InvokerNativeThreadID = invoker.NativeThreadID
	resolved.InvokerProvider = invoker.Provider
	assignment, err := s.normalizeOrchestrationAssignment(req.Assignment)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Assignment = assignment
	// Idempotency follows caller-supplied parameters, not today's server
	// defaults. Otherwise a Nano restart with a different default model or cwd
	// would reject the same stable creation request before it could re-attach.
	hashRequest := req
	hashRequest.Provider = resolved.Provider
	hashRequest.Mode = resolved.Mode
	hashRequest.PresentationMode = resolved.PresentationMode
	// Reference aliases are transport spellings of one durable relationship,
	// not different creation parameters. Hash the normalized hosted identity so
	// a retry may use the parent's native id first and hosted id later.
	hashRequest.InvokerThreadRef = invoker.ID
	hashBody, _ := json.Marshal(hashRequest)
	record, created, err := s.agentExecutions.beginNamedSessionWithSubscriptionsAndPlacement(
		resolved.ClientSessionID, agentHash(hashBody), resolved.Provider, resolved.PresentationMode, resolved.SidebarPlacement, resolved.Name, invoker, choices, assignment,
	)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errClientSessionIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	// beginSessionWithInvoker committed hosted_threads and the normalized
	// execution row in one control.sqlite transaction before this point.  No
	// provider transport can observe an orphan execution record.
	if live := s.hostedSessionForRequest(record.ID); live != nil {
		httpapi.WriteJSON(w, s.agentSessionView(record.ID))
		return
	}
	if !created {
		if record.Status == sessionStatusExited {
			httpapi.WriteJSON(w, s.agentSessionView(record.ID))
			return
		}
		if record.NativeThreadID == "" {
			w.Header().Set("Content-Type", "application/json")
			status := http.StatusOK
			if record.Status == agentSessionCreating {
				status = http.StatusAccepted
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(s.agentSessionView(record.ID))
			return
		}
		resolved.Provider = record.Provider
		resolved.Mode = "resume"
		resolved.ThreadID = record.NativeThreadID
		resolved.HostedThreadID = record.ID
		resolved.PresentationMode = record.PresentationMode
	}
	resolved.InvokerHostedThreadID = record.InvokerHostedThreadID
	resolved.InvokerNativeThreadID = record.InvokerNativeThreadID
	resolved.InvokerProvider = record.InvokerProvider
	resolved.HostedThreadID = record.ID
	sess, err := s.spawnSession(resolved)
	if err != nil {
		_ = s.agentExecutions.updateSession(record.ID, "", record.NativeThreadID, sessionStatusExited, err.Error())
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s.agentSessionView(sess.hostedThreadID))
}

func (s *server) hostedThreadOperation(handler func(http.ResponseWriter, *http.Request, agentSessionRecord)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.agentExecutions == nil {
			message := "execution control unavailable; durable hosted mutations are disabled"
			if s.executionControlErr != "" {
				message += ": " + s.executionControlErr
			}
			httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
			return
		}
		ref := strings.TrimSpace(r.PathValue("threadRef"))
		if ref == "" {
			httpapi.WriteNotFound(w)
			return
		}
		record, ok := s.resolveHostedThreadRecord(ref)
		if !ok {
			httpapi.WriteHTTPError(w, "unknown hosted thread", http.StatusNotFound)
			return
		}
		handler(w, r, record)
	}
}

func (s *server) handleHostedThreadRead(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
	httpapi.WriteJSON(w, s.agentSessionView(record.ID))
}

func (s *server) handleHostedThreadDelete(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
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
	if err := s.agentExecutions.removeSession(record.ID); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.controlMetadata != nil {
		if err := s.controlMetadata.destroy(record.ID); err != nil {
			log.Printf("destroy canonical metadata %s: %v", record.ID, err)
		}
	}
	httpapi.WriteJSON(w, map[string]any{"id": record.ID, "deleted": true})
}

func (s *server) handleHostedThreadClose(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
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
	if err := s.agentExecutions.updateSession(record.ID, "", "", sessionStatusExited, "closed"); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpapi.WriteJSON(w, s.agentSessionView(record.ID))
}

func (s *server) canonicalMetadataAvailable(w http.ResponseWriter) bool {
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		message := "canonical metadata unavailable"
		if s.controlMetadataErr != "" {
			message += ": " + s.controlMetadataErr
		}
		httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *server) handleHostedThreadMetadataRead(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if !s.canonicalMetadataAvailable(w) {
		return
	}
	view, err := s.controlMetadata.view(record.ID)
	if err == sql.ErrNoRows {
		httpapi.WriteNotFound(w)
		return
	}
	if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	httpapi.WriteJSON(w, s.decorateHostedMetadata(view, s.hostedAttachmentState(record.ID)))
}

func (s *server) handleHostedThreadMetadataChanges(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if !s.canonicalMetadataAvailable(w) {
		return
	}
	rawAfter := r.URL.Query().Get("afterRevision")
	after := int64(0)
	var parseErr error
	if rawAfter != "" {
		after, parseErr = strconv.ParseInt(rawAfter, 10, 64)
	}
	if parseErr != nil || after < 0 {
		httpapi.WriteHTTPError(w, "afterRevision must be a non-negative integer", http.StatusBadRequest)
		return
	}
	view, err := s.controlMetadata.view(record.ID)
	if err == sql.ErrNoRows {
		httpapi.WriteNotFound(w)
		return
	}
	if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	changes := s.controlMetadata.changesAfter(record.ID, after)
	oldest := view.Revision
	if len(changes) > 0 {
		oldest = changes[0].Revision
	}
	refetch := after > view.Revision || (after < view.Revision && (len(changes) == 0 || after < oldest-1))
	httpapi.WriteJSON(w, map[string]any{"changes": changes, "currentRevision": view.Revision, "oldestRetainedRevision": oldest, "refetchRequired": refetch})
	return
}

func (s *server) handleHostedThreadMetadataEvents(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if !s.canonicalMetadataAvailable(w) {
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after = 0
		err = nil
	}
	limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
	if r.URL.Query().Get("limit") == "" {
		limit = 100
		limitErr = nil
	}
	filter := metadataEventFilter{
		Field:       strings.TrimSpace(r.URL.Query().Get("field")),
		Source:      strings.TrimSpace(r.URL.Query().Get("source")),
		Disposition: strings.TrimSpace(r.URL.Query().Get("disposition")),
		Freshness:   strings.TrimSpace(r.URL.Query().Get("freshness")),
	}
	if err != nil || after < 0 || limitErr != nil || limit < 1 || limit > 200 || !validMetadataEventFilter(filter) {
		httpapi.WriteHTTPError(w, "after/limit or metadata event filters are invalid", http.StatusBadRequest)
		return
	}
	if _, err := s.controlMetadata.view(record.ID); err == sql.ErrNoRows {
		httpapi.WriteNotFound(w)
		return
	} else if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	page, err := s.controlMetadata.eventsAfter(record.ID, after, limit, filter)
	if err != nil {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	httpapi.WriteJSON(w, page)
	return
}

func (s *server) handleHostedThreadPermissionEvents(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		httpapi.WriteHTTPError(w, "canonical metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after = 0
		err = nil
	}
	limit, limitErr := strconv.Atoi(r.URL.Query().Get("limit"))
	if r.URL.Query().Get("limit") == "" {
		limit = 100
		limitErr = nil
	}
	if err != nil || after < 0 || limitErr != nil || limit < 1 || limit > 200 {
		httpapi.WriteHTTPError(w, "after and limit must be valid non-negative pagination values", http.StatusBadRequest)
		return
	}
	page, err := s.controlMetadata.permissionEventsAfter(record.ID, after, limit)
	if err != nil {
		httpapi.WriteHTTPError(w, "permission events unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	httpapi.WriteJSON(w, page)
}

func (s *server) handleHostedThreadFamily(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
	ancestors, descendants := s.agentExecutions.lineageForHosted(record.ID)
	memberIDs := append([]string{record.ID}, descendants...)
	members := make([]executionFamilyMemberView, 0, len(memberIDs))
	for _, memberID := range memberIDs {
		member, found, err := s.agentExecutions.sessionFromCanonical(memberID)
		if err != nil {
			httpapi.WriteHTTPError(w, "durable family lineage unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if !found {
			httpapi.WriteHTTPError(w, "durable family lineage is incomplete", http.StatusServiceUnavailable)
			return
		}
		members = append(members, executionFamilyMemberView{HostedThreadID: member.ID, Provider: member.Provider,
			NativeThreadID: member.NativeThreadID, InvokerHostedThreadID: member.InvokerHostedThreadID,
			Status: member.Status, OperationIDs: s.agentExecutions.operationIDsForHosted(member.ID)})
	}
	httpapi.WriteJSON(w, map[string]any{"rootHostedThreadId": record.ID, "ancestorHostedThreadIds": ancestors, "members": members})
}

func (s *server) handleHostedThreadPresentation(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
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
	rec, _, changed, err := s.agentExecutions.setPresentation(record.ID, req.PresentationMode)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if changed {
		if sess := s.sessionForHostedThread(record.ID); sess != nil {
			sess.setPresentation(rec.PresentationMode)
		}
		s.notifyRuntime()
	}
	httpapi.WriteJSON(w, s.agentSessionView(record.ID))
}

func (s *server) handleHostedThreadSubscriptions(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
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
	_, _, _, err = s.agentExecutions.setSubscriptions(record.ID, choices)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	httpapi.WriteJSON(w, s.agentSessionView(record.ID))
}

func (s *server) handleHostedThreadSidebarPlacement(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	var req struct {
		SidebarPlacement string `json:"sidebarPlacement"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.SidebarPlacement) == "" {
		httpapi.WriteHTTPError(w, `body must be {"sidebarPlacement":"invoker"} or {"sidebarPlacement":"standalone"}`, http.StatusBadRequest)
		return
	}
	rec, _, changed, err := s.agentExecutions.setSidebarPlacement(record.ID, req.SidebarPlacement)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if changed {
		if sess := s.sessionForHostedThread(record.ID); sess != nil {
			sess.setSidebarPlacement(rec.SidebarPlacement)
		}
		// Placement changes are structural catalog changes, not runtime
		// overlay fields. Force every open browser to re-read the sidebar
		// graph so another tab cannot retain the old invoker edge.
		s.notifyRuntimeRefresh()
	}
	httpapi.WriteJSON(w, s.agentSessionView(record.ID))
}

func (s *server) handleHostedThreadAssignment(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
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
	if !validAgentClientID(clientActionID) {
		httpapi.WriteHTTPError(w, "clientActionId is required and must be URL-safe", http.StatusBadRequest)
		return
	}
	var requested *orchestrationAssignment
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
	retry, err := s.controlMetadata.orchestrationAssignmentActionExists(record.ID, clientActionID, assignment)
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
	event, err := s.controlMetadata.appendOrchestrationAssignment(record.ID, clientActionID, assignment)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errAssignmentIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	if s.runtimeEvents != nil {
		s.runtimeEvents.notify(runtimeDelta{Refresh: true})
	}
	httpapi.WriteJSON(w, assignmentMutationView{agentSessionView: s.agentSessionView(record.ID), AssignmentEventID: event.ID})
}

func (s *server) handleHostedThreadDetach(w http.ResponseWriter, _ *http.Request, record agentSessionRecord) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	// Detach never invents turn evidence: with an operation still open the
	// only honest terminal claims are "caller closed" or "process died",
	// and neither is true here. (A turn starting inside this check's race
	// window is settled as interrupted by the close below — narrow, and
	// acceptable under the localhost trust boundary.)
	if s.agentExecutions.hasNonTerminalTurns(record.ID) {
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
		httpapi.WriteJSON(w, s.agentSessionView(record.ID))
		return
	}
	if sess := s.sessionForHostedThread(record.ID); sess != nil {
		sess.close()
		s.removeSession(sess.id)
		s.notifyRuntime()
	}
	// The attachment record survives — that is the difference from close:
	// boot reattach and click-resume both restore a detached thread.
	_ = s.agentExecutions.updateSession(record.ID, "", "", agentSessionDetached, "")
	httpapi.WriteJSON(w, s.agentSessionView(record.ID))
}

func (s *server) handleHostedThreadConversation(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.handleConversationView(w, r, conversationViewMessages)
}

func (s *server) handleHostedThreadConversationEvents(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.handleConversationEventsView(w, r, conversationViewMessages)
}

func (s *server) handleHostedThreadInteraction(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.handleInteraction(w, r, r.PathValue("requestId"))
}

func (s *server) handleHostedThreadInterrupt(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	sess := s.hostedSessionForRequest(record.ID)
	if sess == nil {
		httpapi.WriteHTTPError(w, "hosted thread is not attached", http.StatusConflict)
		return
	}
	sess.handleInterrupt(w, r)
}

func validMetadataEventFilter(filter metadataEventFilter) bool {
	if filter.Field != "" && filter.Field != postureFieldModel && filter.Field != postureFieldEffort && filter.Field != postureFieldPermissionMode && filter.Field != postureFieldServiceTier && filter.Field != "cwd" && filter.Field != "subscriptions" && filter.Field != orchestrationAssignmentField {
		return false
	}
	if filter.Source != "" && filter.Source != postureSourceCreation && filter.Source != postureSourceOperator && filter.Source != postureSourceProvider && filter.Source != metadataSourceMigration {
		return false
	}
	if filter.Disposition != "" && filter.Disposition != metadataDispositionRequested && filter.Disposition != metadataDispositionConfirmed && filter.Disposition != metadataDispositionRejected && filter.Disposition != metadataDispositionCleared && filter.Disposition != metadataDispositionObserved {
		return false
	}
	return filter.Freshness == "" || filter.Freshness == metadataFreshnessLive || filter.Freshness == metadataFreshnessHistorical || filter.Freshness == metadataFreshnessReplayed
}

func validAgentClientID(value string) bool {
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

func (s *server) handleAgentTurnSubmit(w http.ResponseWriter, r *http.Request, hostedThreadID string) {
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
		origin = turnOriginAgent
	}
	if origin != turnOriginAgent && origin != turnOriginSystem {
		httpapi.WriteHTTPError(w, "origin must be agent or system", http.StatusBadRequest)
		return
	}
	if origin == turnOriginAgent && strings.HasPrefix(req.ClientActionID, "nano:") {
		httpapi.WriteHTTPError(w, "clientActionId prefix nano: is reserved for Nano", http.StatusBadRequest)
		return
	}
	if origin == turnOriginSystem && !strings.HasPrefix(req.ClientActionID, "nano:") {
		httpapi.WriteHTTPError(w, "system-origin clientActionId must use the reserved nano: prefix", http.StatusBadRequest)
		return
	}
	if req.RequireIdle && req.NextTurn {
		httpapi.WriteHTTPError(w, "requireIdle and nextTurn cannot be combined", http.StatusBadRequest)
		return
	}
	submission := hostedTurnSubmission{Prompt: req.Prompt, RequireIdle: req.RequireIdle, NextTurn: req.NextTurn, Origin: origin}
	ref := strings.TrimSpace(req.SenderThreadRef)
	if origin == turnOriginSystem {
		if ref != "" {
			writeSenderAdmissionError(w, "sender_forbidden", "senderThreadRef is not used for system-origin turn submissions")
			return
		}
		submission.Attribution = turnAttribution{Origin: turnOriginSystem}
		footer := senderFooter(submission.Attribution)
		if strings.HasPrefix(req.ClientActionID, heartbeatActionPrefix) {
			footer = heartbeatFooter
		}
		submission.Delivered = req.Prompt + "\n\n" + footer
	} else {
		if ref == "" {
			writeSenderAdmissionError(w, "sender_required", "senderThreadRef is required for agent-origin turn submissions")
			return
		}
		sender, ok := s.resolveHostedThreadRecord(ref)
		if !ok {
			writeSenderAdmissionError(w, "sender_not_hosted", "senderThreadRef does not resolve to a hosted thread")
			return
		}
		attribution, err := s.turnAttributionFor(sender)
		if err != nil {
			writeSenderAdmissionError(w, "sender_identity_incomplete", err.Error())
			return
		}
		submission.Attribution = attribution
		submission.Delivered = req.Prompt + "\n\n" + senderFooter(submission.Attribution)
	}
	record, existing, err := s.submitHostedTurnFull(hostedThreadID, req.ClientActionID, submission)
	if err != nil {
		if errors.Is(err, errUnreadFooterUnavailable) {
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
		httpapi.WriteJSON(w, s.agentTurnView(record))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	latest, _ := s.agentExecutions.turn(record.ID)
	_ = json.NewEncoder(w).Encode(s.agentTurnView(latest))
}

// writeSenderAdmissionError returns before submitHostedTurnFull. That boundary
// is intentional: rejected public agent submissions create no operation, queue
// entry, send attempt, or provider delivery.
func writeSenderAdmissionError(w http.ResponseWriter, code, detail string) {
	httpapi.WriteError(w, http.StatusBadRequest, code, detail)
}

// hostedTurnSubmission carries one agent-API prompt through admission.
// Prompt is the caller's text and is what the idempotency hash covers;
// Delivered is what the provider receives (Prompt plus Nano's sender footer).
// Origin and Attribution are persisted on the operation.
type hostedTurnSubmission struct {
	Prompt      string
	Delivered   string
	RequireIdle bool
	// NextTurn holds the prompt until the target's running turn ends.
	NextTurn    bool
	Origin      string
	Attribution turnAttribution
	CreateGuard agentTurnCreationGuard
}

// turnAttributionFor snapshots a complete sender identity at admission time.
// A public agent turn cannot proceed on partial identity: the persisted footer
// is immutable, so deferring a missing provider-native id would make a
// permanently untraceable message. A name is presentation, not identity; an
// unnamed but otherwise complete sender gets a deterministic label.
func (s *server) turnAttributionFor(sender agentSessionRecord) (turnAttribution, error) {
	provider := strings.TrimSpace(sender.Provider)
	native := strings.TrimSpace(sender.NativeThreadID)
	if sender.ID == "" || provider == "" || native == "" || nativeThreadIDProvisional(provider, native) {
		return turnAttribution{}, errors.New("senderThreadRef resolves to a hosted thread without a confirmed provider-native thread identity")
	}
	identity, err := s.resolveIdentity(context.Background(), sender.ID)
	if err != nil {
		return turnAttribution{}, fmt.Errorf("resolve sender identity: %w", err)
	}
	// Names and native identities must come from the same answer as the public
	// resolver. Refuse a changed binding rather than signing it with stale facts.
	if identity.MatchCount != 1 || identity.HostedThreadID != sender.ID || identity.Provider != provider || identity.NativeThreadID != native || identity.NativeThreadIDProvisional {
		return turnAttribution{}, errors.New("senderThreadRef does not resolve to the same confirmed hosted provider-native identity")
	}
	return turnAttribution{
		Origin: turnOriginAgent, SenderHostedThreadID: sender.ID, SenderNativeThreadID: native,
		SenderProvider: provider, SenderName: strings.TrimSpace(identity.DisplayName), SenderSubmittedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func senderPresentationName(a turnAttribution) string {
	if name := strings.TrimSpace(a.SenderName); name != "" {
		return name
	}
	provider, native := strings.TrimSpace(a.SenderProvider), strings.TrimSpace(a.SenderNativeThreadID)
	if provider == "" || native == "" {
		return ""
	}
	prefix := native
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return provider + " " + prefix + " (unnamed)"
}

// senderFooter is the one trailing line Nano appends to every agent-origin
// prompt so it never reads as a human message. Agent footers identify the
// sender and Nano's original UTC admission time, before any queue delay.
// Agent submissions reach this function only after complete attribution
// admission. System prompts retain their distinct Nano footer.
func senderFooter(a turnAttribution) string {
	if a.Origin == turnOriginSystem {
		return "[from nano]"
	}
	name := senderPresentationName(a)
	native := strings.TrimSpace(a.SenderNativeThreadID)
	submittedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(a.SenderSubmittedAt))
	if name == "" || native == "" || err != nil {
		return ""
	}
	return "[from " + name + " · " + native + " · submitted " + submittedAt.UTC().Format(time.RFC3339) + "]"
}

// persistedSenderFooter replays an operation's original attribution snapshot.
// Older operations have no recorded admission timestamp; retain their
// historical footer form rather than fabricating a new time during recovery.
// New public admissions always use senderFooter above and therefore always
// carry a submission timestamp.
func persistedSenderFooter(a turnAttribution) string {
	if footer := senderFooter(a); footer != "" {
		return footer
	}
	if a.Origin != turnOriginAgent || strings.TrimSpace(a.SenderSubmittedAt) != "" {
		return ""
	}
	name, native := strings.TrimSpace(a.SenderName), strings.TrimSpace(a.SenderNativeThreadID)
	if name == "" || native == "" {
		return ""
	}
	return "[from " + name + " · " + native + "]"
}

// submitHostedTurn is the single ordinary-turn path used by the public route
// and Nano's notification dispatcher. A persisted pre-delivery operation with
// no outbox or send-journal evidence is safe to enqueue again after a crash.
func (s *server) submitHostedTurn(hostedThreadID, actionID, prompt string) (agentTurnRecord, bool, error) {
	return s.submitHostedTurnWithOptions(hostedThreadID, actionID, prompt, false)
}

// submitHostedTurnWithOptions is for Nano-internal system actions only. Public
// agent submissions must enter through handleAgentTurnSubmit, which admits a
// complete sender before any operation can exist.
func (s *server) submitHostedTurnWithOptions(hostedThreadID, actionID, prompt string, requireIdle bool) (agentTurnRecord, bool, error) {
	return s.submitHostedTurnGuardedWithOptions(hostedThreadID, actionID, prompt, requireIdle, nil)
}

// submitHostedTurnGuardedWithOptions lets a Nano-internal caller validate the
// current hosted-operation set atomically with creation of its operation. The
// guard is never exposed by the public turn API.
func (s *server) submitHostedTurnGuardedWithOptions(hostedThreadID, actionID, prompt string, requireIdle bool, guard agentTurnCreationGuard) (agentTurnRecord, bool, error) {
	if !strings.HasPrefix(actionID, "nano:") {
		return agentTurnRecord{}, false, errors.New("agent-origin submissions require senderThreadRef through the public hosted-turn endpoint")
	}
	return s.submitHostedTurnFull(hostedThreadID, actionID, hostedTurnSubmission{
		Prompt: prompt, Delivered: prompt + "\n\n" + senderFooter(turnAttribution{Origin: turnOriginSystem}), RequireIdle: requireIdle, Origin: turnOriginSystem, Attribution: turnAttribution{Origin: turnOriginSystem}, CreateGuard: guard,
	})
}

type hostedTurnAdmissionError struct{ Reason string }

func (e *hostedTurnAdmissionError) Error() string { return "target is not idle: " + e.Reason }

func hostedTurnPromptHash(prompt string, requireIdle bool) string {
	if !requireIdle {
		return sendTextHash(prompt)
	}
	return sendTextHash(prompt + "\x00requireIdle")
}

func (s *server) submitHostedTurnFull(hostedThreadID, actionID string, sub hostedTurnSubmission) (agentTurnRecord, bool, error) {
	prompt, requireIdle := sub.Prompt, sub.RequireIdle
	if sub.Delivered == "" {
		sub.Delivered = prompt
	}
	var err error
	sub.Delivered, err = s.promptWithUnreadFooter(hostedThreadID, sub.Delivered)
	if err != nil {
		return agentTurnRecord{}, false, fmt.Errorf("%w: %v", errUnreadFooterUnavailable, err)
	}
	sub.Origin = string(prompts.NormalizeOrigin(prompts.Origin(sub.Origin)))
	sub.Attribution.Origin = sub.Origin
	hash := hostedTurnPromptHash(prompt, requireIdle)
	if sub.NextTurn {
		hash = turnRequestHash(prompt, true)
	}
	_, waited := s.boot.waitingHosted(hostedThreadID)
	s.reattachWaitingHosted(hostedThreadID)
	_, found := s.agentExecutions.turnForAction(hostedThreadID, actionID)
	if !found && s.sessionForHostedThread(hostedThreadID) == nil {
		if requireIdle {
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		}
		return agentTurnRecord{}, false, errHostedThreadNotAttached
	}
	if found {
		return s.submitExistingHostedTurn(hostedThreadID, actionID, sub, hash)
	}
	sess := s.sessionForHostedThread(hostedThreadID)
	if sess == nil {
		if requireIdle {
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		}
		return agentTurnRecord{}, false, errHostedThreadNotAttached
	}
	if requireIdle {
		sess.sendMu.Lock()
		defer sess.sendMu.Unlock()
		sess.stateMu.Lock()
		status, permission := sess.connectionStatusLocked(), sess.lifecyclePermissionLocked()
		sess.stateMu.Unlock()
		// A thread resting since a restart had no work open (bootWorkOpen) and
		// is attaching for this prompt, so it is idle: the prompt queues behind
		// the attach, as an idle thread's would start.
		restingIdle := waited == "resting" && status == "starting"
		switch {
		case status == sessionStatusExited:
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "detached"}
		case sess.nanoIsStopping() || status != "ready" && !restingIdle:
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "notReady"}
		case !restingIdle && (permission.Submit != lifecycle.SendNow || sess.corePromptInFlight()):
			return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "busy"}
		}
		if sess.queuedTurns != nil {
			if _, queued := sess.queuedTurns.first(sess.provider, sess.currentThreadID(), sess.hostedThreadID); queued {
				return agentTurnRecord{}, false, &hostedTurnAdmissionError{Reason: "queuedBacklog"}
			}
		}
		record, created, err := s.agentExecutions.beginTurnAttributedGuarded(hostedThreadID, actionID, hash, sub.Attribution, sub.CreateGuard)
		if err != nil {
			return agentTurnRecord{}, false, err
		}
		op, responseStatus, reason, err := sess.queueOrStartTurnSendLockedPreparedPresentation(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, false)
		return s.finishHostedTurnSubmission(sess, record, created, op, responseStatus, reason, err)
	}
	record, created, err := s.agentExecutions.beginTurnAttributedGuarded(hostedThreadID, actionID, hash, sub.Attribution, sub.CreateGuard)
	if err != nil {
		if errors.Is(err, errAgentTurnIdempotencyConflict) {
			return agentTurnRecord{}, false, errAgentTurnIdempotencyConflict
		}
		return agentTurnRecord{}, false, err
	}
	if !created && s.turnSubmissionDeliveryUnknown(record) {
		_ = s.agentExecutions.markTurnDelivery(record.ID, "", agentTurnDeliveryUnknown, &agentExecutionError{
			Source: "delivery", Code: "nano_restarted", Message: "Nano restarted after provider delivery began",
		})
		latest, _ := s.agentExecutions.turn(record.ID)
		return latest, true, nil
	}
	if !created && !s.needsTurnSubmissionReconciliation(record) {
		return record, true, nil
	}
	sess = s.sessionForHostedThread(hostedThreadID)
	if sess == nil {
		return record, !created, errHostedThreadNotAttached
	}
	op, responseStatus, reason, err := sess.queueOrStartTurnSendPreparedPresentation(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, sub.NextTurn)
	return s.finishHostedTurnSubmission(sess, record, created, op, responseStatus, reason, err)
}

func (s *server) submitExistingHostedTurn(hostedThreadID, actionID string, sub hostedTurnSubmission, hash string) (agentTurnRecord, bool, error) {
	record, created, err := s.agentExecutions.beginTurnAttributed(hostedThreadID, actionID, hash, sub.Attribution)
	if err == nil {
		// A retry of the same clientActionId must replay the text Nano already
		// persisted: the footer is never rewritten, and a thread rename between
		// retries must not trip the queue's text-hash guard.
		if record.Origin == turnOriginAgent {
			footer := persistedSenderFooter(turnAttribution{
				Origin: record.Origin, SenderHostedThreadID: record.SenderHostedThreadID,
				SenderNativeThreadID: record.SenderNativeThreadID, SenderProvider: record.SenderProvider,
				SenderName: record.SenderName, SenderSubmittedAt: record.SenderSubmittedAt,
			})
			if footer != "" {
				sub.Delivered = sub.Prompt + "\n\n" + footer
				var footerErr error
				sub.Delivered, footerErr = s.promptWithUnreadFooter(hostedThreadID, sub.Delivered)
				if footerErr != nil {
					return agentTurnRecord{}, false, fmt.Errorf("%w: %v", errUnreadFooterUnavailable, footerErr)
				}
			}
		}
		if sess := s.sessionForHostedThread(hostedThreadID); sess != nil {
			if queued, ok := sess.queuedPrompt(record.TransportActionID); ok && queued.Text != "" {
				sub.Delivered = queued.Text
			}
		}
	}
	if err != nil {
		return agentTurnRecord{}, false, err
	}
	if !created && s.turnSubmissionDeliveryUnknown(record) {
		_ = s.agentExecutions.markTurnDelivery(record.ID, "", agentTurnDeliveryUnknown, &agentExecutionError{Source: "delivery", Code: "nano_restarted", Message: "Nano restarted after provider delivery began"})
		latest, _ := s.agentExecutions.turn(record.ID)
		return latest, true, nil
	}
	if !created && !s.needsTurnSubmissionReconciliation(record) {
		return record, true, nil
	}
	sess := s.sessionForHostedThread(hostedThreadID)
	if sess == nil {
		return record, true, errHostedThreadNotAttached
	}
	op, responseStatus, reason, err := sess.queueOrStartTurnSendPreparedPresentation(record.TransportActionID, sub.Delivered, sub.Prompt, hash, sub.Origin, sub.NextTurn)
	return s.finishHostedTurnSubmission(sess, record, created, op, responseStatus, reason, err)
}

func (s *server) finishHostedTurnSubmission(sess *session, record agentTurnRecord, created bool, op *turnSendOperation, responseStatus int, reason string, err error) (agentTurnRecord, bool, error) {
	if err != nil {
		_ = s.agentExecutions.markTurnDelivery(record.ID, "", agentTurnFailed, &agentExecutionError{Source: "delivery", Message: err.Error()})
		sess.notifyRuntime()
		return record, false, err
	}
	if responseStatus == http.StatusAccepted && reason == "queued_for_next_turn" {
		_ = s.agentExecutions.markTurnDelivery(record.ID, "", agentTurnQueued, nil)
	} else if responseStatus != 0 || op == nil {
		state := agentTurnDeliveryUnknown
		if responseStatus >= http.StatusBadRequest {
			state = agentTurnFailed
		}
		code := "existing_delivery_state"
		if reason == "reconnect_failed" {
			code = reason
		}
		_ = s.agentExecutions.markTurnDelivery(record.ID, "", state, &agentExecutionError{Source: "delivery", Code: code, Message: reason})
		if state == agentTurnFailed {
			sess.notifyRuntime()
		}
	}
	latest, _ := s.agentExecutions.turn(record.ID)
	return latest, !created, nil
}

var errHostedThreadNotAttached = errors.New("hosted thread is not attached")

func (s *server) needsTurnSubmissionReconciliation(record agentTurnRecord) bool {
	if record.Status != agentTurnDeliveryPending {
		return false
	}
	var queue sessionQueue = s.queuedTurns
	var attempts sessionJournal = s.sendAttempts
	if sess := s.sessionForHostedThread(record.HostedThreadID); sess != nil {
		if sess.queuedTurns != nil {
			queue = sess.queuedTurns
		}
		if sess.sendAttempts != nil {
			attempts = sess.sendAttempts
		}
	}
	if queue != nil {
		if _, ok := queue.get(record.TransportActionID); ok {
			return false
		}
	}
	if attempts != nil {
		if _, ok := attempts.get(record.TransportActionID); ok {
			return false
		}
	}
	return true
}

func (s *server) turnSubmissionDeliveryUnknown(record agentTurnRecord) bool {
	if record.Status != agentTurnDeliveryPending || record.RunID == s.agentExecutions.runID {
		return false
	}
	var attempts sessionJournal = s.sendAttempts
	if sess := s.sessionForHostedThread(record.HostedThreadID); sess != nil && sess.sendAttempts != nil {
		attempts = sess.sendAttempts
	}
	attempt, ok := attempts.get(record.TransportActionID)
	return ok && (attempt.Status == sendAttemptSending || attempt.Status == sendAttemptAccepted)
}

func (s *server) turnOperationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.agentExecutions == nil {
		httpapi.WriteHTTPError(w, errAgentExecutionUnavailable.Error(), http.StatusServiceUnavailable)
		return "", false
	}
	id := strings.TrimSpace(r.PathValue("operationId"))
	if id == "" {
		httpapi.WriteNotFound(w)
		return "", false
	}
	return id, true
}

func (s *server) handleTurnOperationRead(w http.ResponseWriter, r *http.Request) {
	id, ok := s.turnOperationID(w, r)
	if !ok {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("afterRevision"), 10, 64)
	waitMS, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > 30000 {
		waitMS = 30000
	}
	rec, ok := s.agentExecutions.waitTurn(r.Context(), id, after, time.Duration(waitMS)*time.Millisecond)
	if !ok {
		httpapi.WriteHTTPError(w, "unknown turn operation", http.StatusNotFound)
		return
	}
	httpapi.WriteJSON(w, s.agentTurnView(rec))
}

func (s *server) handleTurnOperationCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.turnOperationID(w, r)
	if ok {
		s.cancelQueuedAgentTurn(w, id)
	}
}

// handleTurnOperationChanges is the composite coordination pull: one call
// answers "did any of my workers change state" across every operation, with
// an optional invoker filter and the same detached posture as single-op
// polling — waitMs is explicit foreground waiting, never the default loop.
func (s *server) handleTurnOperationChanges(w http.ResponseWriter, r *http.Request) {
	waitMS, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > 30000 {
		waitMS = 30000
	}
	var keep func(agentOperationTransition) bool
	if invokerRef := strings.TrimSpace(r.URL.Query().Get("invoker")); invokerRef != "" {
		invoker, ok := s.resolveHostedThreadRecord(invokerRef)
		if !ok {
			httpapi.WriteHTTPError(w, "invoker does not resolve to a hosted thread", http.StatusBadRequest)
			return
		}
		keep = func(t agentOperationTransition) bool { return t.InvokerHostedThreadID == invoker.ID }
	}
	response := s.agentExecutions.waitChanges(
		r.Context(), r.URL.Query().Get("since"), time.Duration(waitMS)*time.Millisecond, keep,
	)
	httpapi.WriteJSON(w, response)
}

// handleTurnOperationsCollection serves bulk status: GET ?ids=op1,op2 returns
// each operation's current view, with {id, missing: true} for ids the store
// has pruned or never held — a changes-feed cursor may reference operations
// that no longer exist, and one stale id must not fail the batch.
func (s *server) handleTurnOperationsCollection(w http.ResponseWriter, r *http.Request) {
	if s.agentExecutions == nil {
		httpapi.WriteHTTPError(w, errAgentExecutionUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	const maxBulkOperationIDs = 64
	raw := strings.Split(r.URL.Query().Get("ids"), ",")
	ids := make([]string, 0, len(raw))
	for _, id := range raw {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		httpapi.WriteHTTPError(w, "ids is required: a comma-separated list of operation ids", http.StatusBadRequest)
		return
	}
	if len(ids) > maxBulkOperationIDs {
		httpapi.WriteHTTPError(w, fmt.Sprintf("at most %d ids per request", maxBulkOperationIDs), http.StatusBadRequest)
		return
	}
	items := make([]any, 0, len(ids))
	for _, id := range ids {
		if rec, ok := s.agentExecutions.turn(id); ok {
			items = append(items, s.agentTurnView(rec))
		} else {
			items = append(items, map[string]any{"id": id, "missing": true})
		}
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}

func (s *server) cancelQueuedAgentTurn(w http.ResponseWriter, operationID string) {
	rec, ok := s.agentExecutions.turn(operationID)
	if !ok {
		httpapi.WriteHTTPError(w, "unknown turn operation", http.StatusNotFound)
		return
	}
	if rec.Status != agentTurnQueued {
		httpapi.WriteHTTPError(w, "turn operation is not queued", http.StatusConflict)
		return
	}
	var err error
	if sess := s.sessionForHostedThread(rec.HostedThreadID); sess != nil {
		_, err = sess.cancelQueuedTurnAction(rec.TransportActionID, discardedByCaller)
	} else {
		_, err = cancelQueuedTurnStores(s.queuedTurns, s.agentExecutions, rec.TransportActionID, discardedByCaller)
	}
	if err != nil {
		status := http.StatusInternalServerError
		var refused *promptActionRefused
		if errors.Is(err, errQueuedTurnNotFound) || errors.Is(err, errQueuedTurnDelivering) || errors.Is(err, errPromptEnded) || errors.As(err, &refused) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	latest, _ := s.agentExecutions.turn(rec.ID)
	httpapi.WriteJSON(w, s.agentTurnView(latest))
}

func (s *server) agentSessionView(id string) agentSessionView {
	rec, _ := s.agentExecutions.session(id)
	view := agentSessionView{
		ID: rec.ID, ClientSessionID: rec.ClientSessionID,
		Provider: rec.Provider, PresentationMode: rec.PresentationMode, SidebarPlacement: rec.SidebarPlacement,
		NanoSessionID: rec.NanoSessionID, NativeThreadID: rec.NativeThreadID,
		InvokerHostedThreadID: rec.InvokerHostedThreadID,
		InvokerNativeThreadID: rec.InvokerNativeThreadID, InvokerProvider: rec.InvokerProvider,
		Subscriptions: storedSubscriptions(rec),
		Status:        rec.Status, Error: rec.Error, CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt, Revision: rec.Revision,
	}
	if sess := s.sessionForHostedThread(id); sess != nil {
		sum := sess.summary()
		view.NanoSessionID, view.NativeThreadID, view.Status = sum.ID, sum.ThreadID, sum.Status
		view.Error, view.TurnActive, view.Waiting = sum.Error, sum.TurnActive, sum.Waiting
		view.ThreadDiagnostics = sum.ThreadDiagnostics
		view.MetadataDegraded = sum.MetadataDegraded
		view.Attached = true
		view.Ready, view.Working = sess.readyAndWorking()
	} else if _, waiting := s.boot.waitingHosted(id); waiting != "" {
		view.Status = waiting
	}
	view.NativeThreadIDProvisional = nativeThreadIDProvisional(view.Provider, view.NativeThreadID)
	view.Attachment = s.hostedAttachmentState(id)
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		if metadata, err := s.controlMetadata.view(id); err == nil {
			view.Metadata = s.decorateHostedMetadata(metadata, view.Attachment)
		} else {
			view.MetadataUnavailable = true
		}
		if assignment, err := s.controlMetadata.currentOrchestrationAssignment(id); err == nil {
			view.Assignment = assignment
		} else {
			view.AssignmentUnavailable = true
		}
	} else {
		view.MetadataUnavailable = true
		view.AssignmentUnavailable = true
	}
	availability := s.availabilityFor(id, time.Now().UTC())
	view.Availability = availability.State
	view.AvailabilityReasons = availability.Reasons
	view.LastHumanMessageAt = availability.Facts.LastHumanMessageAt
	view.AttachmentState = availability.Facts.Attachment
	return view
}

// decorateHostedMetadata adds read-time joins that are deliberately outside
// immutable metadata events: attachment liveness and durable user-assigned
// name provenance.  Neither changes the hosted revision.
func (s *server) decorateHostedMetadata(metadata hostedMetadataView, attachment hostedAttachmentView) *hostedMetadataView {
	metadata.Attachment = attachment
	if name, ok := s.durableThreadNameRef(metadata.Provider, metadata.NativeThreadID); ok {
		metadata.Name = name
	} else if name, ok := s.durableThreadName(metadata.Provider, metadata.NativeThreadID); ok {
		// Compatibility for a catalog implementation that exposes the older
		// value-only seam. The production catalog implements ThreadNameRefFor.
		metadata.Name = hostedMetadataName{Value: name, Provenance: "manual"}
	}
	return &metadata
}

// hostedAttachmentState is a read-time join of process liveness and the
// normalized execution record. It deliberately does not write hosted_threads:
// attachment liveness is volatile and must not manufacture a metadata revision.
func (s *server) hostedAttachmentState(id string) hostedAttachmentView {
	if sess := s.sessionForHostedThread(id); sess != nil {
		sum := sess.summary()
		state := sum.Status
		if state == "" {
			state = "attached"
		}
		return hostedAttachmentView{State: state, Provenance: "live_session", Error: sum.Error}
	}
	if _, waiting := s.boot.waitingHosted(id); waiting != "" {
		return hostedAttachmentView{State: waiting, Provenance: "boot_reattach"}
	}
	if s.agentExecutions == nil {
		return hostedAttachmentView{State: "unavailable", Provenance: "execution_control", Error: "execution control unavailable"}
	}
	rec, ok := s.agentExecutions.session(id)
	if !ok {
		return hostedAttachmentView{State: "unavailable", Provenance: "execution_record", Error: "hosted execution record unavailable"}
	}
	state := rec.Status
	if state == "" {
		state = "detached"
	}
	return hostedAttachmentView{State: state, Provenance: "execution_record", Error: rec.Error}
}

// nativeThreadIDProvisional reports whether a recorded native id is a Claude
// shim stand-in (cs_<hex>, minted in cmd/claude-shim) rather than a provider
// id. The provisional handle survives in durable records when a thread never
// completes its first prompt, and a field named nativeThreadId must not pass
// one off as real (slice-1 design, ID-disambiguation directive).
func nativeThreadIDProvisional(provider, nativeID string) bool {
	if provisional := lifecycleProviders[provider].provisional; provisional != nil {
		return provisional(nativeID)
	}
	return false
}

func (s *server) agentTurnView(rec agentTurnRecord) agentTurnView {
	view := agentTurnView{
		ID: rec.ID, HostedThreadID: rec.HostedThreadID, ClientActionID: rec.ClientActionID,
		Provider: rec.Provider, NanoSessionID: rec.NanoSessionID, NativeThreadID: rec.NativeThreadID,
		ProviderTurnID: rec.ProviderTurnID, Status: rec.Status, Terminal: threadvalues.TurnTerminal(rec.Status),
		Error: rec.Error, FinalItemID: rec.FinalItemID, FinalResponse: rec.FinalResponse,
		FinalResponseTruncated: rec.FinalResponseTruncated, FinalResponseBytes: rec.FinalResponseBytes,
		Origin: rec.Origin, SenderHostedThreadID: rec.SenderHostedThreadID, SenderNativeThreadID: rec.SenderNativeThreadID,
		SenderProvider: rec.SenderProvider, SenderName: rec.SenderName, SenderSubmittedAt: rec.SenderSubmittedAt,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt, Revision: rec.Revision,
		Interactions: []pendingInteraction{},
	}
	var queue sessionQueue = s.queuedTurns
	if sess := s.sessionForHostedThread(rec.HostedThreadID); sess != nil {
		if sess.queuedTurns != nil {
			queue = sess.queuedTurns
		}
		view.NanoSessionID = sess.id
		if threadID := sess.currentThreadID(); threadID != "" {
			view.NativeThreadID = threadID
		}
		if !threadvalues.TurnTerminal(rec.Status) {
			for _, interaction := range sess.pendingInteractionList() {
				if interaction.TurnID == "" || rec.ProviderTurnID == "" || interaction.TurnID == rec.ProviderTurnID {
					view.Interactions = append(view.Interactions, interaction)
				}
			}
		}
	}
	if rec.Status == agentTurnQueued {
		view.QueuePosition = queue.position(rec.TransportActionID)
	}
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		assignment, err := s.controlMetadata.operationOrchestrationAssignment(rec.AssignmentEventID)
		if err != nil {
			view.AssignmentUnavailable = true
		} else {
			view.Assignment = assignment
		}
	} else if rec.AssignmentEventID != nil {
		view.AssignmentUnavailable = true
	}
	return view
}

// hostedSessionForRequest is sessionForHostedThread for a request that needs
// the thread live: a thread still starting after a restart is registered first.
func (s *server) hostedSessionForRequest(id string) *session {
	s.reattachWaitingHosted(id)
	return s.sessionForHostedThread(id)
}

// resolveHostedThreadRecord accepts the durable hosted-thread id, an optional
// client creation alias, the current Nano session id, or the live provider
// thread id. All references converge on the same durable record before an
// operation is created.
func (s *server) resolveHostedThreadRecord(ref string) (agentSessionRecord, bool) {
	if record, ok := s.agentExecutions.resolveSession(ref); ok {
		return record, true
	}
	return s.resolveLiveHostedThreadRecord(ref, s.agentExecutions.session)
}

// closeHostedThreadForRecord ends a hosted thread's attachment whether or not
// a live session exists. Callers hold relationshipMu. The detached branch
// settles non-terminal operations explicitly — previously they dangled
// forever, because only a live session's close path ran settlement.
func (s *server) closeHostedThreadForRecord(record agentSessionRecord) error {
	if s.heartbeat == nil {
		return s.closeHostedThreadForRecordCore(record)
	}
	s.heartbeat.AttachmentMu.Lock()
	defer s.heartbeat.AttachmentMu.Unlock()
	heartbeatErr := s.heartbeat.PauseAttachmentForClosedTarget(record.ID, time.Now())
	return errors.Join(heartbeatErr, s.closeHostedThreadForRecordCore(record))
}

// closeHostedThreadForRecordCore closes only the provider attachment while
// callers hold any feature-specific lifecycle locks.
func (s *server) closeHostedThreadForRecordCore(record agentSessionRecord) error {
	if sess := s.sessionForHostedThread(record.ID); sess != nil {
		return s.closeAgentSession(sess)
	}
	if s.attachments != nil && record.NativeThreadID != "" {
		if attachment, ok := s.attachments.get(record.Provider, record.NativeThreadID); ok {
			if attachment.HostedThreadID != "" && attachment.HostedThreadID != record.ID {
				return errors.New("attachment belongs to another hosted thread; close refused")
			}
			if err := s.closeDetachedAttachment(attachment); err != nil {
				return err
			}
		}
	}
	if err := s.agentExecutions.markSessionTurns(record.ID, agentTurnInterrupted, &agentExecutionError{
		Source: "client", Code: "session_closed",
		Message: "the hosted thread was closed while detached",
	}); err != nil {
		return err
	}
	// Persist the terminal state inside the heartbeat lifecycle lock. The
	// public close and bulk-close handlers repeat this update with their own
	// reason, but doing it here prevents a concurrent attachment from slipping
	// into the detached-close gap.
	return s.agentExecutions.updateSession(record.ID, "", "", sessionStatusExited, "closed")
}

// dropQueuedTurns removes undelivered queued prompts for the thread: an
// operation settled as interrupted must not redeliver its prompt on a later
// reattachment.
func (s *server) dropQueuedTurns(record agentSessionRecord) error {
	if s.queuedTurns == nil {
		return nil
	}
	for {
		rec, ok := s.queuedTurns.first(record.Provider, record.NativeThreadID, record.ID)
		if !ok {
			return nil
		}
		if rec.ClientActionID == "" {
			return nil
		}
		if _, err := cancelQueuedTurnStores(s.queuedTurns, s.agentExecutions, rec.ClientActionID, discardedByCaller); err != nil {
			return err
		}
	}
}

func (s *server) closeAgentSession(sess *session) error {
	if sess == nil {
		return nil
	}
	var attachment attachmentRecord
	var remembered bool
	if s.attachments != nil {
		attachment, remembered = s.attachments.get(sess.provider, sess.currentThreadID())
	}
	if remembered && sess.provider == "claude" {
		if c := sess.claudeHandle(); c != nil {
			if tr, ok := c.tr.(*claudeShimTransport); ok && attachment.ShimSessionID != tr.shimSessionID {
				return errors.New("Claude attachment changed; close refused")
			}
		}
	}
	err := sess.closeWithResult()
	if !sess.isClosed() {
		return err
	}
	s.removeCurrentSession(sess)
	defer s.notifyRuntime()
	if err != nil {
		return fmt.Errorf("Claude stop unconfirmed; attachment retained for retry: %w", err)
	}
	if remembered {
		if err := s.attachments.forgetClosed(attachment); err != nil {
			return err
		}
	}
	sess.confirmClaudeClose()
	return nil
}

func providerTurnIDFromStart(result json.RawMessage) string {
	var body struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(result, &body) != nil {
		return ""
	}
	return body.Turn.ID
}

func providerEventIDFromStart(result json.RawMessage) string {
	var body struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
	}
	if json.Unmarshal(result, &body) != nil {
		return ""
	}
	return body.Event.ID
}

func deliveryExecutionError(err error) *agentExecutionError {
	if err == nil {
		return nil
	}
	code := "delivery_unknown"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "delivery_timeout"
	} else {
		var rpcErr *protocol.RPCError
		if errors.As(err, &rpcErr) {
			code = "delivery_refused"
		}
	}
	return &agentExecutionError{Source: "delivery", Code: code, Message: err.Error()}
}
