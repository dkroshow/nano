package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
)

// fakeStreamCatalog is a sidebar.CatalogReader that also exposes the SSE stream surface,
// backed by a real catalogHub so tests can drive bumps.
type fakeStreamCatalog struct {
	*fakeThreadCatalog
	hub     *catalogHub
	version int64
}

func (f *fakeStreamCatalog) SubscribeCatalog() (<-chan catalog.Change, func()) {
	ch := f.hub.Subscribe()
	return ch, func() { f.hub.Unsubscribe(ch) }
}

func (f *fakeStreamCatalog) CatalogVersion(context.Context) (int64, error) {
	return f.version, nil
}

// TestCatalogEventsStreamDeliversVersionOnConnectAndBump covers the D1 handler
// contract: the first event is the current version, a bump delivers an event,
// and the SSE headers/format are exact.
func TestCatalogEventsStreamDeliversVersionOnConnectAndBump(t *testing.T) {
	hub := newCatalogHub()
	fake := &fakeStreamCatalog{fakeThreadCatalog: &fakeThreadCatalog{}, hub: hub, version: 5}
	srv := httptest.NewServer(http.HandlerFunc((&server{catalog: fake}).catalogViews().HandleEvents))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	reader := bufio.NewReader(resp.Body)
	if got := readCatalogVersionEvent(t, reader); got != 5 {
		t.Fatalf("version-on-connect = %d, want 5", got)
	}
	hub.Notify(catalog.Change{Version: 6, ThreadIDs: []string{"nth_changed"}})
	if got := readCatalogVersionEvent(t, reader); got != 6 {
		t.Fatalf("bump event = %d, want 6", got)
	}
}

func TestCatalogEventsStreamRejectsNonGetAndUnavailable(t *testing.T) {
	fake := &fakeStreamCatalog{fakeThreadCatalog: &fakeThreadCatalog{}, hub: newCatalogHub(), version: 1}
	rec := httptest.NewRecorder()
	applicationHandler(&server{catalog: fake}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/thread-index/events", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}

	// A catalog without the stream surface reports unavailable, not a panic.
	rec = httptest.NewRecorder()
	(&server{catalog: &fakeThreadCatalog{}}).catalogViews().HandleEvents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/thread-index/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-stream status = %d", rec.Code)
	}
	assertCatalogErrorCode(t, rec, "catalog_unavailable")
}

// readCatalogVersionEvent reads SSE lines until a catalog data line, skipping
// heartbeat comments, and returns the parsed threadindex.CatalogVersion.
func readCatalogVersionEvent(t *testing.T, reader *bufio.Reader) int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(line, "data: ") {
			var payload struct {
				CatalogVersion int64 `json:"catalogVersion"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("parse catalog event from %q: %v", line, err)
			}
			return payload.CatalogVersion
		}
	}
	t.Fatal("no catalog event within deadline")
	return 0
}
