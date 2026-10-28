package threadindex

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
func (s *Service) attachedCatchUpFor(owner string) *attachedCatchUp {
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	if current := s.owners[owner]; current != nil && current.catchUp != nil {
		return current.catchUp
	}
	return nil
}

// passedSince reports that a pass after seq ran and no work is queued.
func (s *Service) passedSince(seq uint64) bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return s.passSeq > seq && len(s.dirty) == 0
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
	service := &Service{store: store}
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
	s := &Service{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}}
	s.requestPaths([]string{"/hidden-a", "/hidden-b", "/hidden-c"}, true)
	s.requestPathRefresh([]string{"/visible"})
	s.FileChanged("/hidden-c")                  // a file event keeps its place
	s.requestPathRefresh([]string{"/hidden-b"}) // a reconnect's fresh catch-up
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
	s := &Service{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}, scanned: make(chan struct{})}
	aged := time.Now().Add(-catalogBackgroundMaxWait)
	s.enqueue("/hidden-a", aged, true, false)
	s.enqueue("/hidden-b", aged, true, false)
	s.requestPathRefresh([]string{"/writer-a", "/writer-b"})
	var passes [][]string
	for range 3 {
		batch := s.takeDirtyLocked(catalogIngestBatch)
		var paths []string
		for _, take := range batch {
			paths = append(paths, take.path)
		}
		passes = append(passes, paths)
		s.FileChanged("/writer-a") // both writers append during every pass
		s.FileChanged("/writer-b")
		s.finishPass(batch)
	}
	want := [][]string{{"/hidden-a", "/writer-a"}, {"/hidden-b", "/writer-b"}, {"/writer-a", "/writer-b"}}
	if !slices.EqualFunc(passes, want, slices.Equal[[]string]) {
		t.Fatalf("passes = %q, want %q", passes, want)
	}
}
