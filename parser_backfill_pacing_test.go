package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// A parser-version bump must not re-project every source in one recovery
// sweep. A byte-identical source with a healthy transcript
// checkpoint is left to the paced backfill; a source with no checkpoint, or
// with a checkpoint error, is still the scan's (metadata-only reclassification
// and error recovery). parserBudgetFixture indexes one Codex session file and
// then records it back at an older parser version with its bytes unchanged:
// the state every source is in right after a parser-version bump.
type parserBudgetFixture struct {
	ctx       context.Context
	store     *history.Store
	service   *threadCatalogService
	root      history.SourceRoot
	rootID    int64
	paths     []string
	path      string // the first file
	threadID  string
	sourceID  int64
	projected atomic.Int64
	sweeps    chan struct{}
	dbPath    string
}

const staleParserVersion = "codex-parser-before-bump"

func newParserBudgetFixture(t *testing.T, files int) *parserBudgetFixture {
	t.Helper()
	f, threadIDs := newIndexedCodexFixture(t, files)
	// Record every source and transcript back at the stale parser version.
	for i, path := range f.paths {
		stored := f.storedAt(t, path)
		threadID := threadIDs[path]
		f.markStale(t, stored, threadID)
		if i == 0 {
			f.threadID, f.sourceID = threadID, f.storedAt(t, path).ID
		}
	}
	if f.stored(t).ParserVersion != staleParserVersion || f.checkpointVersion(t) != staleParserVersion {
		t.Fatalf("fixture did not record the stale parser version: source %q, transcript %q", f.stored(t).ParserVersion, f.checkpointVersion(t))
	}
	return f
}

// newIndexedCodexFixture indexes files real Codex sessions through the
// catalog, as a live sweep does, and reports each file's thread id.
func newIndexedCodexFixture(t *testing.T, files int) (*parserBudgetFixture, map[string]string) {
	t.Helper()
	f := &parserBudgetFixture{ctx: context.Background(), sweeps: make(chan struct{}, 8)}
	rootPath := filepath.Join(t.TempDir(), "sessions")
	dayPath := filepath.Join(rootPath, "2026", "09", "24")
	if err := os.MkdirAll(dayPath, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < files; i++ {
		nativeID := fmt.Sprintf("24242424-2424-4242-8242-2424242424%02d", i)
		path := filepath.Join(dayPath, fmt.Sprintf("rollout-2026-09-24T20-00-%02d-%s.jsonl", i, nativeID))
		contents := fmt.Sprintf("{\"timestamp\":\"2026-09-24T20:00:00.000Z\",\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", nativeID) +
			"{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Prompt\"}}\n"
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		f.paths = append(f.paths, path)
	}
	f.path = f.paths[0]
	f.dbPath = filepath.Join(t.TempDir(), "history.sqlite")
	store, err := history.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}
	f.root = root
	service, err := newThreadCatalogService(f.ctx, store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{root}, DisableWatcher: true,
		projectSourceStarted: func(history.SourceCandidate) { f.projected.Add(1) },
		Logf: func(format string, args ...any) {
			if strings.HasPrefix(format, "thread catalog recovery") {
				f.sweeps <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	f.service = service
	service.RequestRefresh()
	threadIDs := map[string]string{}
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(f.ctx, catalogListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != files {
			return false
		}
		for _, item := range page.Items {
			if _, exists, err := store.LoadTranscriptCheckpoint(f.ctx, item.ID); err != nil || !exists {
				return false
			}
			threadIDs[item.NativeID] = item.ID
		}
		return true
	})
	persisted, err := store.EnsureRoot(f.ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	f.rootID = persisted.ID
	byPath := map[string]string{}
	for _, path := range f.paths {
		byPath[path] = threadIDs[strings.TrimSuffix(filepath.Base(path)[len("rollout-2026-09-24T20-00-00-"):], ".jsonl")]
	}
	f.threadID, f.sourceID = byPath[f.path], f.storedAt(t, f.path).ID
	return f, byPath
}

func (f *parserBudgetFixture) markStale(t *testing.T, stored history.StoredSource, threadID string) {
	t.Helper()
	file, err := os.Open(stored.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := history.ProjectParsedFile(f.ctx, history.ParsedFileRequest{Source: stored.Source, Reader: file, SpoolDir: t.TempDir()})
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	metadata := parsed.Metadata
	if _, err := f.store.ApplySource(f.ctx, history.SourceProjection{Source: stored.Source, Result: &metadata,
		ParserVersion: staleParserVersion, Status: history.SourcePresent, IndexedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	stored = f.storedAt(t, stored.Source.Path)
	checkpoint, _, err := f.store.LoadTranscriptCheckpoint(f.ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ApplyTranscript(f.ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: threadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: history.ProviderCodex, ParserVersion: staleParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion,
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: parsed.Transcript,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *parserBudgetFixture) stored(t *testing.T) history.StoredSource {
	t.Helper()
	return f.storedAt(t, f.path)
}

func (f *parserBudgetFixture) storedAt(t *testing.T, path string) history.StoredSource {
	t.Helper()
	stored, ok, err := f.store.LoadSource(f.ctx, f.rootID, path)
	if err != nil || !ok {
		t.Fatalf("stored source = %v, %v", ok, err)
	}
	return stored
}

func (f *parserBudgetFixture) checkpointVersion(t *testing.T) string {
	t.Helper()
	checkpoint, exists, err := f.store.LoadTranscriptCheckpoint(f.ctx, f.threadID)
	if err != nil || !exists {
		t.Fatalf("checkpoint = %v, %v", exists, err)
	}
	return checkpoint.ParserVersion
}

// sweep runs one recovery sweep and returns how many sources it re-projected.
func (f *parserBudgetFixture) sweep(t *testing.T) int64 {
	t.Helper()
	for len(f.sweeps) > 0 {
		<-f.sweeps
	}
	f.projected.Store(0)
	f.service.RequestRefresh()
	select {
	case <-f.sweeps:
	case <-time.After(30 * time.Second):
		t.Fatal("recovery sweep did not finish")
	}
	return f.projected.Load()
}

// TestParserVersionChangeIsNotScanWork catches the storm a parser-version bump
// causes: one recovery sweep re-projected every byte-identical source of the
// provider at once (every Codex file on a Codex parser bump). Boundary: one
// recovery sweep, and an attachment's catch-up, over a source whose only
// difference is its parser version and whose transcript checkpoint is
// healthy; it must be left unchanged and remain a backfill candidate.
func TestParserVersionChangeIsNotScanWork(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	if projected := f.sweep(t); projected != 0 {
		t.Fatalf("the sweep re-projected %d parser-behind sources with a healthy checkpoint; want 0 (the paced backfill owns them)", projected)
	}
	if got := f.stored(t).ParserVersion; got != staleParserVersion {
		t.Fatalf("source parser version = %q after the sweep, want it left at %q for the backfill", got, staleParserVersion)
	}
	// After a parser-bump deploy every attached thread catches up at once;
	// that must verify, not rebuild, a healthy parser-behind transcript.
	f.projected.Store(0)
	f.service.holdAttachedSource("attached", []string{f.path})
	catchUp := f.service.attachedCatchUpFor("attached")
	select {
	case <-catchUp.done:
	case <-time.After(30 * time.Second):
		t.Fatal("attached catch-up did not finish")
	}
	if result := f.service.catchUpResultOf(catchUp); result != catchUpReady || f.projected.Load() != 0 {
		t.Fatalf("attached catch-up = %v after %d projections; want ready after none", result, f.projected.Load())
	}
	candidates, err := f.store.ListTranscriptParserBackfillCandidates(f.ctx, 10)
	if err != nil || len(candidates) != 1 || candidates[0].ThreadID != f.threadID {
		t.Fatalf("backfill candidates = %#v, %v; want this thread", candidates, err)
	}
}

// TestParserVersionChangeWithCheckpointErrorStaysScanWork guards recovery: a
// parser-behind source whose checkpoint records an error is still re-projected
// by the sweep. Boundary: one recovery sweep after MarkTranscriptError.
func TestParserVersionChangeWithCheckpointErrorStaysScanWork(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	if err := f.store.MarkTranscriptError(f.ctx, f.threadID, f.sourceID, "stale projection failed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if projected := f.sweep(t); projected != 1 {
		t.Fatalf("the sweep re-projected %d sources with a checkpoint error; want 1", projected)
	}
	if got, want := f.stored(t).ParserVersion, parserVersion(history.ProviderCodex); got != want {
		t.Fatalf("source parser version = %q after recovery, want %q", got, want)
	}
}

// TestParserBackfillAdvancesSourceAndTranscriptVersions guards the paced path:
// one backfill batch brings both the source's and the transcript's stored
// parser versions to current. Boundary: RunTranscriptParserBackfillBatch on a
// byte-identical parser-behind source.
func TestParserBackfillAdvancesSourceAndTranscriptVersions(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	rebuilt, _, err := f.service.RunTranscriptParserBackfillBatch(f.ctx, 1)
	if err != nil || rebuilt != 1 {
		t.Fatalf("backfill rebuilt = %d, %v", rebuilt, err)
	}
	if got, want := f.stored(t).ParserVersion, parserVersion(history.ProviderCodex); got != want {
		t.Fatalf("source parser version = %q, want %q", got, want)
	}
	if got, want := f.checkpointVersion(t), history.CurrentTranscriptParserVersion(history.ProviderCodex); got != want {
		t.Fatalf("transcript parser version = %q, want %q", got, want)
	}
}

// TestParserBackfillPauseFileHoldsBatches catches a pause that does not hold:
// an operator creates the pause file to stop a long parser upgrade without a
// deploy, and a batch still rebuilds, including one that was already waiting
// for a scan when the file appeared. Boundary: RunTranscriptParserBackfillBatch,
// the scheduler's batch. With the file present it rebuilds nothing and the
// transcript stays behind; once the file is gone the next batch rebuilds it.
func TestParserBackfillPauseFileHoldsBatches(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	pause := filepath.Join(t.TempDir(), "parser-backfill.pause")
	if err := os.WriteFile(pause, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	empty := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexArchived, Path: t.TempDir()}
	service, err := newThreadCatalogService(f.ctx, f.store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{empty}, DisableWatcher: true,
		ParserBackfillPauseFile: pause,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	<-service.catalogReady
	if rebuilt, _, err := service.RunTranscriptParserBackfillBatch(f.ctx, 1); err != nil || rebuilt != 0 {
		t.Fatalf("paused batch rebuilt %d, %v; want 0", rebuilt, err)
	}
	if got := f.checkpointVersion(t); got != staleParserVersion {
		t.Fatalf("transcript parser version = %q while paused, want %q", got, staleParserVersion)
	}
	// A pause made while a batch waits behind a scan holds that batch too.
	if err := os.Remove(pause); err != nil {
		t.Fatal(err)
	}
	service.projectionMu.Lock()
	waiting := make(chan int, 1)
	go func() {
		rebuilt, _, _ := service.RunTranscriptParserBackfillBatch(f.ctx, 1)
		waiting <- rebuilt
	}()
	// The wait only has to outlast the batch's start; the result does not
	// depend on it once the check follows admission.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(pause, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	service.projectionMu.Unlock()
	if rebuilt := <-waiting; rebuilt != 0 {
		t.Fatalf("batch paused while it waited rebuilt %d; want 0", rebuilt)
	}
	if err := os.Remove(pause); err != nil {
		t.Fatal(err)
	}
	if rebuilt, _, err := service.RunTranscriptParserBackfillBatch(f.ctx, 1); err != nil || rebuilt != 1 {
		t.Fatalf("resumed batch rebuilt %d, %v; want 1", rebuilt, err)
	}
	if got, want := f.checkpointVersion(t), history.CurrentTranscriptParserVersion(history.ProviderCodex); got != want {
		t.Fatalf("transcript parser version = %q after resume, want %q", got, want)
	}
}

// TestParserBackfillCoolsDownBetweenBatches catches the backfill running its
// batches back to back: after a bump every parser-behind transcript is a
// candidate, and the scheduler started the next batch at once, so a parser
// bump re-projected every file of a provider without a pause. It also catches
// a cooldown that counts the time a batch waited behind a scan: one long wait
// then stalled the backfill for twice that wait. Boundary: the scheduler on a
// store holding three parser-behind sources, batch size 1, timed through its
// batch log line. The second batch ends no sooner than twice the first
// batch's work after it (at most a third of one core on average); the second
// batch waits behind a held scan lock, and the third still follows it within
// twice its work plus its own work, well inside that wait.
func TestParserBackfillCoolsDownBetweenBatches(t *testing.T) {
	f := newParserBudgetFixture(t, 3)
	f.service.Close()
	type batch struct {
		at      time.Time
		elapsed time.Duration
	}
	batches := make(chan batch, 8)
	// One explicit empty root: no scan touches the parser-behind sources, and
	// the service never falls back to the real provider roots.
	empty := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexArchived, Path: t.TempDir()}
	service, err := newThreadCatalogService(f.ctx, f.store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{empty}, DisableWatcher: true,
		EnableUsageBackfillScheduler: true, UsageBackfillBatchSize: 1,
		// The batch summary line is logged once per batch that rebuilt work,
		// as that batch ends, with its elapsed time last.
		Logf: func(format string, args ...any) {
			if strings.HasPrefix(format, "transcript parser backfill batch ") {
				batches <- batch{time.Now(), args[len(args)-1].(time.Duration)}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	const scanHold = 2 * time.Second
	var got []batch
	for len(got) < 3 {
		select {
		case b := <-batches:
			got = append(got, b)
			if len(got) == 1 {
				// A scan holds the catalog while batch 2 asks for its turn.
				service.projectionMu.Lock()
				time.Sleep(scanHold)
				service.projectionMu.Unlock()
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("backfill rebuilt %d batches, want 3", len(got))
		}
	}
	if gap := got[1].at.Sub(got[0].at); gap < 2*got[0].elapsed {
		t.Errorf("batch 1 took %s and batch 2 ended %s later; want at least %s", got[0].elapsed, gap, 2*got[0].elapsed)
	}
	if gap := got[2].at.Sub(got[1].at); gap < 2*got[1].elapsed || gap >= scanHold {
		t.Errorf("batch 2 worked %s after waiting behind a %s scan, and batch 3 ended %s later; want from %s to under %s", got[1].elapsed, scanHold, gap, 2*got[1].elapsed, scanHold)
	}
}

// TestParserBackfillWaitsForFreshnessScans catches a parser backfill batch
// that starts while attached work is queued, or runs during a targeted pass:
// it re-projects sources ahead of, or alongside, the changed files the user
// is waiting for. Boundary: RunTranscriptParserBackfillBatch, the scheduler's
// batch, on the parser fixture: while a pass is held inside its projection of
// a changed file and another file waits queued behind it, the batch does not
// start; it rebuilds the parser-behind source only after both passes end.
func TestParserBackfillWaitsForFreshnessScans(t *testing.T) {
	f := newParserBudgetFixture(t, 2)
	f.service.Close()
	open := func(options threadCatalogServiceOptions) *threadCatalogService {
		options.Roots, options.DisableWatcher = []history.SourceRoot{f.root}, true
		service, err := newThreadCatalogService(f.ctx, f.store, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(service.Close)
		<-service.catalogReady
		return service
	}
	type result struct {
		rebuilt int
		at      time.Time
	}
	batch := func(service *threadCatalogService, ctx context.Context) chan result {
		done := make(chan result, 1)
		go func() {
			rebuilt, _, _ := service.RunTranscriptParserBackfillBatch(ctx, 2)
			done <- result{rebuilt, time.Now()}
		}()
		return done
	}

	// A scan in progress, held inside its projection of a changed file.
	var once sync.Once
	started, release := make(chan struct{}), make(chan struct{})
	var scanning *threadCatalogService
	var scanEnded time.Time
	scanning = open(threadCatalogServiceOptions{
		projectSource: func(ctx context.Context, candidate history.SourceCandidate, stored history.StoredSource, exists bool,
			header history.TranscriptCheckpoint, hasCheckpoint bool, at time.Time) (history.SourceProjection, *preparedTranscript, error) {
			if candidate.Path == f.paths[1] {
				once.Do(func() { close(started) })
				<-release
				defer func() { scanEnded = time.Now() }()
			}
			return scanning.projectSource(ctx, candidate, stored, exists, header, hasCheckpoint, at)
		}})
	file, err := os.OpenFile(f.paths[1], os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Again\"}}\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	scanning.RequestPathRefresh([]string{f.paths[1]})
	<-started
	scanning.RequestPathRefresh([]string{f.paths[0]})
	done := batch(scanning, f.ctx)
	select {
	case r := <-done:
		t.Fatalf("the batch ran during a scan (rebuilt %d)", r.rebuilt)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case r := <-done:
		if r.rebuilt == 0 || r.at.Before(scanEnded) {
			t.Fatalf("the batch rebuilt %d, finishing at %s against the scan's %s", r.rebuilt, r.at, scanEnded)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the batch never ran after the scan")
	}
	if got, want := f.checkpointVersion(t), history.CurrentTranscriptParserVersion(history.ProviderCodex); got != want {
		t.Fatalf("transcript parser version = %q, want %q", got, want)
	}
}

// TestParserBackfillParsesWithoutTheProjectionLock catches a batch that holds
// projectionMu while it reads and parses a source: every worker pass, and so
// every thread's saved rows, waits for the whole batch. Boundary:
// RunTranscriptParserBackfillBatch on a parser-behind source. Once the batch
// is reading, the lock must come free before the batch commits that source.
func TestParserBackfillParsesWithoutTheProjectionLock(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	appendTurns(t, f.path, 0, 200)
	f.service.RequestPathRefresh([]string{f.path})
	waitForCatalogWithin(t, 30*time.Second, func() bool { return f.stored(t).Source.Size == fileSize(t, f.path) })
	f.markStale(t, f.stored(t), f.threadID)

	progress := &dirtySource{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.RunTranscriptParserBackfillBatch(withReadProgress(f.ctx, progress), 1)
	}()
	waitForCatalogWithin(t, 30*time.Second, func() bool { return progress.readAt.Load() != 0 })
	for !f.service.projectionMu.TryLock() {
		select {
		case <-done:
			t.Fatal("the projection lock stayed held from the batch's first read until it returned")
		default:
		}
	}
	behind := f.checkpointVersion(t) == staleParserVersion
	f.service.projectionMu.Unlock()
	if !behind {
		t.Fatal("the projection lock came free only after the batch committed: the batch held it while it parsed")
	}
	<-done
	if got, want := f.checkpointVersion(t), history.CurrentTranscriptParserVersion(history.ProviderCodex); got != want {
		t.Fatalf("transcript parser version = %q, want %q", got, want)
	}
}

// TestParserBackfillCommitsOnlyAnUnchangedSource catches a batch commit or
// error mark that lands after a worker pass changed the source while the
// batch parsed it. An older parse written over the newer source row reads as
// a replaced file (the generation is bumped), and an error mark lands on the
// newer, healthy transcript, where a completed turn then reads as failed.
// Boundary: commitBackfill under projectionMu, given what
// prepareTranscriptSource read before the change.
func TestParserBackfillCommitsOnlyAnUnchangedSource(t *testing.T) {
	failed := errors.New("parse failed")
	workerAppends := func(t *testing.T, f *parserBudgetFixture) {
		appendTurns(t, f.path, 0, 1)
		f.service.RequestPathRefresh([]string{f.path})
		waitForCatalogWithin(t, 30*time.Second, func() bool { return f.stored(t).Source.Size == fileSize(t, f.path) })
	}
	for _, tc := range []struct {
		name       string
		change     func(t *testing.T, f *parserBudgetFixture)
		unreadable bool
		prepareErr error
		want       backfillOutcome
	}{
		{name: "source row changed", change: workerAppends},
		{name: "source row changed, stale preparation error", change: workerAppends, prepareErr: failed},
		{name: "transcript changed, stale preparation error", change: func(t *testing.T, f *parserBudgetFixture) { f.markStale(t, f.stored(t), f.threadID) }, prepareErr: failed},
		{name: "check unreadable after a newer commit", change: workerAppends, unreadable: true, prepareErr: failed},
		{name: "unchanged preparation error", prepareErr: failed, want: backfillMarked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParserBudgetFixture(t, 1)
			candidates, err := f.store.ListTranscriptParserBackfillCandidates(f.ctx, 1)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates = %v, %v", candidates, err)
			}
			candidate := candidates[0]
			checkpoint, exists, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, candidate.ThreadID)
			if err != nil {
				t.Fatal(err)
			}
			base, prepared, err := f.service.prepareTranscriptSource(f.ctx, candidate.Source, candidate.Source.Source, true, checkpoint, exists)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.File.Close()
			if tc.change != nil {
				tc.change(t, f)
			}
			want := f.stored(t)
			if tc.unreadable {
				u := url.URL{Scheme: "file", Path: f.dbPath}
				query := u.Query()
				query.Set("_pragma", "busy_timeout(5000)")
				u.RawQuery = query.Encode()
				db, err := sql.Open("sqlite", u.String())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`UPDATE source_files SET diagnostics_json = '{' WHERE id = ?`, want.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.service.projectionMu.Lock()
			got, _ := f.service.commitBackfill(f.ctx, candidate, checkpoint, exists, base, prepared, tc.prepareErr, time.Now().UTC())
			f.service.projectionMu.Unlock()
			if got != tc.want {
				t.Fatalf("outcome = %v, want %v", got, tc.want)
			}
			after, _, err := f.store.LoadTranscriptCheckpoint(f.ctx, f.threadID)
			if err != nil {
				t.Fatal(err)
			}
			if marked := after.ErrorSummary != nil; marked != (tc.want == backfillMarked) {
				t.Fatalf("transcript error = %v; want marked only after an unchanged check", after.ErrorSummary)
			}
			if !tc.unreadable {
				if stored := f.stored(t); stored.Source.Size != want.Source.Size || stored.Generation != want.Generation {
					t.Fatalf("source row size %d generation %d; want %d at generation %d", stored.Source.Size, stored.Generation, want.Source.Size, want.Generation)
				}
			}
		})
	}
}

// appendTurns appends count complete Codex turns, numbered from first.
func appendTurns(t *testing.T, path string, first, count int) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines strings.Builder
	for turn := first; turn < first+count; turn++ {
		fmt.Fprintf(&lines, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"turn-%d\"}}\n", turn)
		fmt.Fprintf(&lines, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Prompt %d\"}}\n", turn)
		fmt.Fprintf(&lines, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\",\"turn_id\":\"turn-%d\"}}\n", turn)
	}
	if _, err := file.WriteString(lines.String()); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
