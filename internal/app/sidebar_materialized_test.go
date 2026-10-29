package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// sidebarTestCatalog wraps the fixture's real catalog so a test can publish
// catalog changes, count full sidebar graph reads, and fail one read.
type sidebarTestCatalog struct {
	store *history.Store
	*threadCatalogService
	changes   chan catalog.Change
	mu        sync.Mutex
	fullReads int
	failNext  bool
}

func (c *sidebarTestCatalog) SubscribeCatalog() (<-chan catalog.Change, func()) {
	return c.changes, func() {}
}

func (c *sidebarTestCatalog) ListSidebarGraph(ctx context.Context, params history.SidebarGraphParams) (history.SidebarGraph, error) {
	c.mu.Lock()
	fail := c.failNext
	c.failNext = false
	if !params.OnlyExtra {
		c.fullReads++
	}
	c.mu.Unlock()
	if fail {
		return history.SidebarGraph{}, errors.New("synthetic transient read failure")
	}
	return c.threadCatalogService.ListSidebarGraph(ctx, params)
}

func (c *sidebarTestCatalog) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fullReads
}

// startSidebarFixture runs the real materializer loop over the sidebar
// fixture and the wrapped catalog.
func startSidebarFixture(t *testing.T) (*server, *sidebarTestCatalog) {
	t.Helper()
	s, _, store := sidebarActiveFixtureWithStore(t, time.Now())
	c := &sidebarTestCatalog{store: store, threadCatalogService: s.catalog.(*threadCatalogService), changes: make(chan catalog.Change, 1)}
	s.catalog = c
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.startSidebarMaterializer(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	return s, c
}

// seedLateThread commits a new working thread that only a full rebuild shows.
func seedLateThread(t *testing.T, c *sidebarTestCatalog, native string) func(sidebarThread) bool {
	t.Helper()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := c.store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	seedSidebarThread(t, c.store, root, native, "", history.ThreadMain, time.Now().Add(-5*time.Minute),
		[]historyCourt{{user: true, text: native}})
	return func(row sidebarThread) bool { return row.NativeID == native }
}

func awaitSidebarRow(t *testing.T, s *server, match func(sidebarThread) bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, ok := activeSidebarRow(t, s, match); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the committed catalog change never reached the sidebar")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// publish hands the loop one catalog change and waits until it takes it.
func (c *sidebarTestCatalog) publish(t *testing.T, change catalog.Change) {
	t.Helper()
	c.changes <- change
	deadline := time.Now().Add(time.Second)
	for len(c.changes) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the materializer did not take the catalog change")
		}
		time.Sleep(time.Millisecond)
	}
}

func activeSidebarRow(t *testing.T, s *server, match func(sidebarThread) bool) (sidebarThread, bool) {
	t.Helper()
	snapshot, err := s.sidebar.activeSnapshot(threadindex.ListQuery{}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range snapshot.Items {
		if match(row) {
			return row, true
		}
	}
	return sidebarThread{}, false
}

// A busy catalog spell turned every merged burst into another full SQLite
// read of the sidebar graph, back to back (13.5% of live Nano CPU in a spike
// profile). A burst must share one trailing rebuild, which still shows the
// burst's last committed change.
func TestSidebarCatalogBurstSharesOneTrailingRebuild(t *testing.T) {
	s, c := startSidebarFixture(t)
	before := c.reads()
	isLate := seedLateThread(t, c, "late-recent")
	for range 3 {
		c.publish(t, catalog.Change{Refresh: true})
	}

	time.Sleep(300 * time.Millisecond)
	if _, ok := activeSidebarRow(t, s, isLate); ok || c.reads() != before {
		t.Fatalf("a burst inside the interval rebuilt at once (%d full reads)", c.reads()-before)
	}
	awaitSidebarRow(t, s, isLate, sidebarRebuildMinInterval+3*time.Second)
	if got := c.reads() - before; got != 1 {
		t.Fatalf("burst caused %d full rebuilds, want 1", got)
	}
}

// A runtime refresh rebuilds at once and covers a waiting catalog rebuild,
// but only if it succeeds: a failed read used to cancel the waiting one, and
// the committed row never appeared.
func TestSidebarFailedRuntimeRebuildKeepsWaitingCatalogRebuild(t *testing.T) {
	s, c := startSidebarFixture(t)
	isLate := seedLateThread(t, c, "pending-catalog")
	c.publish(t, catalog.Change{Refresh: true})
	c.mu.Lock()
	c.failNext = true
	c.mu.Unlock()
	s.runtimeEvents.Notify(sidebar.RuntimeDelta{Refresh: true})

	awaitSidebarRow(t, s, isLate, sidebarRebuildMinInterval+3*time.Second)
}

// Some refresh markers carry no rows. Applied as a snapshot, one cleared every
// runtime overlay, and an unrelated working thread left the active sidebar.
func TestSidebarRuntimeRefreshMarkerKeepsOtherRuntime(t *testing.T) {
	s, _ := startSidebarFixture(t)
	events := s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)

	s.runtimeEvents.Notify(sidebar.RuntimeDelta{Refresh: true})
	select {
	case <-events:
	case <-time.After(500 * time.Millisecond):
	}
	row, ok := activeSidebarRow(t, s, func(row sidebarThread) bool { return row.NativeID == "related-parent" })
	if !ok || row.Runtime == nil || !row.Runtime.Attached || !row.Runtime.TurnActive {
		t.Fatalf("unrelated working thread lost its runtime: present=%v runtime=%+v", ok, row.Runtime)
	}
}

// A session's native bind changes its sidebar identity; it must show at once,
// not wait for a paced rebuild beside a stale memory row.
func TestSidebarNativeBindShowsAtOnce(t *testing.T) {
	s, _ := sidebarActiveFixture(t, time.Now())
	sess := memorySession(t, "pending-bind", "codex", "", "starting", "/fixture", "gpt-test", time.Now())
	s.RegisterSession(sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.startSidebarMaterializer(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.runtimeDeltaSnapshot()
	events := s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)

	sess.ReserveNativeID("native-bound")
	s.notifyRuntime()
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("no sidebar update after the native bind")
	}
	row, ok := activeSidebarRow(t, s, func(row sidebarThread) bool { return row.ID == "mem_pending-bind" })
	if !ok || row.NativeID != "native-bound" || row.Runtime == nil {
		t.Fatalf("memory row kept its old identity: present=%v native=%q runtime=%+v", ok, row.NativeID, row.Runtime)
	}
}

// A rebuild and a runtime delta stamped metadataRevision differently, so each
// rebuild republished every held row and the next delta flipped them all back.
// A rebuild that changes nothing publishes nothing, and keeps the delta's revision on the row.
func TestSidebarRebuildAgreesWithRuntimeDelta(t *testing.T) {
	s, _ := startSidebarFixture(t)
	for revision := s.controlMetadata.LatestRevision("nht_related_parent"); revision < 7; revision++ {
		if _, err := s.controlMetadata.BeginTransportBinding("nht_related_parent"); err != nil {
			t.Fatal(err)
		}
	}
	events := s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)
	delta := s.runtimeDeltaSnapshot()
	delta.Refresh, delta.Rebuild = false, false // patch in place, as an ordinary turn-state change does
	s.runtimeEvents.Notify(delta)
	select {
	case <-events:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the runtime delta published no sidebar delta")
	}

	if err := s.materializeSidebar(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case published := <-events:
		t.Fatalf("a rebuild that changed nothing republished %d rows, removed %d", len(published.Rows), len(published.RemovedIDs))
	case <-time.After(300 * time.Millisecond):
	}
	row, ok := activeSidebarRow(t, s, func(row sidebarThread) bool { return row.NativeID == "related-parent" })
	if !ok || row.Runtime == nil || row.Runtime.MetadataRevision != 7 {
		t.Fatalf("rebuilt row lost the delta's metadata revision: present=%v runtime=%+v", ok, row.Runtime)
	}
}

// A delta names only the threads whose runtime changed. Applied as a
// snapshot, one small delta cleared every other row's runtime and the working
// threads it left out fell off the active sidebar.
func TestSidebarPartialRuntimeDeltaKeepsUnnamedRuntime(t *testing.T) {
	s, _ := startSidebarFixture(t)
	events := s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)
	s.runtimeEvents.Notify(sidebar.RuntimeDelta{Rows: []sidebar.RuntimeRow{{
		Provider: "codex", NativeID: "attached-recent",
		Runtime: &sidebar.RuntimeInfo{Attached: true, SessionID: "sess-attached", Status: "ready", TurnActive: true, ThreadStatus: "working"},
	}}})
	select {
	case <-events:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the partial delta published no sidebar delta")
	}
	row, ok := activeSidebarRow(t, s, func(row sidebarThread) bool { return row.NativeID == "related-parent" })
	if !ok || row.Runtime == nil || !row.Runtime.Attached || !row.Runtime.TurnActive {
		t.Fatalf("a thread the delta did not name lost its runtime: present=%v runtime=%+v", ok, row.Runtime)
	}
}

// Revisions count from 1 again after a restart, so the list, every delta and
// the connect marker name the process they belong to; a browser that read the
// list from the previous process must read it again rather than trust a
// matching revision.
func TestSidebarGenerationNamesTheProcess(t *testing.T) {
	s, _ := startSidebarFixture(t)
	snapshot, err := s.sidebar.activeSnapshot(threadindex.ListQuery{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation == "" {
		t.Fatal("the active list names no generation")
	}
	marker, ok := s.sidebar.marker(snapshot.CatalogVersion)
	if !ok || !marker.Refresh || marker.Generation != snapshot.Generation || marker.Revision != snapshot.Revision {
		t.Fatalf("connect marker = %+v ok=%v, want a refresh at the list's generation and revision", marker, ok)
	}
	events := s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)
	s.runtimeEvents.Notify(sidebar.RuntimeDelta{Rows: []sidebar.RuntimeRow{{
		Provider: "codex", NativeID: "attached-recent",
		Runtime: &sidebar.RuntimeInfo{Attached: true, SessionID: "sess-attached", Status: "ready", TurnActive: true, ThreadStatus: "working"},
	}}})
	select {
	case delta := <-events:
		if delta.Generation != snapshot.Generation || delta.Revision != snapshot.Revision+1 {
			t.Fatalf("delta = generation %q revision %d, want %q and %d", delta.Generation, delta.Revision, snapshot.Generation, snapshot.Revision+1)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the runtime delta published no sidebar delta")
	}
	if other := newSidebarMaterializedState(); other.generation == s.sidebar.generation {
		t.Fatal("two processes share one generation")
	}
}

// A newer index time alone is not a row change: about two thirds of the
// sidebar events a busy fleet sent resent a row whose only change was
// source.lastIndexedAt, which no row shows, and each made every open page
// rebuild its sidebar while typing waited. The event still carries the new
// catalog version, so pages keep skipping the catalog patches it covers. A
// changed diagnostic count still resends the row, because rows show it.
// Boundary: the materialized sidebar's published deltas.
func TestSidebarIndexTimeAloneResendsNoRow(t *testing.T) {
	row := func(indexedAt string, diagnostics int) sidebarThread {
		var thread sidebarThread
		thread.ID, thread.Provider, thread.NativeID = "nth_row", "codex", "native-row"
		thread.Source = threadindex.SourceSummary{Status: "present", DiagnosticCount: diagnostics, LastIndexedAt: indexedAt}
		return thread
	}
	active := func(item sidebarThread, version int64) sidebarActiveResponse {
		return sidebarActiveResponse{Items: []sidebarThread{item}, Roots: []string{item.ID}, CatalogVersion: version}
	}
	next := func(events chan sidebarDelta) *sidebarDelta {
		select {
		case delta := <-events:
			return &delta
		default:
			return nil
		}
	}
	m := newSidebarMaterializedState()
	m.replace(nil, threadindex.ListQuery{}, active(row("2026-10-28T20:00:00Z", 0), 1), nil, 1)
	events := m.events.subscribe()
	m.replace(nil, threadindex.ListQuery{}, active(row("2026-10-28T20:00:05Z", 0), 1), nil, 1)
	if delta := next(events); delta != nil {
		t.Fatalf("a newer index time alone sent a delta: %+v", *delta)
	}
	m.replace(nil, threadindex.ListQuery{}, active(row("2026-10-28T20:00:10Z", 0), 2), nil, 2)
	if delta := next(events); delta == nil || len(delta.Rows) != 0 || delta.CatalogVersion != 2 {
		t.Fatalf("a newer index time and catalog version sent %+v, want no rows at version 2", delta)
	}
	m.replace(nil, threadindex.ListQuery{}, active(row("2026-10-28T20:00:15Z", 2), 3), nil, 3)
	if delta := next(events); delta == nil || len(delta.Rows) != 1 || delta.Rows[0].Source.DiagnosticCount != 2 {
		t.Fatalf("a changed diagnostic count sent %+v, want its row", delta)
	}
	if m.active.Items[0].Source.LastIndexedAt != "2026-10-28T20:00:15Z" {
		t.Fatalf("a full read would return index time %q", m.active.Items[0].Source.LastIndexedAt)
	}
}
