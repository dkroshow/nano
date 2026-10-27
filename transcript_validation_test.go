package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func TestTranscriptRepairPriorityUsesFreshVisibleValidation(t *testing.T) {
	visible := true
	store := newTranscriptValidationStore(10)
	if !store.put(transcriptValidationSnapshot{
		SnapshotID: "tab:1", TabID: "tab", PaneID: "pane", Surface: "transcript", ThreadID: "native-open", Status: "match",
		PageVisible: &visible, SurfaceVisible: &visible,
	}) {
		t.Fatal("validation put failed")
	}
	server := &server{transcriptValidations: store, sessions: map[string]*session{
		"attached": {provider: "codex", threadID: "native-attached"},
	}}
	if got := server.transcriptRepairPriority(history.TranscriptParserBackfillCandidate{Provider: history.ProviderCodex, NativeThreadID: "native-open"}); got != 0 {
		t.Fatalf("open priority=%d", got)
	}
	if got := server.transcriptRepairPriority(history.TranscriptParserBackfillCandidate{Provider: history.ProviderCodex, NativeThreadID: "native-attached"}); got != 1 {
		t.Fatalf("attached priority=%d", got)
	}
	if got := server.transcriptRepairPriority(history.TranscriptParserBackfillCandidate{Provider: history.ProviderCodex, NativeThreadID: "native-other"}); got != 2 {
		t.Fatalf("ordinary priority=%d", got)
	}
}

func TestTranscriptValidationsExposeLatestBrowserObservedRowsPerPane(t *testing.T) {
	s := &server{transcriptValidations: newTranscriptValidationStore(10)}
	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/transcript-validations", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		s.handleTranscriptValidationRecord(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	post(`{"snapshotId":"tab-1:1","checkedAt":"2026-08-12T13:00:00Z","tabId":"tab-1","pageId":"page-1","build":"index-one.js","paneId":"pane-1","threadId":"thread-1","pageVisible":true,"status":"mismatch","expectedRows":[{"key":"live:user-1","source":"live","id":"user-1","semanticId":"nit_user_1","type":"userMessage","expectsContent":true}],"mountedRows":[]}`)
	post(`{"snapshotId":"tab-1:2","checkedAt":"2026-08-12T13:00:01Z","tabId":"tab-1","pageId":"page-1","build":"index-two.js","paneId":"pane-1","threadId":"thread-1","pageVisible":true,"status":"match","expectedRows":[{"key":"live:user-1","source":"live","id":"user-1","semanticId":"nit_user_1","type":"userMessage","expectsContent":true}],"mountedRows":[{"key":"live:user-1","source":"live","id":"user-1","semanticId":"nit_user_1","type":"userMessage","hasContent":true}]}`)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/transcript-validations?threadId=thread-1", nil)
	rec := httptest.NewRecorder()
	s.handleTranscriptValidationsList(rec, req)
	var got struct {
		Validations []transcriptValidationSnapshot `json:"validations"`
		History     []transcriptValidationSnapshot `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Validations) != 1 || got.Validations[0].Status != "match" || got.Validations[0].Fresh || got.Validations[0].Build != "index-two.js" || len(got.Validations[0].MountedRows) != 1 {
		t.Fatalf("validations = %#v", got.Validations)
	}
	if len(got.History) != 2 || got.History[0].Status != "match" || got.History[1].Status != "mismatch" {
		t.Fatalf("history = %#v", got.History)
	}
	if got.Validations[0].ReceivedAt == "" {
		t.Fatal("receivedAt is empty")
	}
	if got.Validations[0].MountedRows[0].SemanticID != "nit_user_1" {
		t.Fatalf("semantic browser identity = %#v", got.Validations[0].MountedRows)
	}
}

func TestTranscriptValidationHistoryIgnoresHeartbeatsAndPrefersMismatches(t *testing.T) {
	store := newTranscriptValidationStore(10)
	snapshot := transcriptValidationSnapshot{
		SnapshotID: "tab:1", TabID: "tab", PaneID: "pane", Surface: "sidebar", Status: "match",
		ExpectedRows: []transcriptValidationRow{{Key: "sidebar:one", Source: "sidebar", ID: "one", Type: "thread"}},
		MountedRows:  []transcriptValidationRow{{Key: "sidebar:one", Source: "sidebar", ID: "one", Type: "thread"}},
	}
	if !store.put(snapshot) {
		t.Fatal("initial put failed")
	}
	for index := 0; index < 25; index++ {
		snapshot.SnapshotID = "tab:heartbeat-" + strconv.Itoa(index)
		if !store.put(snapshot) {
			t.Fatal("heartbeat put failed")
		}
	}
	if len(store.history[transcriptValidationPaneKey(snapshot)]) != 1 {
		t.Fatalf("heartbeat history = %d, want 1", len(store.history[transcriptValidationPaneKey(snapshot)]))
	}
	for index := 0; index < 25; index++ {
		snapshot.SnapshotID = "tab:change-" + strconv.Itoa(index)
		snapshot.Status = "match"
		snapshot.ViewMode = "view-" + strconv.Itoa(index)
		if index%4 == 0 {
			snapshot.Status = "mismatch"
		}
		store.put(snapshot)
	}
	history := store.history[transcriptValidationPaneKey(snapshot)]
	if len(history) != maxTranscriptValidationHistory {
		t.Fatalf("history = %d, want %d", len(history), maxTranscriptValidationHistory)
	}
	mismatches := 0
	for _, entry := range history {
		if entry.snapshot.Status == "mismatch" {
			mismatches++
		}
	}
	if mismatches != 7 {
		t.Fatalf("mismatches retained = %d, want 7", mismatches)
	}
	if store.bytes > maxTranscriptValidationStoreBytes {
		t.Fatalf("store bytes = %d, max %d", store.bytes, maxTranscriptValidationStoreBytes)
	}
}

func TestTranscriptValidationHistoryReturnsRecoveredMismatchByStatus(t *testing.T) {
	store := newTranscriptValidationStore(10)
	snapshot := transcriptValidationSnapshot{
		SnapshotID: "tab:1", TabID: "tab", PaneID: "pane", Surface: "transcript", Status: "mismatch",
		ExpectedRows: []transcriptValidationRow{{Key: "live:one", Source: "live", ID: "one", Type: "userMessage"}},
	}
	store.put(snapshot)
	snapshot.SnapshotID = "tab:2"
	snapshot.Status = "match"
	snapshot.MountedRows = []transcriptValidationRow{{Key: "live:one", Source: "live", ID: "one", Type: "userMessage"}}
	store.put(snapshot)

	latest, history := store.list(10, "", "", "", "", "mismatch")
	if len(latest) != 0 || len(history) != 1 || history[0].Status != "mismatch" {
		t.Fatalf("latest = %#v, history = %#v", latest, history)
	}
}

func TestTranscriptValidationRejectsContentAndInvalidRowIdentity(t *testing.T) {
	s := &server{transcriptValidations: newTranscriptValidationStore(10)}
	body := `{"snapshotId":"tab-1:1","tabId":"tab-1","paneId":"pane-1","status":"match","expectedRows":[{"key":"live:wrong","source":"live","id":"user-1","type":"userMessage","text":"secret"}],"mountedRows":[]}`
	rec := httptest.NewRecorder()
	s.handleTranscriptValidationRecord(rec, httptest.NewRequest(http.MethodPost, "/api/v1/transcript-validations", bytes.NewBufferString(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestTranscriptValidationAcceptsGrokSplitMessageIdentity(t *testing.T) {
	store := newTranscriptValidationStore(10)
	row := transcriptValidationRow{
		Key:    "live:4cd567a3-366f-4ab1-9a7a-732fdfafdce5/agentMessage#2",
		Source: "live",
		ID:     "4cd567a3-366f-4ab1-9a7a-732fdfafdce5/agentMessage#2",
		Type:   "agentMessage",
	}
	snapshot := transcriptValidationSnapshot{
		SnapshotID: "tab:1",
		TabID:      "tab",
		PaneID:     "pane",
		Surface:    "transcript",
		Status:     "match",
		ExpectedRows: []transcriptValidationRow{
			row,
		},
		MountedRows: []transcriptValidationRow{
			row,
		},
	}
	if !store.put(snapshot) {
		t.Fatal("Grok split-message identity was rejected")
	}
}

func duplicateValidationBody(snapshotID, paneID, build string) string {
	return `{"snapshotId":"` + snapshotID + `","checkedAt":"2026-08-31T22:10:00Z","tabId":"tab-1","pageId":"page-1","build":"` + build + `","paneId":"` + paneID + `","surface":"transcript","provider":"codex","threadId":"native-duplicate-thread","hostedThreadId":"nht_duplicate_thread","pageVisible":true,"surfaceVisible":true,"status":"mismatch","expectedRows":[{"key":"live:row-1","source":"live","id":"row-1","semanticId":"semantic-1","type":"userMessage","expectsContent":true}],"mountedRows":[{"key":"live:row-1","source":"live","id":"row-1","semanticId":"semantic-1","type":"userMessage","hasContent":true},{"key":"live:row-1","source":"live","id":"row-1","semanticId":"semantic-1","type":"userMessage","hasContent":true}]}`
}

func postTranscriptValidationForTest(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleTranscriptValidationRecord(rec, httptest.NewRequest(http.MethodPost, "/api/v1/browser-validations", bytes.NewBufferString(body)))
	return rec
}

func TestTranscriptDuplicateIncidentPersistsOnceAcrossRepeatedObservations(t *testing.T) {
	control := newTestControlMetadata(t)
	s := &server{controlMetadata: control, transcriptValidations: newTranscriptValidationStore(10), mailEvents: newMailHub()}

	first := postTranscriptValidationForTest(t, s, duplicateValidationBody("tab-1:1", "pane-1", "index-first.js"))
	if first.Code != http.StatusOK {
		t.Fatalf("first duplicate POST = %d %s", first.Code, first.Body.String())
	}
	second := postTranscriptValidationForTest(t, s, duplicateValidationBody("tab-1:2", "pane-2", "index-second.js"))
	if second.Code != http.StatusOK {
		t.Fatalf("repeat duplicate POST = %d %s", second.Code, second.Body.String())
	}

	incidents, err := control.listBrowserDuplicateIncidents(browserDuplicateIncidentFilter{ThreadID: "native-duplicate-thread", Limit: 10})
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents = %+v, err=%v", incidents, err)
	}
	incident := incidents[0]
	if incident.ObservationCount != 2 ||
		incident.RawRenderKeyFingerprint == "" || incident.SemanticIDFingerprint == "" ||
		incident.FirstBuild != "index-first.js" || incident.LastBuild != "index-second.js" ||
		incident.FirstPaneID != "pane-1" || incident.LastPaneID != "pane-2" {
		t.Fatalf("incident = %+v", incident)
	}
	encoded, _ := json.Marshal(incident)
	if strings.Contains(string(encoded), "row-1") || strings.Contains(string(encoded), "semantic-1") {
		t.Fatalf("durable incident retained raw row identity: %s", encoded)
	}

}

func TestTranscriptDuplicateFingerprintsRawAndSemanticIdentitySeparately(t *testing.T) {
	base := transcriptValidationSnapshot{
		CheckedAt: "2026-08-31T22:10:00Z", Build: "index-test.js", PaneID: "pane-1",
		Provider: "codex", ThreadID: "native-thread",
	}
	raw := base
	raw.MountedRows = []transcriptValidationRow{
		{Key: "live:one", Source: "live", ID: "one", Type: "userMessage"},
		{Key: "live:one", Source: "live", ID: "one", Type: "userMessage"},
	}
	rawIncident, ok := deriveBrowserDuplicateIncident(raw, time.Now())
	if !ok || rawIncident.RawRenderKeyFingerprint == "" || rawIncident.SemanticIDFingerprint != "" {
		t.Fatalf("raw-only incident = %+v, ok=%v", rawIncident, ok)
	}
	semantic := base
	semantic.MountedRows = []transcriptValidationRow{
		{Key: "durable:one", Source: "durable", ID: "one", SemanticID: "canonical-one", Type: "userMessage"},
		{Key: "live:two", Source: "live", ID: "two", SemanticID: "canonical-one", Type: "userMessage"},
	}
	semanticIncident, ok := deriveBrowserDuplicateIncident(semantic, time.Now())
	if !ok || semanticIncident.RawRenderKeyFingerprint != "" || semanticIncident.SemanticIDFingerprint == "" {
		t.Fatalf("semantic-only incident = %+v, ok=%v", semanticIncident, ok)
	}
	if rawIncident.IncidentFingerprint == semanticIncident.IncidentFingerprint {
		t.Fatal("raw-key and semantic-only incidents share a fingerprint")
	}
}

func TestTranscriptValidationNormalPathCreatesNoDuplicateIncident(t *testing.T) {
	control := newTestControlMetadata(t)
	s := &server{controlMetadata: control, transcriptValidations: newTranscriptValidationStore(10)}
	body := `{"snapshotId":"tab-1:normal","checkedAt":"2026-08-31T22:10:00Z","tabId":"tab-1","build":"index-normal.js","paneId":"pane-1","surface":"transcript","threadId":"native-normal-thread","pageVisible":true,"surfaceVisible":true,"status":"match","expectedRows":[{"key":"live:row-1","source":"live","id":"row-1","semanticId":"semantic-1","type":"userMessage","expectsContent":true}],"mountedRows":[{"key":"live:row-1","source":"live","id":"row-1","semanticId":"semantic-1","type":"userMessage","hasContent":true}]}`
	if rec := postTranscriptValidationForTest(t, s, body); rec.Code != http.StatusOK {
		t.Fatalf("normal POST = %d %s", rec.Code, rec.Body.String())
	}
	incidents, err := control.listBrowserDuplicateIncidents(browserDuplicateIncidentFilter{Limit: 10})
	if err != nil || len(incidents) != 0 {
		t.Fatalf("normal incidents = %+v, err=%v", incidents, err)
	}
}
