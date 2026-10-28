package threadindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// TestCatalogHubNonBlockingFanOut proves a slow (never-draining) subscriber
// cannot stall a bump, that fan-out conflates to the newest version, and that an
// unsubscribed channel is dropped without panicking a later notify.
func TestCatalogHubNonBlockingFanOut(t *testing.T) {
	hub := newCatalogHub()
	slow := hub.Subscribe()
	fast := hub.Subscribe()

	done := make(chan struct{})
	go func() {
		hub.Notify(catalogChange{Version: 1, ThreadIDs: []string{"nth_a"}})
		hub.Notify(catalogChange{Version: 2, ThreadIDs: []string{"nth_b"}}) // slow's buffer is full — must not block
		hub.Notify(catalogChange{Version: 3, ThreadIDs: []string{"nth_c"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify blocked on a slow subscriber")
	}

	// The slow subscriber holds the latest version and every named repair, not
	// only the final identity.
	if got := <-slow; got.Version != 3 || len(got.ThreadIDs) != 3 || got.Refresh {
		t.Fatalf("slow subscriber = %#v, want version 3 with three row repairs", got)
	}
	if got := <-fast; got.Version != 3 || len(got.ThreadIDs) != 3 || got.Refresh {
		t.Fatalf("fast subscriber = %#v, want version 3 with three row repairs", got)
	}

	// A disconnected subscriber is inert: unsubscribe then notify must not panic.
	hub.Unsubscribe(fast)
	hub.Unsubscribe(fast) // idempotent, no double-close
	hub.Notify(catalogChange{Version: 4, Refresh: true})
	if got := <-slow; got.Version != 4 || !got.Refresh {
		t.Fatalf("remaining subscriber = %#v, want full refresh at 4", got)
	}
}

// TestCatalogHubCloseDisconnectsSubscribers proves service Close releases every
// subscriber (their read loop ends) and refuses new ones.
func TestCatalogHubCloseDisconnectsSubscribers(t *testing.T) {
	hub := newCatalogHub()
	ch := hub.Subscribe()
	hub.Close()
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel was not closed on hub close")
	}
	if _, ok := <-hub.Subscribe(); ok {
		t.Fatal("subscribe after close returned an open channel")
	}
	hub.Close() // idempotent
}

// TestThreadCatalogServiceDisabledWatcherRunsAndCloses covers -no-watch: no
// file watch starts, the saved catalog is ready without a walk, an explicit
// refresh still discovers, and the service closes cleanly.
func TestThreadCatalogServiceDisabledWatcherRunsAndCloses(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "88888888-8888-4888-8888-888888888888"
	writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID, "2026-07-21T09:00:00Z", "No-watch prompt")

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots:          []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
		DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if service.files != nil || service.IngestStatus().Watch.Mode != "disabled" {
		t.Fatal("attached-file watching was started despite DisableWatcher")
	}
	// The saved catalog is ready at once; only an explicit refresh walks.
	waitForCatalog(t, func() bool {
		status, err := service.IndexStatus(context.Background())
		return err == nil && status.State == "ready" && len(status.Roots) == 1 && status.Roots[0].SourceCount == 0
	})
	service.RequestRefresh()
	waitForCatalog(t, func() bool {
		status, err := service.IndexStatus(context.Background())
		return err == nil && status.State == "ready" && len(status.Roots) == 1 && status.Roots[0].SourceCount == 1
	})
	service.Close() // must return promptly with no leaked goroutine
}

// TestThreadCatalogServicePublishesVersionToStream proves the end-to-end notify
// wiring: a real service subscriber receives a version bump when a scan changes
// the catalog, and no event fires for an unchanged rescan.
func TestThreadCatalogServicePublishesVersionToStream(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots:          []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
		DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	events, unsubscribe := service.SubscribeCatalog()
	defer unsubscribe()

	// A newly discovered source bumps the version and pushes it.
	nativeID := "99999999-9999-4999-8999-999999999999"
	writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID, "2026-07-21T10:00:00Z", "Stream prompt")
	service.RequestRefresh()
	first := awaitCatalogChange(t, events)
	if first.Version == 0 || !first.Refresh {
		t.Fatalf("new thread change = %#v, want a structural refresh", first)
	}
	awaitTranscriptCommits(t, events, first.Version)

	// Updating an existing source names only its catalog row. This is the common
	// live-message path and must not tell every browser to reload its window.
	writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID,
		"2026-07-21T10:01:00Z", "Stream prompt with a longer updated body")
	service.RequestRefresh()
	updated := awaitCatalogChange(t, events)
	if updated.Refresh || len(updated.ThreadIDs) != 1 || updated.ThreadIDs[0] == "" {
		t.Fatalf("existing thread change = %#v, want one named row repair", updated)
	}
	awaitTranscriptCommits(t, events, updated.Version)

	// An unchanged rescan must not push another event (monotonic guard).
	service.RequestRefresh()
	select {
	case v := <-events:
		t.Fatalf("unchanged rescan pushed a redundant change %#v", v)
	case <-time.After(300 * time.Millisecond):
	}
}

// A browser re-reads an open saved transcript each time a catalog event names
// its thread, and nothing else tells it to. So a reader that does exactly that
// must end with every row the change committed. Before, a scan named the
// thread before committing its rows and the commit's own event was dropped as
// an old version: a read-only pane showed a Claude thread without its newest
// answer for good. Boundary: a real service subscriber that reads the
// transcript on each event naming the thread.
func TestCatalogEventFollowsTheTranscriptRowsItAnnounces(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots:          []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
		DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	events, unsubscribe := service.SubscribeCatalog()
	defer unsubscribe()

	nativeID := "88888888-8888-4888-8888-888888888888"
	path := filepath.Join(projectPath, nativeID+".jsonl")
	writeClaudeCatalogFixture(t, path, nativeID, "2026-07-21T10:00:00Z", "first prompt")
	service.RequestRefresh()
	awaitTranscriptCommits(t, events, awaitCatalogChange(t, events).Version)

	// Claude saves a turn's last lines together; a long batch makes the apply
	// outlast the reader's read.
	var batch strings.Builder
	for i := 0; i < 300; i++ {
		batch.WriteString(claudeCatalogUserRow(nativeID, "2026-07-21T10:01:00Z", fmt.Sprintf("row %d", i)))
	}
	batch.WriteString(claudeCatalogUserRow(nativeID, "2026-07-21T10:02:00Z", "the newest prompt"))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(batch.String()); err != nil {
		t.Fatal(err)
	}
	file.Close()
	service.RequestRefresh()
	var shown []TranscriptItem
	for change := awaitCatalogChange(t, events); ; {
		if change.Refresh || len(change.ThreadIDs) != 1 {
			t.Fatalf("append change = %#v, want one named row", change)
		}
		page, err := service.LoadTranscript(context.Background(), change.ThreadIDs[0], TranscriptQuery{View: DefaultThreadTranscriptView, Limit: 5})
		if err != nil {
			t.Fatal(err)
		}
		shown = page.Items
		select {
		case change = <-events:
			continue
		case <-time.After(5 * time.Second):
		}
		break
	}
	if !slices.ContainsFunc(shown, func(item TranscriptItem) bool { return item.Text == "the newest prompt" }) {
		t.Fatalf("after the last event naming the thread the reader shows %d items, without the newest prompt", len(shown))
	}
}

// awaitTranscriptCommits takes the events a scan's transcript commits send
// after its own: each names its thread again at the scan's version.
func awaitTranscriptCommits(t *testing.T, events <-chan catalogChange, version uint64) {
	t.Helper()
	for {
		select {
		case again := <-events:
			if again.Version != version || again.Refresh || len(again.ThreadIDs) == 0 {
				t.Fatalf("after version %d: %#v, want only transcript commits naming their threads", version, again)
			}
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

func awaitCatalogChange(t *testing.T, events <-chan catalogChange) catalogChange {
	t.Helper()
	select {
	case change, ok := <-events:
		if !ok {
			t.Fatal("stream closed before a version arrived")
		}
		return change
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a pushed catalog version")
		return catalogChange{}
	}
}
