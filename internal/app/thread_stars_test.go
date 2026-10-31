package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
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
	if err := fixtureSetStarred(t, store, "codex", "native-a", true); err != nil {
		t.Fatal(err)
	}
	s := &server{threadStars: store}
	threads := []sidebar.Thread{
		{Thread: threadindex.Thread{ID: "nth_a", Provider: "codex", NativeID: "native-a"}},
		{Thread: threadindex.Thread{ID: "nth_b", Provider: "claude", NativeID: "native-b"}},
	}
	for i := range threads {
		fixtureOwners(s).sidebarOwner().StarThread(&threads[i])
	}
	if !threads[0].Starred || threads[1].Starred {
		t.Fatalf("overlay = %+v", threads)
	}
}

func TestSidebarActiveStampsCurrentStarsAtReadTime(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	byNative := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	if item := byNative["attached-recent"]; item == nil || item["starred"] != false {
		t.Fatalf("attached row before its star: %v", item)
	}
	if err := fixtureSetStarred(t, s.threadStars, "codex", "attached-recent", true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := fixtureOwners(s).sidebarOwner().StartSidebarMaterializer(ctx, now); err != nil {
		t.Fatal(err)
	}
	byNative = sidebarItemsByNative(t, getSidebarActive(t, s, now))
	if item := byNative["attached-recent"]; item == nil || item["starred"] != true {
		t.Fatalf("current star missing from the active sidebar: %v", item)
	}
}

func TestThreadStarredEndpointTogglesAndReturnsRow(t *testing.T) {
	store := openTestThreadStars(t)
	catalog, id := starEndpointCatalog(t)
	s := &server{catalog: catalog, threadStars: store, Registry: newSessionRegistry()}

	req := httptest.NewRequest(http.MethodPut, "/api/v1/threads/"+id+"/starred", strings.NewReader(`{"starred":true}`))
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("star status = %d body %s", rec.Code, rec.Body.String())
	}
	var starred sidebar.Thread
	if err := json.Unmarshal(rec.Body.Bytes(), &starred); err != nil {
		t.Fatal(err)
	}
	if !starred.Starred || starred.ID != id {
		t.Fatalf("starred response = %+v", starred)
	}
	if !store.IsStarred("codex", "native-1") {
		t.Fatal("PUT did not persist the star")
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/threads/"+id+"/starred", strings.NewReader(`{"starred":false}`))
	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
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
	catalog, id := starEndpointCatalog(t)
	s := &server{
		catalog:       catalog,
		threadStars:   store,
		runtimeEvents: hub,
		Registry:      newSessionRegistry(),
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/threads/"+id+"/starred", strings.NewReader(`{"starred":true}`))
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
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
	catalog, id := starEndpointCatalog(t)
	s := &server{
		catalog:     catalog,
		threadStars: openTestThreadStars(t),
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/threads/"+id+"/starred", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
}

func strPtr(value string) *string { return &value }

func starEndpointCatalog(t *testing.T, identity ...string) (*sidebar.CatalogSource, string) {
	provider, native := "codex", "native-1"
	if len(identity) == 2 {
		provider, native = identity[0], identity[1]
	}
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "star-catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	kind := history.RootCodexSessions
	if provider == "claude" {
		kind = history.RootClaudeProjects
	}
	if provider == "grok" {
		kind = history.RootGrokSessions
	}
	root := history.SourceRoot{Provider: history.Provider(provider), Kind: kind, Path: t.TempDir()}
	if _, err := store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	id := seedSidebarThread(t, store, root, native, "", history.ThreadMain, time.Now(), []historyCourt{{final: true, text: "Pinned"}})
	return newStoredCatalog(t, store), id
}

// fixtureSetStarred seeds through the real sidebar HTTP mutation and store.
func fixtureSetStarred(t *testing.T, stars *sidebar.Stars, provider, native string, starred bool) error {
	t.Helper()
	catalog, id := starEndpointCatalog(t, provider, native)
	controller := sidebar.New(sidebar.Sources{Catalog: catalog.Service, Stars: stars})
	recorder := httptest.NewRecorder()
	controller.HandleThreadStarred(recorder, httptest.NewRequest(http.MethodPut, "/starred", strings.NewReader(fmt.Sprintf(`{"starred":%t}`, starred))), id)
	if recorder.Code != http.StatusOK {
		return fmt.Errorf("star fixture status=%d: %s", recorder.Code, recorder.Body.String())
	}
	return nil
}
