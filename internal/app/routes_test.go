package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplicationRoutesMapEachPatternToOneOperation(t *testing.T) {
	routes := applicationRoutes(fixtureOwners(&server{}))
	seen := map[string]string{}
	for _, route := range routes {
		if route.Method == "" || route.Path == "" || route.Operation == "" || route.Handler == nil {
			t.Errorf("incomplete route: %#v", route)
			continue
		}
		key := route.Method + " " + route.Path
		if prior := seen[key]; prior != "" {
			t.Errorf("route %s maps to both %s and %s", key, prior, route.Operation)
		}
		seen[key] = route.Operation
		if route.Path != "/" && strings.HasSuffix(route.Path, "/") {
			t.Errorf("route %s is a prefix dispatcher, not one operation", key)
		}
	}
	for _, key := range []string{
		"GET /api/v1/hosted-threads/{threadRef}",
		"POST /api/v1/hosted-threads/{threadRef}/turns",
		"POST /api/v1/hosted-threads/{threadRef}/interactions/{requestId}",
		"GET /api/sessions/{sessionId}/events/page",
	} {
		if seen[key] == "" {
			t.Errorf("missing operation route %s", key)
		}
	}
}

func TestRouteMuxReturnsJSONForUnsupportedMethods(t *testing.T) {
	mux := http.NewServeMux()
	mountRoutes(mux, []applicationRoute{{Method: http.MethodGet, Path: "/only-get", Operation: "readOnly", Handler: func(http.ResponseWriter, *http.Request) {}}})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/only-get", nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unsupported method = %d %q %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
}

func TestRouteMuxReturnsJSONForUnknownAPIPaths(t *testing.T) {
	mux := http.NewServeMux()
	mountRoutes(mux, []applicationRoute{{Method: http.MethodGet, Path: "/known", Operation: "known", Handler: func(http.ResponseWriter, *http.Request) {}}})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/not-a-route", nil))
	if recorder.Code != http.StatusNotFound || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unknown API path = %d %q %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
}
