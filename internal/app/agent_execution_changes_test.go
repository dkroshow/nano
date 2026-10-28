package app

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"testing"
)

// Bulk status answers every id it is asked about: pruned or unknown ids come
// back {id, missing: true} rather than failing the batch.
func TestBulkTurnOperationStatusToleratesMissingIDs(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	hosted, _, _ := fixtureBeginSession(store, "caller", "hash", "claude", presentationBackground)
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt-hash")
	s := &server{Registry: newSessionRegistry(), agentExecutions: store}

	rec := httptest.NewRecorder()
	s.handleTurnOperationsCollection(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/turn-operations?ids="+op.ID+",op-gone", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
	var live struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body.Items[0], &live); err != nil || live.ID != op.ID || live.Status == "" {
		t.Fatalf("live item = %s (err %v), want the operation view", body.Items[0], err)
	}
	var gone struct {
		ID      string `json:"id"`
		Missing bool   `json:"missing"`
	}
	if err := json.Unmarshal(body.Items[1], &gone); err != nil || gone.ID != "op-gone" || !gone.Missing {
		t.Fatalf("missing item = %s (err %v), want {id, missing:true}", body.Items[1], err)
	}

	rec = httptest.NewRecorder()
	s.handleTurnOperationsCollection(rec, httptest.NewRequest(http.MethodGet, "/api/v1/turn-operations", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no ids = %d, want 400", rec.Code)
	}
}

// The changes endpoint rejects an unresolvable invoker filter instead of
// silently returning an unfiltered feed.
func TestTurnOperationChangesRejectsUnknownInvoker(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	s := &server{Registry: newSessionRegistry(), agentExecutions: store}
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/turn-operations/changes?invoker=nobody", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown invoker = %d, want 400", rec.Code)
	}
}
