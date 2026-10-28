package threadindex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// Failure caught: a burst of writes to one attached file while its pass runs
// either started a parse per write or lost the writes that landed after the
// pass read its snapshot; and every pass waited out a settle window first.
// Boundary: the catalog worker's queue, with a pass held inside its parse.
func TestThreadCatalogPassHasOneInFlightAndOnePendingAndNoSettle(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "10000000-0000-4000-8000-000000000001"
	path := filepath.Join(projectPath, nativeID+".jsonl")
	writeClaudeCatalogFixture(t, path, nativeID, "2026-09-15T10:00:00Z", "first")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var parses atomic.Int32
	var service *Service
	service, err = newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}}, DisableWatcher: true,
		projectSource: func(ctx context.Context, candidate history.SourceCandidate, stored history.StoredSource, exists bool,
			header history.TranscriptCheckpoint, hasCheckpoint bool, at time.Time) (history.SourceProjection, *preparedTranscript, error) {
			// Read the snapshot first, as a real pass does, then hold.
			input, prepared, err := service.projectSource(ctx, candidate, stored, exists, header, hasCheckpoint, at)
			parses.Add(1)
			entered <- struct{}{}
			<-release
			return input, prepared, err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetTranscriptChanged(func(context.Context, TranscriptChange) {})
	waitCatalogReady(t, service)

	hinted := time.Now()
	service.requestPathRefresh([]string{path})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("a hint on a quiet worker did not start its pass at once")
	}
	if waited := time.Since(hinted); waited > 500*time.Millisecond {
		t.Fatalf("the pass started %s after its hint, want no settle wait", waited)
	}
	// Writes during the pass: the pass read before them, so exactly one more
	// pass follows, however many hints arrive.
	for i := 0; i < 20; i++ {
		appendCatalogRow(t, path, claudeCatalogUserRow(nativeID, "2026-09-15T10:01:00Z", fmt.Sprintf("burst %d", i)))
		service.requestPathRefresh([]string{path})
	}
	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hints during the pass did not schedule the pending pass")
	}
	release <- struct{}{}
	select {
	case <-entered:
		t.Fatal("a third pass ran for hints that one pending pass covered")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	waitForCatalog(t, func() bool { return catalogCompleteLines(t, service, nativeID) == 22 })
	if got := parses.Load(); got != 2 {
		t.Fatalf("parses = %d, want 2", got)
	}
}

// Failure caught: a restart re-walked every provider root (the boot sweep),
// or stamped a completed scan it never ran. Boundary: a restarted service
// over a saved catalog: it parses nothing, logs no walk, keeps the root's
// last completion time and reports it ready at once.
func TestThreadCatalogBootWalksNothingAndIsReady(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "20000000-0000-4000-8000-000000000001"
	writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID, "2026-09-15T11:00:00Z", "persisted")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}
	first, err := newTestCatalogService(context.Background(), store, Options{Roots: []history.SourceRoot{rootSpec}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	first.SetTranscriptChanged(func(context.Context, TranscriptChange) {})
	first.RequestRefresh()
	waitForCatalog(t, func() bool {
		status, err := first.IndexStatus(context.Background())
		return err == nil && status.State == "ready" && status.Roots[0].SourceCount == 1
	})
	before, err := first.IndexStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	// A crash mid-walk leaves the root scanning.
	if err := store.SetIndexState(context.Background(), onlyCatalogRoot(t, store).ID, history.IndexScanning, nil); err != nil {
		t.Fatal(err)
	}

	scans := &catalogScanLog{}
	var parses atomic.Int32
	second, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{rootSpec}, DisableWatcher: true, Logf: scans.logf,
		projectSourceStarted: func(history.SourceCandidate) { parses.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waitCatalogReady(t, second)
	after, err := second.IndexStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "ready" || !reflect.DeepEqual(after.Roots[0].LastCompletedAt, before.Roots[0].LastCompletedAt) || after.Roots[0].SourceCount != 1 {
		t.Fatalf("boot status = %#v, want ready with the last walk's completion %v", after.Roots[0], before.Roots[0].LastCompletedAt)
	}
	time.Sleep(100 * time.Millisecond)
	if parses.Load() != 0 || len(scans.snapshot()) != 0 {
		t.Fatalf("boot parsed %d files and walked %d times, want none", parses.Load(), len(scans.snapshot()))
	}
}

// Failure caught: Explorer's Refresh ran a freshness walk, which neither
// retries errored checkpoints nor proves absence, or it dropped attached
// work queued behind it. Boundary: RequestRefresh on a service whose root
// holds a file no hint named and a file that was removed.
func TestThreadCatalogRefreshIsRecoveryAndKeepsQueuedWork(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	keptID, removedID, outsideID := "40000000-0000-4000-8000-000000000001", "40000000-0000-4000-8000-000000000002", "40000000-0000-4000-8000-000000000003"
	kept := filepath.Join(projectPath, keptID+".jsonl")
	removed := filepath.Join(projectPath, removedID+".jsonl")
	outside := filepath.Join(projectPath, outsideID+".jsonl")
	writeClaudeCatalogFixture(t, kept, keptID, "2026-09-15T13:00:00Z", "kept")
	writeClaudeCatalogFixture(t, removed, removedID, "2026-09-15T13:00:00Z", "removed")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scans := &catalogScanLog{}
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}}, DisableWatcher: true, Logf: scans.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetTranscriptChanged(func(context.Context, TranscriptChange) {})
	service.requestPathRefresh([]string{kept, removed})
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		return err == nil && len(page.Items) == 2
	})
	writeClaudeCatalogFixture(t, outside, outsideID, "2026-09-15T13:01:00Z", "outside")
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	appendCatalogRow(t, kept, claudeCatalogUserRow(keptID, "2026-09-15T13:02:00Z", "kept, changed"))
	// A hint for another file imports nothing it did not name.
	service.requestPathRefresh([]string{kept})
	waitForCatalog(t, func() bool { return catalogCompleteLines(t, service, keptID) == 3 })
	if page, _ := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10}); len(page.Items) != 2 {
		t.Fatalf("a targeted pass imported an unnamed file: %d threads", len(page.Items))
	}

	service.RequestRefresh()
	service.requestPathRefresh([]string{kept})
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != 3 {
			return false
		}
		item, err := service.LoadThread(context.Background(), catalogIDFor(t, service, removedID))
		return err == nil && item.Source.Status == "source_missing"
	})
	waitForCatalog(t, func() bool {
		service.queueMu.Lock()
		defer service.queueMu.Unlock()
		return len(service.dirty) == 0
	})
	if lines := scans.snapshot(); len(lines) != 1 {
		t.Fatalf("recovery walks = %q, want exactly the one requested", lines)
	}
}

func appendCatalogRow(t *testing.T, path, row string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(row); err != nil {
		t.Fatal(err)
	}
}

// catalogCompleteLines is the committed transcript's complete source lines
// for one native thread, or -1 before it has one.
func catalogCompleteLines(t *testing.T, service *Service, nativeID string) int {
	t.Helper()
	id := catalogIDFor(t, service, nativeID)
	if id == "" {
		return -1
	}
	checkpoint, exists, err := service.store.LoadTranscriptCheckpointHeader(context.Background(), id)
	if err != nil || !exists {
		return -1
	}
	return checkpoint.CompleteLines
}

func catalogIDFor(t *testing.T, service *Service, nativeID string) string {
	t.Helper()
	id, err := service.ResolveThreadReference(context.Background(), nativeID)
	if err != nil {
		return ""
	}
	return id
}

func waitCatalogReady(t *testing.T, service *Service) {
	t.Helper()
	select {
	case <-service.catalogReady:
	case <-time.After(5 * time.Second):
		t.Fatal("catalog did not reach ready")
	}
}

func waitTranscriptChangeDrain(t *testing.T, service *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if backlog, inFlight := service.transcriptChangeStatus(); backlog == 0 && inFlight == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	backlog, inFlight := service.transcriptChangeStatus()
	t.Fatalf("transcript callbacks did not drain: backlog=%d in-flight=%d", backlog, inFlight)
}

func onlyCatalogRoot(t *testing.T, store *history.Store) history.PersistedRoot {
	t.Helper()
	roots, err := store.ListRoots(context.Background())
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots=%v err=%v", roots, err)
	}
	return roots[0]
}

func mustCatalogSource(t *testing.T, store *history.Store, rootID int64, path string) history.StoredSource {
	t.Helper()
	source, exists, err := store.LoadSource(context.Background(), rootID, path)
	if err != nil || !exists {
		t.Fatalf("source %s exists=%v err=%v", filepath.Base(path), exists, err)
	}
	return source
}

func TestThreadCatalogParsesAtMostTwoFilesConcurrently(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		nativeID := fmt.Sprintf("30000000-0000-4000-8000-%012d", index)
		writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID, "2026-09-15T12:00:00Z", "concurrent")
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var active, maximum atomic.Int32
	gate := make(chan struct{})
	started := make(chan struct{}, 3)
	project := func(context.Context, history.SourceCandidate, history.StoredSource, bool, history.TranscriptCheckpoint, bool, time.Time) (history.SourceProjection, *preparedTranscript, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-gate
		active.Add(-1)
		return history.SourceProjection{}, nil, errors.New("injected parse stop")
	}
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}}, DisableWatcher: true, projectSource: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	defer close(gate)
	service.RequestRefresh()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("two parse jobs did not start in parallel")
		}
	}
	select {
	case <-started:
		t.Fatal("third parse job started before one of two workers yielded")
	case <-time.After(50 * time.Millisecond):
	}
	gate <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("third parse job did not start after a worker yielded")
	}
	gate <- struct{}{}
	gate <- struct{}{}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent parses=%d, want 2", maximum.Load())
	}
}
