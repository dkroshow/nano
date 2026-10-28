package app

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// attachedCatchUpFor returns the owner's catch-up, or nil when it holds no
// file (its identity or path is unresolved).
func (s *threadCatalogService) attachedCatchUpFor(owner string) *attachedCatchUp {
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	if current := s.owners[owner]; current != nil && current.catchUp != nil {
		return current.catchUp
	}
	return nil
}

type recordingSources struct {
	binds    []string
	releases int
	onBind   func()
}

func (r *recordingSources) BindAttachedSource(owner, provider, nativeID, cwd, known string, background bool) {
	bind := provider + "/" + nativeID + "@" + cwd + "|" + known
	if background {
		bind += "|background"
	}
	r.binds = append(r.binds, bind)
	if r.onBind != nil {
		r.onBind()
	}
}

func (r *recordingSources) ReleaseAttachedSource(string) { r.releases++ }

func (r *recordingSources) AwaitAttachedCurrent(string, bool, func() bool, <-chan struct{}) bool {
	return true
}

// passedSince reports that a pass after seq ran and no work is queued.
func (s *threadCatalogService) passedSince(seq uint64) bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return s.passSeq > seq && len(s.dirty) == 0
}

// Failure caught: a late identity event on a closed attachment
// re-established a watch, or a close racing the bind left one behind.
// Boundary: the session's bind on native identity.
func TestSessionCatalogBindNeverOutlivesClose(t *testing.T) {
	sources := &recordingSources{}
	sess := &session{id: "s1", provider: "claude", cwd: "/work", catalogSources: sources}
	sess.setProjectionThreadID("native-1")
	if len(sources.binds) != 1 || sources.binds[0] != "claude/native-1@/work|" {
		t.Fatalf("binds = %q, want the identity once", sources.binds)
	}
	sources.onBind = func() { sess.closed = true } // a close lands while the hold is made
	sess.setProjectionThreadID("native-2")
	if sources.releases != 1 {
		t.Fatalf("releases after a racing close = %d, want 1", sources.releases)
	}
	sources.onBind = nil
	sess.setProjectionThreadID("native-3")
	if len(sources.binds) != 2 {
		t.Fatalf("a closed attachment bound again: %q", sources.binds)
	}
}

// Failure caught: an attachment whose file cannot be resolved vanished from
// monitoring, so it looked unwatched rather than unresolved. Boundary: a
// session's bind through the real catalog to the monitor's ingest view.
func TestUnresolvedAttachmentStaysVisibleUntilBoundOrReleased(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: root}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	view := func() (int, int, map[string]int) {
		status := service.ingestStatus()
		return status.Attachments, status.HeldSources, status.UnresolvedAttachments
	}
	sess := &session{id: "s1", provider: "codex", cwd: "/work", catalogSources: service}
	sess.bindCatalogSource("")
	if attachments, held, unresolved := view(); attachments != 1 || held != 0 || unresolved[unresolvedAwaitingIdentity] != 1 {
		t.Fatalf("before identity: attachments %d, held %d, unresolved %v", attachments, held, unresolved)
	}
	sess.threadID = "01a0aaaa-0000-7000-8000-000000000001" // providers set it before binding
	sess.setProjectionThreadID(sess.threadID)
	if attachments, held, unresolved := view(); attachments != 1 || held != 0 || unresolved[unresolvedNoSavedSource] != 1 {
		t.Fatalf("identity with no saved source: attachments %d, held %d, unresolved %v", attachments, held, unresolved)
	}
	sess.learnCatalogSourcePath(filepath.Join(root, "2026", "10", "03", "rollout-01a0aaaa-0000-7000-8000-000000000001.jsonl"))
	if attachments, held, unresolved := view(); attachments != 1 || held != 1 || len(unresolved) != 0 {
		t.Fatalf("resolved: attachments %d, held %d, unresolved %v", attachments, held, unresolved)
	}
	sess.releaseCatalogSource()
	if attachments, held, _ := view(); attachments != 0 || held != 0 {
		t.Fatalf("released: attachments %d, held %d", attachments, held)
	}
	claude := &session{id: "s2", provider: "claude", cwd: "/work", catalogSources: service}
	claude.setProjectionThreadID("cs_0123456789abcdef")
	if _, held, unresolved := view(); held != 0 || unresolved[unresolvedProvisional] != 1 {
		t.Fatalf("provisional Claude handle: held %d, unresolved %v", held, unresolved)
	}
}

// Failure caught: a pass whose saved-transcript read failed, or that never
// saved or verified a canonical transcript, released a waiting attachment as
// caught up. Such a file stays pending for a later pass. Boundary: the repair
// step's outcome and the catch-up it settles.
func TestCatchUpNeedsASavedOrVerifiedTranscript(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	rootSpec := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}
	root, err := store.EnsureRoot(context.Background(), rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	service := &threadCatalogService{store: store}
	store.Close() // every read below fails
	path := filepath.Join(rootSpec.Path, "-w", "62000000-0000-4000-8000-000000000001.jsonl")
	pass := newIngestPass([]dirtyTake{{path: path}}, 1)
	pass.sources[path].observe(100)
	service.repairTranscripts(context.Background(), root.ID, catalogScanFreshness, []transcriptRepairCandidate{{Source: history.SourceCandidate{Path: path}}}, time.Now(), pass)
	failed := &attachedCatchUp{done: make(chan struct{}), pending: map[string]bool{path: true}}
	failed.finish(path, pass.sources[path])
	if !failed.pending[path] || pass.sources[path].errKind != ingestErrorStore {
		t.Fatalf("a failed saved-transcript read finished the catch-up (outcome %v)", pass.sources[path].err)
	}

	unverified := &attachedCatchUp{done: make(chan struct{}), pending: map[string]bool{path: true}}
	unverified.finish(path, &ingestOutcome{found: true, canonical: true})
	if !unverified.pending[path] {
		t.Fatal("a canonical transcript neither saved nor verified finished the catch-up")
	}
	unverified.finish(path, &ingestOutcome{found: true, canonical: true, committed: true})
	select {
	case <-unverified.done:
	default:
		t.Fatal("a later verifying pass did not finish the catch-up")
	}
	if unverified.result() != catchUpReady {
		t.Fatalf("catch-up after a verifying pass = %v", unverified.result())
	}
}

// Failure caught: at boot the catalog read hidden threads' files first because
// their holds were queued first (Grok's short boot queue registers early), so
// every visible thread's catch-up waited behind them. A background source now
// waits behind foreground ones; an explicit foreground request moves it
// forward, and a file event never does. Boundary: the worker's queue.
func TestBackgroundSourcesWaitBehindForegroundOnes(t *testing.T) {
	s := &threadCatalogService{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}}
	s.requestPaths([]string{"/hidden-a", "/hidden-b", "/hidden-c"}, true)
	s.RequestPathRefresh([]string{"/visible"})
	s.fileChanged("/hidden-c")                  // a file event keeps its place
	s.RequestPathRefresh([]string{"/hidden-b"}) // a reconnect's fresh catch-up
	var order []string
	for _, take := range s.takeDirtyLocked(10) {
		order = append(order, take.path)
	}
	if want := []string{"/visible", "/hidden-b", "/hidden-a", "/hidden-c"}; !slices.Equal(order, want) {
		t.Fatalf("pass order = %q, want %q", order, want)
	}
}

// Failure caught: a background source was served only when no foreground one
// waited, so threads that kept writing requeued themselves after every pass
// and a hidden thread's catch-up never ran; it stayed starting. Also caught:
// aged background sources taking whole passes, which would put boot holds of
// hidden threads back in front of the visible ones. Boundary: the worker's
// queue, across passes.
func TestAgedBackgroundSourcesShareEachPassWithForegroundWriters(t *testing.T) {
	s := &threadCatalogService{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}, scanned: make(chan struct{})}
	aged := time.Now().Add(-catalogBackgroundMaxWait)
	s.enqueue("/hidden-a", aged, true, false)
	s.enqueue("/hidden-b", aged, true, false)
	s.RequestPathRefresh([]string{"/writer-a", "/writer-b"})
	var passes [][]string
	for range 3 {
		batch := s.takeDirtyLocked(catalogIngestBatch)
		var paths []string
		for _, take := range batch {
			paths = append(paths, take.path)
		}
		passes = append(passes, paths)
		s.fileChanged("/writer-a") // both writers append during every pass
		s.fileChanged("/writer-b")
		s.finishPass(batch)
	}
	want := [][]string{{"/hidden-a", "/writer-a"}, {"/hidden-b", "/writer-b"}, {"/writer-a", "/writer-b"}}
	if !slices.EqualFunc(passes, want, slices.Equal[[]string]) {
		t.Fatalf("passes = %q, want %q", passes, want)
	}
}
