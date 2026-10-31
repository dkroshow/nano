package sidebar

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/threadindex"
)

// sidebarTestCatalog drives the real catalog's name-change publication and
// counts the materializer's completed rebuild diagnostics.
type sidebarTestCatalog struct {
	store      *history.Store
	service    *threadindex.Service
	controller *Controller
	logs       *testsupport.LogBuffer
	sequence   int
}

func (c *sidebarTestCatalog) reads() int {
	return strings.Count(c.logs.String(), "sidebar full rebuild (")
}
func startSidebarFixture(t *testing.T) (*Controller, *sidebarTestCatalog) {
	t.Helper()
	s, _, store := sidebarActiveFixtureWithStore(t, time.Now())
	logs := &testsupport.LogBuffer{}
	prior := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prior) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.StartSidebarMaterializer(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	return s, &sidebarTestCatalog{store: store, service: s.catalog, controller: s, logs: logs}
}

// seedLateThread commits a new thread that only a full rebuild reads.
func seedLateThread(t *testing.T, c *sidebarTestCatalog, native string) func(Thread) bool {
	t.Helper()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := c.store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	seedSidebarThread(t, c.store, root, native, "", history.ThreadMain, time.Now().Add(-5*time.Minute),
		[]historyCourt{{user: true, text: native}})
	return func(row Thread) bool { return row.NativeID == native }
}

func materializedRow(s *Controller, match func(Thread) bool) bool {
	rows, _ := s.rows()
	return slices.ContainsFunc(rows, match)
}

func awaitSidebarRow(t *testing.T, s *Controller, match func(Thread) bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if materializedRow(s, match) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the committed catalog change never reached the sidebar")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// publish commits a name change, then waits for the catalog's real feed.
func (c *sidebarTestCatalog) publish(t *testing.T, change catalog.Change) {
	t.Helper()
	if len(change.ThreadIDs) > 0 {
		if !c.controller.repairSidebarCatalogRows(context.Background(), change) {
			t.Fatal("catalog row repair failed")
		}
		return
	}
	events, stop := c.service.SubscribeCatalog()
	defer stop()
	c.sequence++
	if err := c.service.AssignThreadName("codex", "idle-recent", fmt.Sprintf("catalog event %d", c.sequence)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("the catalog did not publish the name change")
	}
	time.Sleep(10 * time.Millisecond)
}

func activeSidebarRow(t *testing.T, s *Controller, match func(sidebarThread) bool) (sidebarThread, bool) {
	t.Helper()
	snapshot, err := s.activeSnapshot(threadindex.ListQuery{}, true)
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
	if materializedRow(s, isLate) || c.reads() != before {
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
	s.updateMu.Lock()
	savedCatalog := s.catalog
	s.catalog = nil
	s.updateMu.Unlock()
	s.runtimeEvents.Notify(RuntimeDelta{Refresh: true})
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(c.logs.String(), "sidebar runtime rebuild:") {
		if time.Now().After(deadline) {
			t.Fatal("runtime rebuild did not report the unavailable source")
		}
		time.Sleep(time.Millisecond)
	}
	s.updateMu.Lock()
	s.catalog = savedCatalog
	s.updateMu.Unlock()

	awaitSidebarRow(t, s, isLate, sidebarRebuildMinInterval+3*time.Second)
}

// Some refresh markers carry no rows. Applied as a snapshot, one cleared every
// runtime overlay, and an unrelated working thread left the active
func TestSidebarRuntimeRefreshMarkerKeepsOtherRuntime(t *testing.T) {
	s, _ := startSidebarFixture(t)
	events := s.Subscribe()
	defer s.Unsubscribe(events)

	s.runtimeEvents.Notify(RuntimeDelta{Refresh: true})
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
	if err := s.StartSidebarMaterializer(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.runtimeDeltaSnapshot()
	events := s.Subscribe()
	defer s.Unsubscribe(events)

	sess.ReserveNativeID("native-bound")
	s.PublishRuntimeDelta(false, false)
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
	events := s.Subscribe()
	defer s.Unsubscribe(events)
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
// threads it left out fell off the active
func TestSidebarPartialRuntimeDeltaKeepsUnnamedRuntime(t *testing.T) {
	s, _ := startSidebarFixture(t)
	events := s.Subscribe()
	defer s.Unsubscribe(events)
	s.runtimeEvents.Notify(RuntimeDelta{Rows: []RuntimeRow{{
		Provider: "codex", NativeID: "attached-recent",
		Runtime: &RuntimeInfo{Attached: true, SessionID: "sess-attached", Status: "ready", TurnActive: true, ThreadStatus: "working"},
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
	snapshot, err := s.activeSnapshot(threadindex.ListQuery{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation == "" {
		t.Fatal("the active list names no generation")
	}
	marker, ok := s.Marker(snapshot.CatalogVersion)
	if !ok || !marker.Refresh || marker.Generation != snapshot.Generation || marker.Revision != snapshot.Revision {
		t.Fatalf("connect marker = %+v ok=%v, want a refresh at the list's generation and revision", marker, ok)
	}
	events := s.Subscribe()
	defer s.Unsubscribe(events)
	s.runtimeEvents.Notify(RuntimeDelta{Rows: []RuntimeRow{{
		Provider: "codex", NativeID: "attached-recent",
		Runtime: &RuntimeInfo{Attached: true, SessionID: "sess-attached", Status: "ready", TurnActive: true, ThreadStatus: "working"},
	}}})
	select {
	case delta := <-events:
		if delta.Generation != snapshot.Generation || delta.Revision != snapshot.Revision+1 {
			t.Fatalf("delta = generation %q revision %d, want %q and %d", delta.Generation, delta.Revision, snapshot.Generation, snapshot.Revision+1)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("the runtime delta published no sidebar delta")
	}
	if other := New(Sources{}); other.generation == s.generation {
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
	next := func(events chan Delta) *Delta {
		select {
		case delta := <-events:
			return &delta
		default:
			return nil
		}
	}
	m := New(Sources{})
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
