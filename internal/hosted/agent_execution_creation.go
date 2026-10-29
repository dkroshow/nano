package hosted

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// HandleCreate admits a hosted thread before starting its provider attachment.
func (s *Controller) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if s.Executions == nil {
		message := "execution control unavailable; durable hosted mutations are disabled"
		if s.ExecutionError != "" {
			message += ": " + s.ExecutionError
		}
		httpapi.WriteHTTPError(w, message, http.StatusServiceUnavailable)
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		httpapi.WriteHTTPError(w, "invalid session request", http.StatusBadRequest)
		return
	}
	var req CreationRequest
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
	if req.ClientSessionID != "" && !ValidClientID(req.ClientSessionID) {
		httpapi.WriteHTTPError(w, "clientSessionId must be 1-200 URL-safe letters, numbers, or . _ : @ -", http.StatusBadRequest)
		return
	}
	resolved, err := s.AttachmentsControl.prepareCreation(req)
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
	invoker, ok := s.ResolveHostedRecord(req.InvokerThreadRef)
	if !ok {
		httpapi.WriteHTTPError(w, "invokerThreadRef does not resolve to a hosted thread", http.StatusBadRequest)
		return
	}
	if invoker.Status == hostedstore.StatusExited {
		httpapi.WriteHTTPError(w, "invokerThreadRef resolves to a closed hosted thread", http.StatusConflict)
		return
	}
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
	record, created, err := s.Executions.BeginNamedSessionWithSubscriptionsAndPlacement(
		resolved.ClientSessionID, hostedstore.AgentHash(hashBody), resolved.Provider, resolved.PresentationMode, resolved.SidebarPlacement, resolved.Name, invoker, choices, assignment,
	)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, hostedstore.ErrClientSessionIdempotencyConflict) {
			status = http.StatusConflict
		}
		httpapi.WriteHTTPError(w, err.Error(), status)
		return
	}
	// beginSessionWithInvoker committed hosted_threads and the normalized
	// execution row in one control.sqlite transaction before this point.  No
	// provider transport can observe an orphan execution record.
	s.AttachmentsControl.reattachWaitingHosted(record.ID)
	if live := s.sessionForHosted(record.ID); live != nil {
		httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
		return
	}
	if !created {
		if record.Status == hostedstore.StatusExited {
			httpapi.WriteJSON(w, s.Views().SessionView(record.ID))
			return
		}
		if record.NativeThreadID == "" {
			w.Header().Set("Content-Type", "application/json")
			status := http.StatusOK
			if record.Status == ownedthreads.AgentSessionCreating {
				status = http.StatusAccepted
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(s.Views().SessionView(record.ID))
			return
		}
	}
	attachedID, err := resolved.Attach(record, !created)
	if err != nil {
		_ = s.Executions.UpdateSession(record.ID, "", record.NativeThreadID, hostedstore.StatusExited, err.Error())
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s.Views().SessionView(attachedID))
}
