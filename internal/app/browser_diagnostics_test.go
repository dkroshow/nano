package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserDiagnosticsAcceptsDeduplicatesAndFilters(t *testing.T) {
	s := &server{browserDiagnostics: newBrowserDiagnosticStore(10)}
	body := []byte(`{"events":[
		{"eventId":"tab-1:1","kind":"projection.open","tabId":"tab-1","threadId":"thread-1","interactionId":"request-5","revision":7,"activeRenderSource":"messageItems","visibleItemCount":3,"matchingDOMActionRowCount":0,"expectedRowCount":4,"mountedRowCount":3,"missingRowCount":1,"extraRowCount":0,"duplicateRowCount":0,"semanticDuplicateRowCount":1,"blankRowCount":0,"typeMismatchCount":0,"orderMismatchCount":0,"arrivalRateCps":275,"batchIntervalMs":121,"layoutSequence":4,"rowIndex":0,"previousRowIndex":3,"topPx":0,"previousTopPx":120,"movementPx":120,"movementCount":7,"maxMovementPx":240,"reorderCount":5,"addedCount":1,"removedCount":2,"workingCount":3,"starredCount":1,"scrollTop":0,"scrollHeight":1200,"viewportHeight":640},
		{"eventId":"tab-1:1","kind":"projection.open","tabId":"tab-1","threadId":"thread-1","revision":7},
		{"eventId":"tab-2:1","kind":"pane.bind","tabId":"tab-2","threadId":"thread-2","sessionId":"session-2"}
	]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/browser-diagnostics", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleBrowserDiagnosticsRecord(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var posted struct {
		Accepted int    `json:"accepted"`
		BootID   string `json:"bootId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Accepted != 2 || posted.BootID == "" {
		t.Fatalf("POST response = %#v", posted)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/browser-diagnostics?threadId=thread-1&limit=10", nil)
	rec = httptest.NewRecorder()
	s.handleBrowserDiagnosticsList(rec, req)
	var got struct {
		Events []browserDiagnosticEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].EventID != "tab-1:1" || got.Events[0].InteractionID != "request-5" || got.Events[0].ArrivalRateCPS != 275 || got.Events[0].BatchIntervalMS != 121 || got.Events[0].ReceivedAt == "" {
		t.Fatalf("filtered events = %#v", got.Events)
	}
	if got.Events[0].ActiveRenderSource != "messageItems" || got.Events[0].VisibleItemCount == nil || *got.Events[0].VisibleItemCount != 3 ||
		got.Events[0].MatchingDOMActionRowCount == nil || *got.Events[0].MatchingDOMActionRowCount != 0 {
		t.Fatalf("render diagnostics = %#v", got.Events[0])
	}
	if got.Events[0].ExpectedRowCount == nil || *got.Events[0].ExpectedRowCount != 4 ||
		got.Events[0].MountedRowCount == nil || *got.Events[0].MountedRowCount != 3 ||
		got.Events[0].MissingRowCount == nil || *got.Events[0].MissingRowCount != 1 ||
		got.Events[0].ExtraRowCount == nil || *got.Events[0].ExtraRowCount != 0 {
		t.Fatalf("transcript validation diagnostics = %#v", got.Events[0])
	}
	if got.Events[0].SemanticDuplicateRowCount == nil || *got.Events[0].SemanticDuplicateRowCount != 1 {
		t.Fatalf("semantic duplicate diagnostics = %#v", got.Events[0])
	}
	if got.Events[0].LayoutSequence != 4 || got.Events[0].RowIndex == nil || *got.Events[0].RowIndex != 0 ||
		got.Events[0].PreviousRowIndex == nil || *got.Events[0].PreviousRowIndex != 3 ||
		got.Events[0].TopPX == nil || *got.Events[0].TopPX != 0 ||
		got.Events[0].PreviousTopPX == nil || *got.Events[0].PreviousTopPX != 120 ||
		got.Events[0].MovementPX != 120 || got.Events[0].MovementCount != 7 || got.Events[0].MaxMovementPX != 240 ||
		got.Events[0].ReorderCount != 5 || got.Events[0].AddedCount != 1 || got.Events[0].RemovedCount != 2 ||
		got.Events[0].WorkingCount != 3 || got.Events[0].StarredCount != 1 ||
		got.Events[0].ScrollTop != 0 || got.Events[0].ScrollHeight != 1200 || got.Events[0].ViewportHeight != 640 {
		t.Fatalf("sidebar layout diagnostics = %#v", got.Events[0])
	}
}

// The agent-readable API must distinguish missing measurements from a real zero.
func TestBrowserDiagnosticsThreadOpenTimingRoundTrip(t *testing.T) {
	s := &server{browserDiagnostics: newBrowserDiagnosticStore(10)}
	body := `{"events":[{"eventId":"open:1","kind":"thread_open","actionId":"open-one","trigger":"sidebar","view":"enhanced","firstRowsMs":0,"newestRowsMs":32,"historyMs":8,"historyBytes":1024,"outcome":"ready","warm":true},{"eventId":"open:2","kind":"thread_open","actionId":"open-two","trigger":"private-title","view":"private-text","outcome":"abandoned"}]}`
	recorded := httptest.NewRecorder()
	s.handleBrowserDiagnosticsRecord(recorded, httptest.NewRequest(http.MethodPost, "/api/v1/browser-diagnostics", bytes.NewBufferString(body)))
	if recorded.Code != http.StatusOK {
		t.Fatalf("record: %s", recorded.Body.String())
	}
	read := httptest.NewRecorder()
	s.handleBrowserDiagnosticsList(read, httptest.NewRequest(http.MethodGet, "/api/v1/browser-diagnostics?kind=thread_open", nil))
	var got struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events = %#v", got.Events)
	}
	for key, want := range map[string]float64{"firstRowsMs": 0, "newestRowsMs": 32, "historyMs": 8, "historyBytes": 1024} {
		if got.Events[0][key] != want {
			t.Fatalf("%s = %v", key, got.Events[0][key])
		}
		if _, exists := got.Events[1][key]; exists {
			t.Fatalf("missing %s became a measurement", key)
		}
	}
	if got.Events[0]["warm"] != true || got.Events[1]["warm"] != nil {
		t.Fatalf("a warm open must keep its flag and only it: %#v", got.Events)
	}
	if got.Events[0]["view"] != "enhanced" || got.Events[1]["view"] != nil {
		t.Fatalf("view normalization = %#v", got.Events)
	}
	if got.Events[0]["trigger"] != "sidebar" || got.Events[1]["trigger"] != nil {
		t.Fatalf("trigger normalization = %#v", got.Events)
	}
}
