package hosted

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
)

// operationID validates that operation reads have an available store and a path ID.
func (s *ViewService) operationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.Executions == nil {
		httpapi.WriteHTTPError(w, hostedstore.ErrAgentExecutionUnavailable.Error(), http.StatusServiceUnavailable)
		return "", false
	}
	id := strings.TrimSpace(r.PathValue("operationId"))
	if id == "" {
		httpapi.WriteNotFound(w)
		return "", false
	}
	return id, true
}

// HandleOperationRead serves an operation, optionally waiting for its next revision.
func (s *ViewService) HandleOperationRead(w http.ResponseWriter, r *http.Request) {
	id, ok := s.operationID(w, r)
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
	rec, ok := s.Executions.WaitTurn(r.Context(), id, after, time.Duration(waitMS)*time.Millisecond)
	if !ok {
		httpapi.WriteHTTPError(w, "unknown turn operation", http.StatusNotFound)
		return
	}
	httpapi.WriteJSON(w, s.TurnView(rec))
}

// HandleOperationChanges is the composite coordination pull: one call
// answers "did any of my workers change state" across every operation, with
// an optional invoker filter and the same detached posture as single-op
// polling — waitMs is explicit foreground waiting, never the default loop.
func (s *ViewService) HandleOperationChanges(w http.ResponseWriter, r *http.Request) {
	waitMS, _ := strconv.Atoi(r.URL.Query().Get("waitMs"))
	if waitMS < 0 {
		waitMS = 0
	}
	if waitMS > 30000 {
		waitMS = 30000
	}
	var keep func(hostedstore.AgentOperationTransition) bool
	if invokerRef := strings.TrimSpace(r.URL.Query().Get("invoker")); invokerRef != "" {
		invoker, ok := s.ResolveRecord(invokerRef)
		if !ok {
			httpapi.WriteHTTPError(w, "invoker does not resolve to a hosted thread", http.StatusBadRequest)
			return
		}
		keep = func(t hostedstore.AgentOperationTransition) bool { return t.InvokerHostedThreadID == invoker.ID }
	}
	response := s.Executions.WaitChanges(
		r.Context(), r.URL.Query().Get("since"), time.Duration(waitMS)*time.Millisecond, keep,
	)
	httpapi.WriteJSON(w, response)
}

// HandleOperationsCollection serves bulk status: GET ?ids=op1,op2 returns
// each operation's current view, with {id, missing: true} for ids the store
// has pruned or never held — a changes-feed cursor may reference operations
// that no longer exist, and one stale id must not fail the batch.
func (s *ViewService) HandleOperationsCollection(w http.ResponseWriter, r *http.Request) {
	if s.Executions == nil {
		httpapi.WriteHTTPError(w, hostedstore.ErrAgentExecutionUnavailable.Error(), http.StatusServiceUnavailable)
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
		if rec, ok := s.Executions.Turn(id); ok {
			items = append(items, s.TurnView(rec))
		} else {
			items = append(items, map[string]any{"id": id, "missing": true})
		}
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}
