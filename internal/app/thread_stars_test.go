package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func openTestThreadStars(t *testing.T) *sidebar.Stars {
	t.Helper()
	store, err := sidebar.OpenStars(filepath.Join(t.TempDir(), "thread-stars.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestAttachStarredOverlayMarksMatchingRows(t *testing.T) {
	store := openTestThreadStars(t)
	if err := store.SetStarred("codex", "native-a", true); err != nil {
		t.Fatal(err)
	}
	s := &server{threadStars: store}
	threads := []sidebar.Thread{
		{Thread: threadindex.Thread{ID: "nth_a", Provider: "codex", NativeID: "native-a"}},
		{Thread: threadindex.Thread{ID: "nth_b", Provider: "claude", NativeID: "native-b"}},
	}
	s.attachStarredOverlay(threads)
	if !threads[0].Starred || threads[1].Starred {
		t.Fatalf("overlay = %+v", threads)
	}
}

func TestSidebarActiveStampsCurrentStarsAtReadTime(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	body := getSidebarActive(t, s, now)
	byNative := sidebarItemsByNative(t, body)
	if item := byNative["starred-old"]; item == nil || item["starred"] != true {
		t.Fatalf("current star missing from active sidebar: %v", item)
	}
	if err := s.threadStars.SetStarred("codex", "starred-old", false); err != nil {
		t.Fatal(err)
	}
	if err := s.materializeSidebar(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	body = getSidebarActive(t, s, now)
	byNative = sidebarItemsByNative(t, body)
	if _, ok := byNative["starred-old"]; ok {
		t.Fatalf("unstarring should drop the rescued historical row: %v", byNative["starred-old"])
	}
}

func TestThreadStarredEndpointTogglesAndReturnsRow(t *testing.T) {
	store := openTestThreadStars(t)
	catalog := &fakeThreadCatalog{thread: sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-1", Title: strPtr("Pinned")}}}
	s := &server{catalog: catalog, threadStars: store, Registry: newSessionRegistry()}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/threads/nth_1/starred", strings.NewReader(`{"starred":true}`))
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("star status = %d body %s", rec.Code, rec.Body.String())
	}
	var starred sidebar.Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &starred); err != nil {
		t.Fatal(err)
	}
	if !starred.Starred || starred.ID != "nth_1" {
		t.Fatalf("starred response = %+v", starred)
	}
	if !store.IsStarred("codex", "native-1") {
		t.Fatal("PUT did not persist the star")
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/threads/nth_1/starred", strings.NewReader(`{"starred":false}`))
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unstar status = %d body %s", rec.Code, rec.Body.String())
	}
	if store.IsStarred("codex", "native-1") {
		t.Fatal("unstar did not persist")
	}
}

func TestThreadStarredEndpointNudgesOpenBrowsers(t *testing.T) {
	store := openTestThreadStars(t)
	hub := sidebar.NewRuntimeHub()
	ch := hub.Subscribe()
	t.Cleanup(func() { hub.Unsubscribe(ch) })
	s := &server{
		catalog:       &fakeThreadCatalog{thread: sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-1"}}},
		threadStars:   store,
		runtimeEvents: hub,
		Registry:      newSessionRegistry(),
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/threads/nth_1/starred", strings.NewReader(`{"starred":true}`))
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("star status = %d body %s", rec.Code, rec.Body.String())
	}
	select {
	case delta := <-ch:
		if !delta.Refresh {
			t.Fatalf("runtime nudge = %+v, want refresh", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("star write did not nudge open browsers")
	}
}

func TestThreadStarredEndpointRejectsMissingBody(t *testing.T) {
	s := &server{
		catalog:     &fakeThreadCatalog{thread: sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-1"}}},
		threadStars: openTestThreadStars(t),
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/threads/nth_1/starred", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
}

func strPtr(value string) *string { return &value }
