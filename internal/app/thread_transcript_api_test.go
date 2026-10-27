package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/history"
)

type fakeTranscriptCatalog struct {
	fakeThreadCatalog
	response transcriptResponse
	query    transcriptListQuery
	threadID string
	err      error
}

type fakeTranscriptEvidenceCatalog struct {
	fakeTranscriptCatalog
	evidence transcriptEvidenceResponse
	err      error
}

type fakeTranscriptBodyCatalog struct {
	fakeTranscriptCatalog
	body history.TranscriptRetainedBody
	err  error
}

func (f *fakeTranscriptBodyCatalog) LoadTranscriptBody(_ context.Context, _, _ string) (history.TranscriptRetainedBody, error) {
	return f.body, f.err
}

func (f *fakeTranscriptEvidenceCatalog) LoadTranscriptEvidence(_ context.Context, _, _ string) (transcriptEvidenceResponse, error) {
	return f.evidence, f.err
}

func (f *fakeTranscriptCatalog) LoadTranscript(_ context.Context, id string, query transcriptListQuery) (transcriptResponse, error) {
	f.threadID = id
	f.query = query
	return f.response, f.err
}

func TestTranscriptNestedRouteAndQueryContract(t *testing.T) {
	fake := &fakeTranscriptCatalog{response: transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView,
		Projection: transcriptProjection{State: "pending", SourceStatus: "present"},
	}}
	server := &server{catalog: fake}
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(server))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?limit=999&view=readable&before=opaque", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if fake.threadID != "nth_one" || fake.query.Limit != maxThreadTranscriptLimit ||
		fake.query.View != defaultThreadTranscriptView || fake.query.Before != "opaque" {
		t.Fatalf("thread = %q, query = %#v", fake.threadID, fake.query)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if recorder.Header().Get("ETag") == "" || recorder.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("cache headers = %#v", recorder.Header())
	}
	if !strings.Contains(recorder.Body.String(), `"items":[]`) || !strings.Contains(recorder.Body.String(), `"turns":[]`) {
		t.Fatalf("typed pending body = %s", recorder.Body.String())
	}
}

func TestTranscriptModesAndCategoryOverridesAreCanonical(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?view=dev&hide=system,reasoning&hide=toolCall", nil)
	query, err := parseTranscriptListQuery(request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"reasoning", "toolCall", "system"}
	if query.View != devThreadTranscriptView || strings.Join(query.HiddenCategories, ",") != strings.Join(want, ",") || !query.HiddenSpecified {
		t.Fatalf("query = %#v", query)
	}
	all, err := parseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?view=enhanced&hide=none", nil))
	if err != nil || len(all.HiddenCategories) != 0 || !all.HiddenSpecified {
		t.Fatalf("explicit all categories = %#v, %v", all, err)
	}
	legacy, err := parseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?view=readable", nil))
	if err != nil || legacy.View != defaultThreadTranscriptView || strings.Join(legacy.HiddenCategories, ",") != "reasoning,toolCall,toolResult,subagent,system,error,unclassified" {
		t.Fatalf("legacy alias = %#v, %v", legacy, err)
	}
	// Only the default Enhanced profile, as the browser sends it or left to
	// the default, reads message rows; every other profile filters by
	// category, Dev with the same hide set included.
	const enhancedHide = "hide=reasoning,toolCall,toolResult,subagent,system,error,unclassified"
	for raw, messagesOnly := range map[string]bool{
		"view=enhanced":                       true,
		"view=enhanced&" + enhancedHide:       true,
		"view=dev&" + enhancedHide:            false,
		"view=enhanced&hide=reasoning,system": false,
		"view=enhanced&hide=none":             false,
	} {
		query, err := parseTranscriptListQuery(httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript?"+raw, nil))
		if err != nil {
			t.Fatal(err)
		}
		request, err := transcriptReadRequest("nth_one", &query)
		if err != nil || request.MessagesOnly != messagesOnly || (messagesOnly && request.HiddenCategories != nil) {
			t.Fatalf("%s: read request = %#v, %v", raw, request, err)
		}
	}
}

func TestTranscriptTurnWindowQueryIsParsed(t *testing.T) {
	query, err := parseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?beforeNativeTurn=turn_native_7&turnLimit=3", nil))
	if err != nil {
		t.Fatal(err)
	}
	if query.BeforeNativeTurn != "turn_native_7" || query.BeforeTurn != 0 || query.TurnLimit != 3 {
		t.Fatalf("native turn window = %#v", query)
	}

	query, err = parseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?beforeTurn=7&turnLimit=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if query.BeforeNativeTurn != "" || query.BeforeTurn != 7 || query.TurnLimit != 1 {
		t.Fatalf("ordinal turn window = %#v", query)
	}
}

func TestTranscriptConditionalRequestReturnsNotModified(t *testing.T) {
	fake := &fakeTranscriptCatalog{response: transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView,
		Items: []transcriptItem{}, Turns: []transcriptTurn{},
		Projection: transcriptProjection{Key: "ntp_one", State: "empty", SourceStatus: "present", EvidenceAvailable: true},
	}}
	server := &server{catalog: fake}
	first := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	secondRequest := httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil)
	secondRequest.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(second, secondRequest)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("conditional response = %d %q", second.Code, second.Body.String())
	}
}

func TestTranscriptEvidenceRouteReturnsExactRawTextWithoutCaching(t *testing.T) {
	raw := ` { "large" : 900719925474099312345, "type" : "user" } `
	rawValue := raw
	fake := &fakeTranscriptEvidenceCatalog{evidence: transcriptEvidenceResponse{
		ThreadID: "nth_one", ItemID: "nit_one", ProjectionKey: "ntp_one", Provider: "claude",
		SourcePath: "/tmp/session.jsonl", SourceLine: 7, RawBytes: len(raw), ValidJSON: true, ValidUTF8: true, Raw: &rawValue,
	}}
	server := &server{catalog: fake}
	recorder := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript/evidence/nit_one", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("evidence response = %d %#v %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var body transcriptEvidenceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Raw == nil || *body.Raw != raw || body.RawBytes != len(raw) || !body.ValidJSON || !body.ValidUTF8 {
		t.Fatalf("evidence body = %#v", body)
	}
}

func TestTranscriptBodyRouteReturnsNanoRetainedContentWithoutCaching(t *testing.T) {
	fake := &fakeTranscriptBodyCatalog{body: history.TranscriptRetainedBody{
		ThreadID: "nth_one", ItemID: "nit_one", ProjectionKey: "ntp_one",
		ContentRef: "ncb_one", Text: "complete retained content", OriginalBytes: 25, StoredBytes: 25,
	}}
	server := &server{catalog: fake}
	recorder := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript/body/nit_one", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("body response = %d %#v %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var body history.TranscriptRetainedBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Text != "complete retained content" || body.ProjectionKey != "ntp_one" || body.ContentRef != "ncb_one" {
		t.Fatalf("retained body = %#v", body)
	}
}

func TestTranscriptEvidenceRouteMapsUnavailableAndCancellation(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
		body string
	}{
		{errTranscriptEvidenceUnavailable, http.StatusGone, "evidence_unavailable"},
		{context.Canceled, clientClosedRequestStatus, "request_cancelled"},
	} {
		fake := &fakeTranscriptEvidenceCatalog{err: test.err}
		server := &server{catalog: fake}
		recorder := httptest.NewRecorder()
		applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
			"/api/v1/threads/nth_one/transcript/evidence/nit_stale", nil))
		if recorder.Code != test.code || !strings.Contains(recorder.Body.String(), test.body) {
			t.Fatalf("evidence error %v => %d %s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}

func TestTranscriptRejectsInvalidQueryAndMethods(t *testing.T) {
	server := &server{catalog: &fakeTranscriptCatalog{}}
	for _, target := range []string{
		"/api/v1/threads/nth_one/transcript?limit=0",
		"/api/v1/threads/nth_one/transcript?view=raw",
		"/api/v1/threads/nth_one/transcript?turnLimit=1",
		"/api/v1/threads/nth_one/transcript?beforeTurn=2",
		"/api/v1/threads/nth_one/transcript?beforeTurn=2&beforeNativeTurn=native&turnLimit=1",
		"/api/v1/threads/nth_one/transcript?beforeTurn=2&turnLimit=21",
		"/api/v1/threads/nth_one/transcript?before=cursor&beforeTurn=2&turnLimit=1",
	} {
		recorder := httptest.NewRecorder()
		applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("%s error Cache-Control = %q", target, got)
		}
		assertCatalogErrorCode(t, recorder, "invalid_query")
	}
	recorder := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/threads/nth_one/transcript", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
	}
}

func TestTranscriptSafeBackendErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
		body string
	}{
		{errInvalidTranscriptCursor, http.StatusBadRequest, "invalid_cursor"},
		{errTranscriptCursorStale, http.StatusConflict, "transcript_changed"},
		{errTranscriptEvidenceUnavailable, http.StatusGone, "evidence_unavailable"},
		{errCatalogThreadNotFound, http.StatusNotFound, "thread_not_found"},
		{errors.New("private sqlite detail"), http.StatusInternalServerError, "transcript_error"},
	} {
		server := &server{catalog: &fakeTranscriptCatalog{err: test.err}}
		recorder := httptest.NewRecorder()
		applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil))
		if recorder.Code != test.code || strings.Contains(recorder.Body.String(), "private sqlite detail") {
			t.Fatalf("error %v => %d %q", test.err, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("error %v Cache-Control = %q", test.err, got)
		}
		assertCatalogErrorCode(t, recorder, test.body)
	}
}

func TestTranscriptNeverWritesOversizedResponse(t *testing.T) {
	fake := &fakeTranscriptCatalog{response: transcriptResponse{
		ThreadID: "nth_one", View: defaultThreadTranscriptView,
		Items:      []transcriptItem{{ID: "nit_one", ProjectedSeq: 1, Type: "agent_message", Text: strings.Repeat("x", maxThreadTranscriptBytes)}},
		Projection: transcriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present", EvidenceAvailable: true},
	}}
	server := &server{catalog: fake}
	recorder := httptest.NewRecorder()
	applicationHandler(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil))
	if recorder.Code != http.StatusInternalServerError || recorder.Body.Len() >= maxThreadTranscriptBytes {
		t.Fatalf("oversized response = status %d bytes %d", recorder.Code, recorder.Body.Len())
	}
	assertCatalogErrorCode(t, recorder, "transcript_error")
}
