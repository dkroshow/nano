package threadindex

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
	response TranscriptResponse
	query    TranscriptQuery
	threadID string
	err      error
}

type fakeTranscriptEvidenceCatalog struct {
	fakeTranscriptCatalog
	evidence TranscriptEvidenceResponse
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

func (f *fakeTranscriptEvidenceCatalog) LoadTranscriptEvidence(_ context.Context, _, _ string) (TranscriptEvidenceResponse, error) {
	return f.evidence, f.err
}

func (f *fakeTranscriptCatalog) LoadTranscript(_ context.Context, id string, query TranscriptQuery) (TranscriptResponse, error) {
	f.threadID = id
	f.query = query
	return f.response, f.err
}

func TestTranscriptModesAndCategoryOverridesAreCanonical(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?view=dev&hide=system,reasoning&hide=toolCall", nil)
	query, err := ParseTranscriptListQuery(request)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"reasoning", "toolCall", "system"}
	if query.View != DevThreadTranscriptView || strings.Join(query.HiddenCategories, ",") != strings.Join(want, ",") || !query.HiddenSpecified {
		t.Fatalf("query = %#v", query)
	}
	all, err := ParseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?view=enhanced&hide=none", nil))
	if err != nil || len(all.HiddenCategories) != 0 || !all.HiddenSpecified {
		t.Fatalf("explicit all categories = %#v, %v", all, err)
	}
	legacy, err := ParseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?view=readable", nil))
	if err != nil || legacy.View != DefaultThreadTranscriptView || strings.Join(legacy.HiddenCategories, ",") != "reasoning,toolCall,toolResult,subagent,system,error,unclassified" {
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
		query, err := ParseTranscriptListQuery(httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript?"+raw, nil))
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
	query, err := ParseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?beforeNativeTurn=turn_native_7&turnLimit=3", nil))
	if err != nil {
		t.Fatal(err)
	}
	if query.BeforeNativeTurn != "turn_native_7" || query.BeforeTurn != 0 || query.TurnLimit != 3 {
		t.Fatalf("native turn window = %#v", query)
	}

	query, err = ParseTranscriptListQuery(httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?beforeTurn=7&turnLimit=1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if query.BeforeNativeTurn != "" || query.BeforeTurn != 7 || query.TurnLimit != 1 {
		t.Fatalf("ordinal turn window = %#v", query)
	}
}

func TestTranscriptConditionalRequestReturnsNotModified(t *testing.T) {
	fake := &fakeTranscriptCatalog{response: TranscriptResponse{
		ThreadID: "nth_one", View: DefaultThreadTranscriptView,
		Items: []TranscriptItem{}, Turns: []TranscriptTurn{},
		Projection: TranscriptProjection{Key: "ntp_one", State: "empty", SourceStatus: "present", EvidenceAvailable: true},
	}}
	server := NewHandlers(fake)
	first := httptest.NewRecorder()
	indexTestRoutes(server).ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	secondRequest := httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil)
	secondRequest.Header.Set("If-None-Match", first.Header().Get("ETag"))
	second := httptest.NewRecorder()
	indexTestRoutes(server).ServeHTTP(second, secondRequest)
	if second.Code != http.StatusNotModified || second.Body.Len() != 0 {
		t.Fatalf("conditional response = %d %q", second.Code, second.Body.String())
	}
}

func TestTranscriptEvidenceRouteReturnsExactRawTextWithoutCaching(t *testing.T) {
	raw := ` { "large" : 900719925474099312345, "type" : "user" } `
	rawValue := raw
	fake := &fakeTranscriptEvidenceCatalog{evidence: TranscriptEvidenceResponse{
		ThreadID: "nth_one", ItemID: "nit_one", ProjectionKey: "ntp_one", Provider: "claude",
		SourcePath: "/tmp/session.jsonl", SourceLine: 7, RawBytes: len(raw), ValidJSON: true, ValidUTF8: true, Raw: &rawValue,
	}}
	server := NewHandlers(fake)
	recorder := httptest.NewRecorder()
	indexTestRoutes(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript/evidence/nit_one", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("evidence response = %d %#v %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var body TranscriptEvidenceResponse
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
	server := NewHandlers(fake)
	recorder := httptest.NewRecorder()
	indexTestRoutes(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
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
		server := NewHandlers(fake)
		recorder := httptest.NewRecorder()
		indexTestRoutes(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
			"/api/v1/threads/nth_one/transcript/evidence/nit_stale", nil))
		if recorder.Code != test.code || !strings.Contains(recorder.Body.String(), test.body) {
			t.Fatalf("evidence error %v => %d %s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}

// Query validation is tested here; method dispatch uses the real app route table.
func TestTranscriptRejectsInvalidQueryAndMethods(t *testing.T) {
	server := NewHandlers(&fakeTranscriptCatalog{})
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
		indexTestRoutes(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("%s error Cache-Control = %q", target, got)
		}
		assertCatalogErrorCode(t, recorder, "invalid_query")
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
		{ErrCatalogThreadNotFound, http.StatusNotFound, "thread_not_found"},
		{errors.New("private sqlite detail"), http.StatusInternalServerError, "transcript_error"},
	} {
		server := NewHandlers(&fakeTranscriptCatalog{err: test.err})
		recorder := httptest.NewRecorder()
		indexTestRoutes(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil))
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
	fake := &fakeTranscriptCatalog{response: TranscriptResponse{
		ThreadID: "nth_one", View: DefaultThreadTranscriptView,
		Items:      []TranscriptItem{{ID: "nit_one", ProjectedSeq: 1, Type: "agent_message", Text: strings.Repeat("x", MaxThreadTranscriptBytes)}},
		Projection: TranscriptProjection{Key: "ntp_one", State: "ready", SourceStatus: "present", EvidenceAvailable: true},
	}}
	server := NewHandlers(fake)
	recorder := httptest.NewRecorder()
	indexTestRoutes(server).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_one/transcript", nil))
	if recorder.Code != http.StatusInternalServerError || recorder.Body.Len() >= MaxThreadTranscriptBytes {
		t.Fatalf("oversized response = status %d bytes %d", recorder.Code, recorder.Body.Len())
	}
	assertCatalogErrorCode(t, recorder, "transcript_error")
}

type fakeThreadCatalog struct{}

func (*fakeThreadCatalog) IndexStatus(context.Context) (IndexStatus, error) {
	return IndexStatus{}, nil
}
func (*fakeThreadCatalog) RequestRefresh() bool { return false }
func indexTestRoutes(h *Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/threads/{threadId}/transcript", func(w http.ResponseWriter, r *http.Request) { h.HandleThreadTranscript(w, r, r.PathValue("threadId")) })
	mux.HandleFunc("GET /api/v1/threads/{threadId}/transcript/body/{itemId}", func(w http.ResponseWriter, r *http.Request) {
		h.HandleThreadTranscriptBody(w, r, r.PathValue("threadId"), r.PathValue("itemId"))
	})
	mux.HandleFunc("GET /api/v1/threads/{threadId}/transcript/evidence/{itemId}", func(w http.ResponseWriter, r *http.Request) {
		h.HandleThreadTranscriptEvidence(w, r, r.PathValue("threadId"), r.PathValue("itemId"))
	})
	return mux
}

func assertCatalogErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != want {
		t.Fatalf("error code = %q, want %q", body.Error.Code, want)
	}
}
