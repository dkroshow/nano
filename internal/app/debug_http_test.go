package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Process diagnostics are local-only; the remote application mux must not expose them.
func TestLoopbackOnlyRoutesNeverReachRemoteHandler(t *testing.T) {
	application := http.NewServeMux()
	local, remote := listenerHandlersWithLoopback(application, func(handler http.Handler) http.Handler { return handler }, false)

	response := httptest.NewRecorder()
	local.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost:7690/debug/pprof/goroutine?debug=1", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "goroutine profile") {
		t.Fatalf("loopback goroutine profile = %d %q", response.Code, response.Body.String())
	}

	remoteResponse := httptest.NewRecorder()
	remote.ServeHTTP(remoteResponse, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil))
	if remoteResponse.Code != http.StatusNotFound || strings.Contains(remoteResponse.Body.String(), "goroutine profile") {
		t.Fatalf("application/remote handler exposed pprof = %d %q", remoteResponse.Code, remoteResponse.Body.String())
	}
}

func TestLoopbackDebugHandlerExposesStandardPprofRoutes(t *testing.T) {
	local, _ := listenerHandlersWithLoopback(http.NewServeMux(), func(handler http.Handler) http.Handler { return handler }, false)
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol"} {
		response := httptest.NewRecorder()
		local.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost:7690"+path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, body = %q", path, response.Code, response.Body.String())
		}
	}
}

func TestLocalListenerRejectsUntrustedHostBeforeAnyRoute(t *testing.T) {
	application := http.NewServeMux()
	application.HandleFunc("/api/write", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	application.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	application.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	local, _ := listenerHandlersWithLoopback(application, func(handler http.Handler) http.Handler { return handler }, false)
	for _, host := range []string{"127.0.0.1:7690", "localhost:7690", "[::1]:7690", "127.0.0.2:7690"} {
		t.Run(host, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://localhost:7690/api/write", nil)
			request.Host = host
			response := httptest.NewRecorder()
			local.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("loopback Host %q status = %d", host, response.Code)
			}
		})
	}
	for _, test := range []struct{ host, path string }{
		{"localhost:7690", "/healthz"},
		{"127.0.0.1:7690", "/"},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://localhost:7690"+test.path, nil)
		request.Host = test.host
		response := httptest.NewRecorder()
		local.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s with Host %q status = %d", test.path, test.host, response.Code)
		}
	}
	for _, path := range []string{"/api/write", "/debug/pprof/", "/api/v1/maintenance/interrupted-submissions"} {
		t.Run(path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://localhost:7690"+path, nil)
			request.Host = "attacker.example:7690"
			response := httptest.NewRecorder()
			local.ServeHTTP(response, request)
			if response.Code != http.StatusMisdirectedRequest {
				t.Fatalf("untrusted Host on %s status = %d", path, response.Code)
			}
		})
	}
}

func TestLocalListenerRejectsCrossSiteWritesWithoutBlockingLocalTools(t *testing.T) {
	application := http.NewServeMux()
	application.HandleFunc("/api/write", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	local, _ := listenerHandlersWithLoopback(application, func(handler http.Handler) http.Handler { return handler }, false)
	for _, test := range []struct {
		name, method, path, host, origin, referer, fetchSite string
		want                                                 int
	}{
		{"curl and hosted agents", http.MethodPost, "/api/write", "127.0.0.1:7690", "", "", "", http.StatusNoContent},
		{"same origin browser", http.MethodPut, "/api/write", "localhost:7690", "http://localhost:7690", "", "same-origin", http.StatusNoContent},
		{"Vite proxy", http.MethodPost, "/api/write", "localhost:5173", "http://localhost:5173", "", "same-origin", http.StatusNoContent},
		{"Vite direct API", http.MethodPost, "/api/write", "localhost:7690", "http://localhost:5173", "", "same-site", http.StatusForbidden},
		{"cross-site origin", http.MethodPost, "/api/write", "127.0.0.1:7690", "https://attacker.example", "", "", http.StatusForbidden},
		{"opaque origin", http.MethodPost, "/api/write", "127.0.0.1:7690", "null", "", "", http.StatusForbidden},
		{"cross-site fetch metadata", http.MethodPut, "/api/write", "127.0.0.1:7690", "", "", "cross-site", http.StatusForbidden},
		{"cross-site referer", http.MethodDelete, "/api/write", "localhost:7690", "", "https://attacker.example/form", "", http.StatusForbidden},
		{"cross-site maintenance", http.MethodPost, "/api/v1/maintenance/interrupted-submissions", "localhost:7690", "https://attacker.example", "", "", http.StatusForbidden},
		// An https target arrives over TLS: the loopback HTTPS listener serves this same handler.
		{"HTTPS same origin browser", http.MethodPost, "https://localhost:7694/api/write", "localhost:7694", "https://localhost:7694", "", "same-origin", http.StatusNoContent},
		{"HTTPS same origin referer", http.MethodPost, "https://localhost:7694/api/write", "localhost:7694", "", "https://localhost:7694/?thread=a", "", http.StatusNoContent},
		{"plain HTTP page writing over HTTPS", http.MethodPost, "https://localhost:7694/api/write", "localhost:7694", "http://localhost:7694", "", "same-site", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := test.path
			if strings.HasPrefix(target, "/") {
				target = "http://localhost:7690" + target
			}
			request := httptest.NewRequest(test.method, target, nil)
			request.Host = test.host
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.referer != "" {
				request.Header.Set("Referer", test.referer)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			response := httptest.NewRecorder()
			local.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestLocalListenerRejectsCrossSiteProfilingReads(t *testing.T) {
	local, _ := listenerHandlersWithLoopback(http.NewServeMux(), func(handler http.Handler) http.Handler { return handler }, false)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7690/debug/pprof/goroutine?debug=1", nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	local.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "goroutine profile") {
		t.Fatalf("cross-site profiling read = %d %q", response.Code, response.Body.String())
	}
}

func TestApplicationRoutesOmitRawProviderRPC(t *testing.T) {
	for _, route := range applicationRoutes(&server{}) {
		if route.Path == "/api/sessions/{sessionId}/rpc" {
			t.Fatal("raw provider RPC remains reachable")
		}
	}
}
