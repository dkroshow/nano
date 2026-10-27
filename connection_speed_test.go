package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The browser's throughput readout divides the bytes it received by the time
// they took. A missing route blanks the readout; a cacheable answer makes it
// measure the browser's cache instead of the connection.
func TestConnectionTestServesFixedUncacheableBytes(t *testing.T) {
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(&server{}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/connection-test", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 64<<10 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("connection test = %d, %d bytes, Cache-Control %q", rec.Code, rec.Body.Len(), rec.Header().Get("Cache-Control"))
	}
}
