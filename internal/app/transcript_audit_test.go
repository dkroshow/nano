package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

type fakeTranscriptAuditCatalog struct {
	fakeTranscriptCatalog
	reference           transcriptAuditReference
	referenceErr        error
	referenceThread     string
	referenceProjection string
	referenceHead       int64
	referenceMin        int64
	referenceMax        int64
	referenceScopes     []transcriptAuditScope
}

func (f *fakeTranscriptAuditCatalog) LoadTranscriptAuditReference(_ context.Context, threadID, projectionKey string, snapshotHead, minSeq, maxSeq int64, scopes []transcriptAuditScope) (transcriptAuditReference, error) {
	f.referenceThread, f.referenceProjection, f.referenceHead = threadID, projectionKey, snapshotHead
	f.referenceMin, f.referenceMax = minSeq, maxSeq
	f.referenceScopes = append([]transcriptAuditScope(nil), scopes...)
	return f.reference, f.referenceErr
}

func newTranscriptAuditTestServer() (*server, *fakeTranscriptAuditCatalog) {
	catalog := &fakeTranscriptAuditCatalog{
		fakeTranscriptCatalog: fakeTranscriptCatalog{response: transcriptResponse{
			ThreadID: "nth-one", Projection: transcriptProjection{Key: "projection-1", State: "ready", SourceStatus: "present"},
			Page: transcriptPageInfo{SnapshotHeadSeq: 3},
		}},
		reference: transcriptAuditReference{
			ProjectionKey: "projection-1", SnapshotHeadSeq: 3, StartSeq: 1, EndSeq: 3,
			Rows: []transcriptAuditReferenceRow{{ID: "a", ProjectedSeq: 1, Category: "messages", Role: "assistant", Text: "private-reference-body"}},
		},
	}
	return &server{catalog: catalog, transcriptAudits: newTranscriptAuditStore(), transcriptAuditEvents: newTranscriptAuditHub()}, catalog
}

func createTranscriptAuditForTest(t *testing.T, s *server) transcriptAuditRequest {
	t.Helper()
	body := `{"clientActionId":"action-1","threadRef":"nth-one","tabId":"tab-1","pageId":"page-1","paneId":"pane-1"}`
	recorder := httptest.NewRecorder()
	s.handleTranscriptAuditCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/transcript-audits", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("create = %d %s", recorder.Code, recorder.Body.String())
	}
	var request transcriptAuditRequest
	if err := json.Unmarshal(recorder.Body.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	return pinTranscriptAuditTestBuild(t, s, request)
}

// Handler tests pin a synthetic build independently of the embedded assets.
// Production still rejects missing or different browser builds.
func pinTranscriptAuditTestBuild(t *testing.T, s *server, request transcriptAuditRequest) transcriptAuditRequest {
	t.Helper()
	if request.ExpectedBuild != embeddedBundleName() {
		t.Fatalf("expected build = %q, want the embedded bundle %q", request.ExpectedBuild, embeddedBundleName())
	}
	request.ExpectedBuild = "index-audit-test.js"
	s.transcriptAudits.mu.Lock()
	s.transcriptAudits.requests[request.ID].ExpectedBuild = request.ExpectedBuild
	s.transcriptAudits.mu.Unlock()
	return request
}

func TestTranscriptAuditTargetsExactPaneAndPinsReference(t *testing.T) {
	s, catalog := newTranscriptAuditTestServer()
	events := s.transcriptAuditEvents.subscribe()
	defer s.transcriptAuditEvents.unsubscribe(events)
	request := createTranscriptAuditForTest(t, s)
	if request.Status != "pending" || request.CatalogThreadID != "nth-one" || !slices.Equal(request.Scopes, []transcriptAuditScope{transcriptAuditMessages}) || request.ReferenceProjectionKey != "projection-1" || request.ReferenceSnapshotHead != 3 {
		t.Fatalf("request = %#v", request)
	}
	select {
	case event := <-events:
		if event.AuditID != request.ID || event.TabID != "tab-1" || event.PageID != "page-1" || event.PaneID != "pane-1" || event.CatalogThreadID != "nth-one" {
			t.Fatalf("event = %#v", event)
		}
	default:
		t.Fatal("missing transcript audit browser event")
	}

	referenceRecorder := httptest.NewRecorder()
	referenceURL := "/api/v1/transcript-audits/" + request.ID + "/reference?minProjectedSeq=1&maxProjectedSeq=3"
	s.handleTranscriptAuditReference(referenceRecorder, httptest.NewRequest(http.MethodGet, referenceURL, nil), request.ID)
	if referenceRecorder.Code != http.StatusOK {
		t.Fatalf("reference = %d %s", referenceRecorder.Code, referenceRecorder.Body.String())
	}
	if catalog.referenceThread != "nth-one" || catalog.referenceProjection != "projection-1" || catalog.referenceHead != 3 || catalog.referenceMin != 1 || catalog.referenceMax != 3 {
		t.Fatalf("reference request = thread=%q projection=%q head=%d range=%d-%d", catalog.referenceThread, catalog.referenceProjection, catalog.referenceHead, catalog.referenceMin, catalog.referenceMax)
	}
}

func TestTranscriptAuditAcceptsContentFreeScopedMatch(t *testing.T) {
	s, _ := newTranscriptAuditTestServer()
	request := createTranscriptAuditForTest(t, s)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(map[string]any{
		"capturedAt": now, "tabId": "tab-1", "pageId": "page-1", "build": request.ExpectedBuild,
		"paneId": "pane-1", "paneThreadRef": "nth-one", "viewMode": "clean",
		"pageVisible": true, "surfaceVisible": true, "projectionKey": "projection-1", "snapshotHeadSeq": 3,
		"result": map[string]any{
			"status": "match", "complete": true, "scopes": []string{"messages"},
			"counts":      map[string]int{"reference": 1, "browser": 1, "contentChecked": 1},
			"differences": []any{}, "differencesTruncated": false, "exclusions": []any{}, "reasons": []any{},
			"range": map[string]any{"startSeq": 1, "endSeq": 1, "snapshotHeadSeq": 3, "partial": true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	s.handleTranscriptAuditResult(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)), request.ID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("result = %d %s", recorder.Code, recorder.Body.String())
	}
	stored, ok := s.transcriptAudits.get(request.ID)
	if !ok || stored.Status != "match" || stored.Result == nil || stored.Result.PageID != "page-1" || stored.Result.Counts.ContentChecked != 1 || !stored.Result.Range.Partial {
		t.Fatalf("stored = %#v", stored)
	}
	encoded, _ := json.Marshal(stored)
	if strings.Contains(string(encoded), "private-reference-body") {
		t.Fatalf("stored audit leaked reference body: %s", encoded)
	}
}

func TestTranscriptAuditRejectsRemovedScopes(t *testing.T) {
	for _, scope := range []string{"tools", "reasoning"} {
		t.Run(scope, func(t *testing.T) {
			s, _ := newTranscriptAuditTestServer()
			body := `{"clientActionId":"removed-scope","threadRef":"nth-one","tabId":"tab-1","pageId":"page-1","paneId":"pane-1","scopes":["` + scope + `"]}`
			recorder := httptest.NewRecorder()
			s.handleTranscriptAuditCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/transcript-audits", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("removed scope accepted: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestTranscriptAuditRejectsMalformedReferenceRange(t *testing.T) {
	s, _ := newTranscriptAuditTestServer()
	request := createTranscriptAuditForTest(t, s)
	recorder := httptest.NewRecorder()
	url := "/api/v1/transcript-audits/" + request.ID + "/reference?minProjectedSeq=not-a-number"
	s.handleTranscriptAuditReference(recorder, httptest.NewRequest(http.MethodGet, url, nil), request.ID)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("reference = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestTranscriptAuditRejectsIdempotencyConflict(t *testing.T) {
	s, _ := newTranscriptAuditTestServer()
	createTranscriptAuditForTest(t, s)
	body := `{"clientActionId":"action-1","threadRef":"nth-one","tabId":"tab-1","pageId":"different-page","paneId":"pane-1","scopes":["messages"]}`
	recorder := httptest.NewRecorder()
	s.handleTranscriptAuditCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/transcript-audits", strings.NewReader(body)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("create = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestTranscriptAuditWrongBuildAndChangedReferenceCannotPass(t *testing.T) {
	t.Run("wrong build", func(t *testing.T) {
		s, _ := newTranscriptAuditTestServer()
		request := createTranscriptAuditForTest(t, s)
		body := `{"capturedAt":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","tabId":"tab-1","pageId":"page-1","build":"index-stale.js","paneId":"pane-1","paneThreadRef":"nth-one","viewMode":"clean","pageVisible":true,"surfaceVisible":true,"projectionKey":"projection-1","snapshotHeadSeq":3,"result":{"status":"match","complete":true,"scopes":["messages"],"counts":{},"differences":[],"differencesTruncated":false,"exclusions":[],"reasons":[],"range":{}}}`
		recorder := httptest.NewRecorder()
		s.handleTranscriptAuditResult(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), request.ID)
		stored, _ := s.transcriptAudits.get(request.ID)
		if stored.Status != "inconclusive" || stored.Result == nil || stored.Result.Reasons[0] != "wrong_or_stale_build" {
			t.Fatalf("stored = %#v", stored)
		}
	})

	t.Run("changed reference", func(t *testing.T) {
		s, catalog := newTranscriptAuditTestServer()
		request := createTranscriptAuditForTest(t, s)
		catalog.referenceErr = history.ErrTranscriptChanged
		recorder := httptest.NewRecorder()
		s.handleTranscriptAuditReference(recorder, httptest.NewRequest(http.MethodGet, "/", nil), request.ID)
		stored, _ := s.transcriptAudits.get(request.ID)
		if recorder.Code != http.StatusConflict || stored.Status != "inconclusive" || stored.Result.Reasons[0] != "saved_reference_changed" {
			t.Fatalf("response=%d stored=%#v", recorder.Code, stored)
		}
	})
}

func TestTranscriptAuditWrongOrStaleBrowserCannotPass(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(map[string]any)
		wantReason string
	}{
		{name: "wrong page", mutate: func(body map[string]any) { body["pageId"] = "page-other" }, wantReason: "wrong_browser_pane"},
		{name: "wrong pane thread", mutate: func(body map[string]any) { body["paneThreadRef"] = "nth-other" }, wantReason: "wrong_thread"},
		{name: "stale capture", mutate: func(body map[string]any) {
			body["capturedAt"] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		}, wantReason: "stale_browser_capture"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newTranscriptAuditTestServer()
			request := createTranscriptAuditForTest(t, s)
			body := map[string]any{
				"capturedAt": time.Now().UTC().Format(time.RFC3339Nano), "tabId": "tab-1", "pageId": "page-1", "build": request.ExpectedBuild,
				"paneId": "pane-1", "paneThreadRef": "nth-one", "viewMode": "clean",
				"pageVisible": true, "surfaceVisible": true, "projectionKey": "projection-1", "snapshotHeadSeq": 3,
				"result": map[string]any{"status": "match", "complete": true, "scopes": []string{"messages"}, "counts": map[string]int{}, "differences": []any{}, "differencesTruncated": false, "exclusions": []any{}, "reasons": []any{}, "range": map[string]any{}},
			}
			test.mutate(body)
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			s.handleTranscriptAuditResult(recorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded)), request.ID)
			stored, _ := s.transcriptAudits.get(request.ID)
			if recorder.Code != http.StatusOK || stored.Status != "inconclusive" || stored.Result == nil || stored.Result.Reasons[0] != test.wantReason {
				t.Fatalf("response=%d stored=%#v", recorder.Code, stored)
			}
		})
	}
}

func TestTranscriptAuditExpiresWithoutBrowserAndCapsConcurrency(t *testing.T) {
	store := newTranscriptAuditStore()
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	for index := 0; index < maxTranscriptAuditPending; index++ {
		_, err := store.create(transcriptAuditRequest{
			ID: "audit-" + string(rune('a'+index)), ClientActionID: "action-" + string(rune('a'+index)), TabID: "tab", PaneID: "pane",
			Scopes: []transcriptAuditScope{transcriptAuditMessages}, Status: "pending", deadline: now.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.create(transcriptAuditRequest{ID: "overflow", ClientActionID: "overflow", TabID: "tab", PaneID: "pane", Status: "pending", deadline: now.Add(time.Second)}); err == nil {
		t.Fatal("pending concurrency cap was not enforced")
	}
	now = now.Add(2 * time.Second)
	expired, ok := store.get("audit-a")
	if !ok || expired.Status != "inconclusive" || expired.Result == nil || expired.Result.Reasons[0] != "browser_capture_missing" {
		t.Fatalf("expired = %#v", expired)
	}
}

func TestTranscriptAuditReferenceUsesPinnedStoreScopesAndBounds(t *testing.T) {
	ctx := context.Background()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	persistedRoot, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	path := filepath.Join(root.Path, "audit.jsonl")
	source := history.SourceProjection{
		Source: history.SourceCandidate{Root: root, Path: path, RelativePath: "audit.jsonl", Kind: history.ThreadMain, Size: 1, ModTime: now},
		Result: &history.ProjectionResult{Projection: history.ThreadProjection{
			Provider: history.ProviderCodex, NativeThreadID: "native-audit", IdentityEvidence: history.EvidenceNativeDurable,
			Kind: history.ThreadMain, CreatedAt: &now, UpdatedAt: &now, Metadata: history.Metadata{
				CWD: history.UnknownField(), Model: history.UnknownField(), Effort: history.UnknownField(), Priority: history.UnknownField(), Permission: history.UnknownField(), HistoricalBranch: history.UnknownField(), Origin: history.UnknownField(),
			},
		}, CompleteOffset: 1, CompleteLines: 1},
		ParserVersion: "audit-test", Status: history.SourcePresent, IndexedAt: now,
	}
	applied, err := store.ApplySource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, persistedRoot.ID, path)
	if err != nil || !ok {
		t.Fatalf("load source: ok=%t err=%v", ok, err)
	}
	items := make([]history.TranscriptItem, 0, 108)
	for index := 1; index <= 105; index++ {
		items = append(items, history.TranscriptItem{
			ID: fmt.Sprintf("message-%03d", index), ProjectedSeq: int64(index), SourceKey: fmt.Sprintf("source-%03d", index),
			TurnKey: "turn-1", TurnOrdinal: 1, Type: history.ItemAgentMessage, Status: history.ItemStatusCompleted,
			Role: history.RoleAssistant, Text: fmt.Sprintf("message %d", index), SourceLineSHA256: strings.Repeat("a", 64),
		})
	}
	items = append(items,
		history.TranscriptItem{ID: "tool-call", ProjectedSeq: 106, SourceKey: "source-106", TurnKey: "turn-1", TurnOrdinal: 1, Type: history.ItemToolCall, Status: history.ItemStatusUnknown, Role: history.RoleAssistant, Text: "input", SourceLineSHA256: strings.Repeat("b", 64)},
		history.TranscriptItem{ID: "tool-result", ProjectedSeq: 107, SourceKey: "source-107", TurnKey: "turn-1", TurnOrdinal: 1, Type: history.ItemToolResult, Status: history.ItemStatusUnknown, Role: history.RoleTool, Text: "output", SourceLineSHA256: strings.Repeat("c", 64)},
		history.TranscriptItem{ID: "reasoning", ProjectedSeq: 108, SourceKey: "source-108", TurnKey: "turn-1", TurnOrdinal: 1, Type: history.ItemReasoning, Status: history.ItemStatusCompleted, Role: history.RoleAssistant, Text: "thought", SourceLineSHA256: strings.Repeat("d", 64)},
	)
	projection := func(key string) history.TranscriptProjectionResult {
		return history.TranscriptProjectionResult{
			ProjectionKey: key, CompleteOffset: 1, ReadOffset: 1, CompleteLines: 1, ParserState: json.RawMessage(`{}`), Items: items,
			Turns: []history.TranscriptTurn{{Key: "turn-1", Ordinal: 1, StartSeq: 1, EndSeq: 108, CompletionSeq: 108, Status: history.TurnCompleted, CompletionEvidence: history.CompletionTaskComplete}},
		}
	}
	written, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: history.ProviderCodex, ParserVersion: "audit-test", CompletionRuleVersion: "audit-test", Result: projection("projection-a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := newStoredCatalog(t, store)
	messages, err := service.LoadTranscriptAuditReference(ctx, applied.ThreadID, written.ProjectionKey, written.LatestSeq, 1, 999, []transcriptAuditScope{transcriptAuditMessages})
	if err != nil || !messages.CapReached || messages.CapReason != "rows" || messages.StartSeq != 6 || messages.EndSeq != 105 || len(messages.Rows) != 100 {
		t.Fatalf("bounded messages = %#v err=%v", messages, err)
	}
	for _, row := range messages.Rows {
		if row.Category != string(history.CategoryMessages) || row.ProjectedSeq > written.LatestSeq {
			t.Fatalf("message scope leaked row: %#v", row)
		}
	}
	exactWindow, err := service.LoadTranscriptAuditReference(ctx, applied.ThreadID, written.ProjectionKey, written.LatestSeq, 20, 30, []transcriptAuditScope{transcriptAuditMessages})
	if err != nil || exactWindow.CapReached || exactWindow.StartSeq != 20 || exactWindow.EndSeq != 30 || len(exactWindow.Rows) != 11 {
		t.Fatalf("exact bounded window = %#v err=%v", exactWindow, err)
	}
	resolvedNative, err := service.ResolveThreadReference(ctx, "native-audit")
	if err != nil || resolvedNative != applied.ThreadID {
		t.Fatalf("resolve native = %q, %v", resolvedNative, err)
	}
	resolvedCatalog, err := service.ResolveThreadReference(ctx, applied.ThreadID)
	if err != nil || resolvedCatalog != applied.ThreadID {
		t.Fatalf("resolve catalog = %q, %v", resolvedCatalog, err)
	}
	for _, unsupported := range []string{"session-one", "nht_not_a_catalog_id"} {
		if _, err := service.ResolveThreadReference(ctx, unsupported); !errors.Is(err, errCatalogThreadNotFound) {
			t.Fatalf("unsupported reference %q resolved: %v", unsupported, err)
		}
	}

	realServer := &server{catalog: service, transcriptAudits: newTranscriptAuditStore(), transcriptAuditEvents: newTranscriptAuditHub()}
	createBody := `{"clientActionId":"real-resolver","threadRef":"native-audit","tabId":"tab-real","pageId":"page-real","paneId":"pane-real"}`
	createRecorder := httptest.NewRecorder()
	realServer.handleTranscriptAuditCreate(createRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/transcript-audits", strings.NewReader(createBody)))
	var realRequest transcriptAuditRequest
	if createRecorder.Code != http.StatusOK || json.Unmarshal(createRecorder.Body.Bytes(), &realRequest) != nil || realRequest.CatalogThreadID != applied.ThreadID {
		t.Fatalf("real resolver create = %d %s", createRecorder.Code, createRecorder.Body.String())
	}
	realRequest = pinTranscriptAuditTestBuild(t, realServer, realRequest)
	resultBody, _ := json.Marshal(map[string]any{
		"capturedAt": time.Now().UTC().Format(time.RFC3339Nano), "tabId": "tab-real", "pageId": "page-real", "build": realRequest.ExpectedBuild,
		"paneId": "pane-real", "paneThreadRef": "native-audit", "viewMode": "clean", "pageVisible": true, "surfaceVisible": true,
		"projectionKey": written.ProjectionKey, "snapshotHeadSeq": written.LatestSeq,
		"result": map[string]any{"status": "match", "complete": true, "scopes": []string{"messages"}, "counts": map[string]int{}, "differences": []any{}, "differencesTruncated": false, "exclusions": []any{}, "reasons": []any{}, "range": map[string]any{}},
	})
	resultRecorder := httptest.NewRecorder()
	realServer.handleTranscriptAuditResult(resultRecorder, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(resultBody)), realRequest.ID)
	realFinished, _ := realServer.transcriptAudits.get(realRequest.ID)
	if resultRecorder.Code != http.StatusOK || realFinished.Status != "match" {
		t.Fatalf("real resolver result = %d %#v", resultRecorder.Code, realFinished)
	}

	byteItems := append([]history.TranscriptItem(nil), items...)
	for index := 102; index < 105; index++ {
		byteItems[index].Text = strings.Repeat("large-output-", 12_000)
	}
	byteProjection := projection("projection-byte-cap")
	byteProjection.Items = byteItems
	byteWritten, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: history.ProviderCodex, ParserVersion: "audit-test", CompletionRuleVersion: "audit-test", ExpectedProjectionKey: written.ProjectionKey, Result: byteProjection,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.LoadTranscriptAuditReference(ctx, applied.ThreadID, written.ProjectionKey, written.LatestSeq, 0, 0, []transcriptAuditScope{transcriptAuditMessages}); !errors.Is(err, history.ErrTranscriptChanged) {
		t.Fatalf("changed projection error = %v", err)
	}
	byteDetail, err := service.LoadTranscriptAuditReference(ctx, applied.ThreadID, byteWritten.ProjectionKey, byteWritten.LatestSeq, 0, 0, []transcriptAuditScope{transcriptAuditMessages})
	if err != nil || !byteDetail.CapReached || byteDetail.CapReason != "bytes" || len(byteDetail.Rows) != 1 || byteDetail.StartSeq != 105 {
		t.Fatalf("byte-capped detail = %#v err=%v", byteDetail, err)
	}
}
