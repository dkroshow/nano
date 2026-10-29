package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threadindex"
)

type fakeTranscriptCatalog struct {
	fakeThreadCatalog
	response threadindex.TranscriptResponse
	query    threadindex.TranscriptQuery
	threadID string
	err      error
}

type fakeTranscriptEvidenceCatalog struct {
	fakeTranscriptCatalog
	evidence threadindex.TranscriptEvidenceResponse
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

func (f *fakeTranscriptEvidenceCatalog) LoadTranscriptEvidence(_ context.Context, _, _ string) (threadindex.TranscriptEvidenceResponse, error) {
	return f.evidence, f.err
}

func (f *fakeTranscriptCatalog) LoadTranscript(_ context.Context, id string, query threadindex.TranscriptQuery) (threadindex.TranscriptResponse, error) {
	f.threadID = id
	f.query = query
	return f.response, f.err
}

func TestTranscriptNestedRouteAndQueryContract(t *testing.T) {
	fake := &fakeTranscriptCatalog{response: threadindex.TranscriptResponse{
		ThreadID: "nth_one", View: threadindex.DefaultThreadTranscriptView,
		Projection: threadindex.TranscriptProjection{State: "pending", SourceStatus: "present"},
	}}
	server := &server{catalog: fake}
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(fixtureOwners(server)))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/v1/threads/nth_one/transcript?limit=999&view=readable&before=opaque", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if fake.threadID != "nth_one" || fake.query.Limit != threadindex.MaxThreadTranscriptLimit ||
		fake.query.View != threadindex.DefaultThreadTranscriptView || fake.query.Before != "opaque" {
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
	post := httptest.NewRecorder()
	mux.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/v1/threads/nth_one/transcript", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", post.Code)
	}
}
