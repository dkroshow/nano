package threadindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// The worker must defer only append-safe large re-reads, including errored checkpoints and the post-ApplySource crash window.
func TestTranscriptRereadDeferralExcludesRewritesAndShrinks(t *testing.T) {
	now := time.Now().UTC()
	source := history.SourceCandidate{Root: history.SourceRoot{Provider: history.ProviderClaude}, Path: "/synthetic/file.jsonl", Size: 3 << 20, ModTime: now, Device: 1, Inode: 2}
	stored := history.StoredSource{ID: 1, ThreadID: "thread", CanonicalTranscriptSource: true, Source: source, ParserVersion: parserVersion(history.ProviderClaude)}
	cp := history.TranscriptCheckpoint{SourceID: 1, ParserVersion: "old"}
	cases := []struct {
		name   string
		change func(*history.SourceCandidate, *history.TranscriptCheckpoint, *bool)
		want   bool
	}{
		{"behind-checkpoint-current-source", func(*history.SourceCandidate, *history.TranscriptCheckpoint, *bool) {}, true},
		{"append", func(c *history.SourceCandidate, _ *history.TranscriptCheckpoint, _ *bool) {
			c.Size++
			c.ModTime = c.ModTime.Add(time.Second)
		}, true},
		{"shrink-still-large", func(c *history.SourceCandidate, _ *history.TranscriptCheckpoint, _ *bool) { c.Size = 2 << 20 }, false},
		{"same-size-rewrite", func(c *history.SourceCandidate, _ *history.TranscriptCheckpoint, _ *bool) {
			c.ModTime = c.ModTime.Add(time.Second)
		}, false},
		{"replacement", func(c *history.SourceCandidate, _ *history.TranscriptCheckpoint, _ *bool) { c.Inode++ }, false},
		{"small", func(c *history.SourceCandidate, _ *history.TranscriptCheckpoint, _ *bool) {
			c.Size = rereadInlineMaxBytes
		}, false},
		{"healthy", func(_ *history.SourceCandidate, c *history.TranscriptCheckpoint, _ *bool) {
			c.ParserVersion = parserVersion(history.ProviderClaude)
		}, false},
		{"errored-current", func(_ *history.SourceCandidate, c *history.TranscriptCheckpoint, _ *bool) {
			c.ParserVersion = parserVersion(history.ProviderClaude)
			message := "error"
			c.ErrorSummary = &message
		}, true},
		{"first-read", func(_ *history.SourceCandidate, _ *history.TranscriptCheckpoint, exists *bool) { *exists = false }, false},
		{"grok-json", func(c *history.SourceCandidate, _ *history.TranscriptCheckpoint, _ *bool) {
			c.Root.Provider = history.ProviderGrok
			c.Path = "/synthetic/file.json"
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			candidate, checkpoint, exists := source, cp, true
			c.change(&candidate, &checkpoint, &exists)
			if got := shouldDeferTranscriptReread(stored, candidate, checkpoint, exists); got != c.want {
				t.Fatalf("deferral=%t want %t", got, c.want)
			}
		})
	}
}

// Hints must not duplicate retries, reset their delay or block ready direct work behind an errored thread.
func TestTranscriptRereadDirectQueueDeduplicatesAndBacksOffErrors(t *testing.T) {
	var logs []string
	s := &Service{rereadWake: make(chan struct{}, 1), logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }}
	now := time.Now()
	s.enqueueTranscriptReread("error")
	s.enqueueTranscriptReread("active")
	s.enqueueTranscriptReread("error")
	if len(s.rereadQueue) != 2 {
		t.Fatal("duplicate direct queue entries")
	}
	for _, id := range []string{"error", "active"} {
		if count := strings.Count(strings.Join(logs, "\n"), "transcript reread queued thread="+id); count != 1 {
			t.Errorf("queue transition for %s logged %d times, want once", id, count)
		}
	}
	for failures := 1; failures <= 9; failures++ {
		s.recordTranscriptRereadFailure("error", now)
		want := min(time.Minute*time.Duration(1<<uint(failures-1)), time.Hour)
		if got := s.rereadRetry["error"].after.Sub(now); got != want {
			t.Fatalf("retry %d delay=%s want %s", failures, got, want)
		}
	}
	id, _ := s.takeTranscriptReread(now)
	if id != "active" {
		t.Fatal("backoff thread blocked active work", id)
	}
	s.enqueueTranscriptReread("active")
	if count := strings.Count(strings.Join(logs, "\n"), "transcript reread queued thread=active"); count != 1 {
		t.Errorf("duplicate hint while running logged %d queue transitions", count)
	}
	s.finishTranscriptReread("error", true)
	if strings.Contains(strings.Join(logs, "\n"), "transcript reread dropped") {
		t.Fatal("retained error retry logged a drop")
	}
	s.finishTranscriptReread("active", false)
	if s.hasDirectTranscriptReread(now) {
		t.Fatal("hint bypassed error retry backoff")
	}
	if id, _ := s.takeTranscriptReread(now.Add(time.Hour)); id != "error" {
		t.Fatal("errored thread never retried", id)
	}
	s.finishTranscriptReread("error", false)
	if len(s.rereadRetry) != 0 || s.DeferredTranscriptReread("error") {
		t.Fatal("successful retry did not clear delay/diagnostic state")
	}
	if strings.Contains(strings.Join(logs, "\n"), "transcript reread dropped") {
		t.Fatal("successful finish logged a drop")
	}
	// A real removal from the direct queue must close its interval exactly once.
	for _, name := range []string{"checkpoint-removed", "source-removed", "file-removed", "superseded", "canceled", "deadline", "error"} {
		t.Run(name, func(t *testing.T) {
			f := newParserBudgetFixture(t, 1)
			f.service.Close()
			s := synchronousRereadService(f)
			var lines []string
			s.logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
			id, reason := f.threadID, "removed"
			ctx := f.ctx
			wantError := false
			switch name {
			case "checkpoint-removed", "source-removed":
				db, err := sql.Open("sqlite", f.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				statement := `UPDATE source_files SET status='missing'`
				if name == "checkpoint-removed" {
					statement = `UPDATE conversation_projections SET state='superseded'`
				}
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			case "file-removed":
				if err := os.Remove(f.path); err != nil {
					t.Fatal(err)
				}
			case "superseded":
				reason = "superseded"
			case "canceled", "deadline":
				reason, wantError = "canceled", true
				var cancel context.CancelFunc
				if name == "canceled" {
					ctx, cancel = context.WithCancel(f.ctx)
					cancel()
				} else {
					ctx, cancel = context.WithDeadline(f.ctx, time.Now().Add(-time.Second))
				}
				defer cancel()
			case "error":
				reason, wantError = "error", true
				f.store.Close()
			}
			s.enqueueTranscriptReread(id)
			s.enqueueTranscriptReread(id)
			if len(lines) != 1 || lines[0] != "transcript reread queued thread="+id {
				t.Errorf("queue log missing at admission or duplicated by a hint: %v", lines)
			}
			if _, _, _, err := s.runDirectTranscriptRereads(ctx, 1); (err != nil) != wantError {
				t.Fatal("unexpected drop result", err)
			}
			if s.DeferredTranscriptReread(id) {
				t.Fatal("dropped thread remained queued")
			}
			// Re-running an empty queue must not repeat the drop.
			s.runDirectTranscriptRereads(f.ctx, 1)
			if len(lines) != 2 || lines[1] != "transcript reread dropped thread="+id+" reason="+reason {
				t.Errorf("drop log missing at removal, out of order or emitted extra content: %v", lines)
			}
		})
	}
}

func synchronousRereadService(f *parserBudgetFixture) *Service {
	s := &Service{store: f.store, roots: f.service.roots, logf: func(string, ...any) {}, hub: newCatalogHub(), dirty: make(map[string]*dirtySource), scanned: make(chan struct{}), transcriptChanges: make(map[string]*pendingTranscriptChange), rereadWake: make(chan struct{}, 1)}
	s.transcriptChangedCond = sync.NewCond(&s.transcriptChangedMu)
	return s
}

func growSyntheticReread(t *testing.T, f *parserBudgetFixture, path string) history.SourceCandidate {
	t.Helper()
	// One synthetic growing tool-output record makes the native input cross two read chunks.
	raw := `{"type":"event_msg","payload":{"type":"agent_message","message":"` + strings.Repeat("x", rereadInlineMaxBytes) + `"}}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(raw); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	candidate, found, err := history.DiscoverFile(f.ctx, f.root, path)
	if err != nil || !found {
		t.Fatal(err)
	}
	return candidate
}

// At the next chunk edge, a direct thread must abandon a backlog shadow and get rebuilt first.
func TestTranscriptRereadDirectQueuePreemptsBacklogAndSwitchesFirst(t *testing.T) {
	f := newParserBudgetFixture(t, 2)
	f.service.Close()
	s := synchronousRereadService(f)
	first := f.storedAt(t, f.paths[0])
	second := f.storedAt(t, f.paths[1])
	firstCandidate := growSyntheticReread(t, f, f.paths[0])
	growSyntheticReread(t, f, f.paths[1])
	cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	base, prepared, err := s.prepareTranscriptSource(f.ctx, first, firstCandidate, true, cp, true)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.File.Close()
	s.enqueueTranscriptReread(second.ThreadID)
	item := history.TranscriptParserBackfillCandidate{ThreadID: first.ThreadID, Source: first, Provider: first.Source.Root.Provider}
	outcome, _, err := s.buildPreparedTranscriptReread(f.ctx, item, cp, base, prepared, false, time.Time{}, time.Now().UTC())
	if outcome != backfillSkipped || !errors.Is(err, history.ErrTranscriptRebuildPreempted) {
		t.Fatal("direct thread did not preempt backlog", outcome, err)
	}
	old, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, first.ThreadID)
	if err != nil || old.ProjectionKey != cp.ProjectionKey {
		t.Fatal("preemption replaced A", err)
	}
	inspected, _, err := s.runTranscriptParserBackfillBatch(f.ctx, 1)
	if err != nil || inspected != 1 {
		t.Fatal("direct rebuild failed", inspected, err)
	}
	direct, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, second.ThreadID)
	if err != nil || direct.ParserVersion != parserVersion(f.root.Provider) {
		t.Fatal("direct thread did not switch first", err)
	}
	old, _, err = f.store.LoadTranscriptCheckpointHeader(f.ctx, first.ThreadID)
	if err != nil || old.ParserVersion != staleParserVersion {
		t.Fatal("backlog jumped ahead of direct thread", err)
	}
	if s.DeferredTranscriptReread(second.ThreadID) {
		t.Fatal("completed direct build retained diagnostic label")
	}
}

// Source-row and checkpoint changes must abandon the ready shadow rather than overwrite newer metadata or rows.
func TestTranscriptRereadSourceBaseChecksAbandonChangedBuild(t *testing.T) {
	for _, change := range []string{"metadata", "removed", "checkpoint"} {
		t.Run(change, func(t *testing.T) {
			f := newParserBudgetFixture(t, 1)
			f.service.Close()
			s := synchronousRereadService(f)
			source := f.stored(t)
			candidate := growSyntheticReread(t, f, f.path)
			cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
			if err != nil {
				t.Fatal(err)
			}
			base, prepared, err := s.prepareTranscriptSource(f.ctx, source, candidate, true, cp, true)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.File.Close()
			db, err := sql.Open("sqlite", f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			s.projectionMu.Lock()
			type buildResult struct {
				outcome backfillOutcome
				err     error
			}
			resultCh := make(chan buildResult, 1)
			item := history.TranscriptParserBackfillCandidate{ThreadID: f.threadID, Source: source, Provider: f.root.Provider}
			go func() {
				outcome, _, err := s.buildPreparedTranscriptReread(f.ctx, item, cp, base, prepared, true, time.Now(), time.Now().UTC())
				resultCh <- buildResult{outcome, err}
			}()
			waitForCatalog(t, func() bool {
				var count int
				err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='building'`).Scan(&count)
				return err == nil && count == 1
			})
			statement := `UPDATE threads SET title='changed' WHERE id=?`
			if change == "removed" {
				statement = `UPDATE source_files SET status='missing' WHERE id=?`
			}
			if change == "checkpoint" {
				statement = `UPDATE conversation_projections SET read_offset=read_offset+1 WHERE thread_id=? AND state='active'`
			}
			arg := any(f.threadID)
			if change == "removed" {
				arg = source.ID
			}
			if _, err := db.Exec(statement, arg); err != nil {
				t.Fatal(err)
			}
			s.projectionMu.Unlock()
			result := <-resultCh
			if result.outcome != backfillSkipped || !errors.Is(result.err, history.ErrTranscriptChanged) {
				t.Fatal("changed base overwrote new state", result.outcome, result.err)
			}
			var building, abandoned int
			if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='building'`).Scan(&building); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='abandoned'`).Scan(&abandoned); err != nil {
				t.Fatal(err)
			}
			if building != 0 || abandoned != 1 {
				t.Fatal("failed base check did not abandon B", building, abandoned)
			}
		})
	}
}

type startupCollectorProbe struct{ calls atomic.Int32 }

func (p *startupCollectorProbe) CollectSupersededProjectionsBatch(context.Context, int) (history.SupersededProjectionBatch, error) {
	p.calls.Add(1)
	return history.SupersededProjectionBatch{}, nil
}

// catalogReady alone must not release either builder or collector after a failed startup abandon.
func TestTranscriptRereadStartupGateHoldsBuilderAndCollectorUntilAbandonCommits(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	source := f.stored(t)
	cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.service.prepareTranscriptFile(f.ctx, source.Source, source, true, cp, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.File.Close()
	if _, err := f.store.BeginTranscriptRebuild(f.ctx, prepared.transcriptApplyRequest(source, time.Now().UTC()), cp); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_startup BEFORE UPDATE OF state ON conversation_projections WHEN OLD.state='building' BEGIN SELECT RAISE(ABORT,'startup failed'); END`); err != nil {
		t.Fatal(err)
	}
	probe := &startupCollectorProbe{}
	failed := make(chan struct{}, 1)
	empty := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexArchived, Path: t.TempDir()}
	s, err := newTestCatalogService(f.ctx, f.store, Options{Roots: []history.SourceRoot{empty}, DisableWatcher: true, EnableUsageBackfillScheduler: true, EnableSupersededProjectionCollector: true, supersededCollector: probe, Logf: func(format string, _ ...any) {
		if strings.HasPrefix(format, "transcript rebuild startup abandon failed") {
			select {
			case failed <- struct{}{}:
			default:
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("startup failure not reported")
	}
	select {
	case <-s.catalogReady:
	default:
		t.Fatal("fixture never reached catalogReady")
	}
	select {
	case <-s.rereadReady:
		t.Fatal("failed abandon released startup gate")
	default:
	}
	if probe.calls.Load() != 0 {
		t.Fatal("collector started before abandon committed")
	}
	unchanged, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
	if err != nil || unchanged.ParserVersion != staleParserVersion {
		t.Fatal("builder started before abandon committed", err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_startup`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.rereadReady:
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not retry abandon")
	}
	waitForCatalog(t, func() bool { return probe.calls.Load() > 0 && f.checkpointVersion(t) == parserVersion(f.root.Provider) })
	var building int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='building'`).Scan(&building); err != nil || building != 0 {
		t.Fatal("startup retained a building projection", building, err)
	}
}

// A retrying large errored source must pass through the same queue, with no inline parse or metadata update.
func TestTranscriptRereadWorkerDefersLargeErroredCheckpoint(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	s := synchronousRereadService(f)
	s.projectSourceFn = func(context.Context, history.SourceCandidate, history.StoredSource, bool, history.TranscriptCheckpoint, bool, time.Time) (history.SourceProjection, *preparedTranscript, error) {
		t.Fatal("deferred source was parsed inline")
		return history.SourceProjection{}, nil, nil
	}
	source := f.stored(t)
	growSyntheticReread(t, f, f.path)
	if err := f.store.MarkTranscriptError(f.ctx, f.threadID, source.ID, "synthetic error", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.projectionMu.Lock()
	s.refreshAll(f.ctx, catalogScanRecovery)
	s.projectionMu.Unlock()
	if !s.DeferredTranscriptReread(f.threadID) {
		t.Fatal("large errored checkpoint missed direct queue")
	}
	after := f.stored(t)
	if after.Source.Size != source.Source.Size || after.ParserVersion != source.ParserVersion {
		t.Fatal("deferral changed the base source row")
	}
	if count, _, err := s.runTranscriptParserBackfillBatch(f.ctx, 1); err != nil || count != 1 {
		t.Fatal("errored source was never retried", count, err)
	}
	cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
	if err != nil || cp.ErrorSummary != nil || cp.ParserVersion != parserVersion(f.root.Provider) {
		t.Fatal("errored rebuild did not switch", err)
	}
}

// Canceled and expired builders abandon their work without marking A or starting error retry backoff.
func TestTranscriptRereadCancellationDoesNotBackOffErrors(t *testing.T) {
	for _, name := range []string{"canceled", "deadline"} {
		t.Run(name, func(t *testing.T) {
			f := newParserBudgetFixture(t, 1)
			f.service.Close()
			s := synchronousRereadService(f)
			source := f.stored(t)
			candidate := growSyntheticReread(t, f, f.path)
			cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
			if err != nil {
				t.Fatal(err)
			}
			base, prepared, err := s.prepareTranscriptSource(f.ctx, source, candidate, true, cp, true)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.File.Close()
			ctx, cancel := context.WithCancel(f.ctx)
			want := context.Canceled
			if name == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(f.ctx, time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				cancel()
			}
			defer cancel()
			item := history.TranscriptParserBackfillCandidate{ThreadID: f.threadID, Source: source, Provider: f.root.Provider}
			outcome, _, err := s.buildPreparedTranscriptReread(ctx, item, cp, base, prepared, true, time.Now(), time.Now().UTC())
			if outcome != backfillSkipped || !errors.Is(err, want) || len(s.rereadRetry) != 0 {
				t.Fatal("cancellation became an error retry", outcome, err, s.rereadRetry)
			}
			after, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
			if err != nil || after.ProjectionKey != cp.ProjectionKey || after.ErrorSummary != nil {
				t.Fatal("cancellation changed A", err)
			}
		})
	}
}

// A Refresh walk must leave unchanged healthy parser-behind files in the paused backlog.
func TestTranscriptRereadRefreshLeavesUnchangedLargeThreadInBacklog(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	stored := f.stored(t)
	stored.Source = growSyntheticReread(t, f, f.path)
	f.markStale(t, stored, f.threadID)
	s := synchronousRereadService(f)
	s.projectSourceFn = func(context.Context, history.SourceCandidate, history.StoredSource, bool, history.TranscriptCheckpoint, bool, time.Time) (history.SourceProjection, *preparedTranscript, error) {
		t.Fatal("unchanged parser-behind source was parsed inline")
		return history.SourceProjection{}, nil, nil
	}
	s.backfillPauseFile = filepath.Join(t.TempDir(), "parser-backfill.pause")
	if err := os.WriteFile(s.backfillPauseFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.projectionMu.Lock()
	s.refreshAll(f.ctx, catalogScanRecovery)
	s.projectionMu.Unlock()
	if s.DeferredTranscriptReread(f.threadID) {
		t.Fatal("Refresh queued an idle parser-behind thread as direct work")
	}
	if count, _, err := s.runTranscriptParserBackfillBatch(f.ctx, 1); err != nil || count != 0 {
		t.Fatal("unchanged thread bypassed the backlog pause", count, err)
	}
	if f.checkpointVersion(t) != staleParserVersion {
		t.Fatal("Refresh advanced the idle checkpoint")
	}
}

// An attachment's catch-up must not finish while a grown source's saved tail
// is frozen by deferral, and must finish once the deferred reread lands.
func TestTranscriptRereadDeferredCatchUpIsNotReady(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	s := f.service
	growSyntheticReread(t, f, f.path)
	s.holdAttachedSource("deferred-attachment", []string{f.path})
	defer s.ReleaseAttachedSource("deferred-attachment")
	catchUp := s.attachedCatchUpFor("deferred-attachment")
	s.queueMu.Lock()
	batch := s.takeDirtyLocked(2)
	seq := s.passSeq
	s.queueMu.Unlock()
	s.projectionMu.Lock()
	s.ingestBatch(f.ctx, batch, seq)
	s.projectionMu.Unlock()
	s.finishPass(batch)
	select {
	case <-catchUp.done:
		t.Fatal("deferred source finished the catch-up")
	default:
	}
	if !s.DeferredTranscriptReread(f.threadID) {
		t.Fatal("grown source was not deferred")
	}
	held := s.held[f.path]
	if held.failures != 0 || held.retry != nil {
		t.Fatal("deferral started a held-file failure retry")
	}

	if _, _, _, err := s.runDirectTranscriptRereads(f.ctx, 1); err != nil {
		t.Fatal(err)
	}
	s.queueMu.Lock()
	batch = s.takeDirtyLocked(2)
	seq = s.passSeq
	s.queueMu.Unlock()
	if len(batch) != 1 || batch[0].path != f.path {
		t.Fatalf("the finished reread queued %v; want one targeted pass of the held file", batch)
	}
	s.projectionMu.Lock()
	s.ingestBatch(f.ctx, batch, seq)
	s.projectionMu.Unlock()
	s.finishPass(batch)
	select {
	case <-catchUp.done:
	default:
		t.Fatal("the pass after the reread did not finish the catch-up")
	}
	if result := s.catchUpResultOf(catchUp); result != catchUpReady {
		t.Fatalf("catch-up after the reread = %v", result)
	}
}

// The catalog switch must honor the search-maintenance skip when S0 changes the thread kind.
func TestTranscriptRereadKindSwitchSkipsOldThreadSearch(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	s := synchronousRereadService(f)
	source := f.stored(t)
	candidate := growSyntheticReread(t, f, f.path)
	cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
	if err != nil {
		t.Fatal(err)
	}
	base, prepared, err := s.prepareTranscriptSource(f.ctx, source, candidate, true, cp, true)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.File.Close()
	modes := []history.ConversationSearchMode{history.ConversationSearchLiteral, history.ConversationSearchTerms}
	if _, err := f.store.StartConversationSearchBackfill(f.ctx, history.ConversationSearchBackfillStart{Modes: modes, CorpusReset: true}); err != nil {
		t.Fatal(err)
	}
	for {
		progress, err := f.store.RunConversationSearchBackfillBatch(f.ctx, history.ConversationSearchBackfillBatch{Modes: modes, BatchSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if progress.Complete {
			break
		}
	}
	prepared.File.Metadata.Projection.Kind = history.ThreadGuardian
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER no_search_delete BEFORE DELETE ON conversation_search_documents BEGIN SELECT RAISE(ABORT,'thread search maintenance ran'); END`); err != nil {
		t.Fatal(err)
	}
	item := history.TranscriptParserBackfillCandidate{ThreadID: f.threadID, Source: source, Provider: f.root.Provider}
	outcome, _, err := s.buildPreparedTranscriptReread(f.ctx, item, cp, base, prepared, true, time.Now(), time.Now().UTC())
	if err != nil || outcome != backfillRepaired {
		t.Fatal("kind-changing catalog switch ran old-thread search maintenance", outcome, err)
	}
	if stored := f.stored(t); stored.Projection.Kind != history.ThreadGuardian {
		t.Fatal("switch lost S0's kind")
	}
	for _, mode := range modes {
		page, err := f.store.SearchConversationItems(f.ctx, history.ConversationSearchQuery{Query: "Prompt", Mode: mode})
		if err != nil || len(page.Items) != 0 {
			t.Fatal("kind-changing switch exposed ineligible search rows", mode, page, err)
		}
	}
}

// A real chunk-write failure must leave A active, mark its error, and cool the queued retry for one minute.
func TestTranscriptRereadFailedBuildWaitsForFirstRetry(t *testing.T) {
	f := newParserBudgetFixture(t, 1)
	f.service.Close()
	s := synchronousRereadService(f)
	var logs []string
	s.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	cp, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
	if err != nil {
		t.Fatal(err)
	}
	growSyntheticReread(t, f, f.path)
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_rebuild BEFORE INSERT ON conversation_items BEGIN SELECT RAISE(ABORT,'synthetic write failure'); END`); err != nil {
		t.Fatal(err)
	}
	s.enqueueTranscriptReread(f.threadID)
	s.enqueueTranscriptReread(f.threadID)
	before := time.Now()
	count, _, err := s.runTranscriptParserBackfillBatch(f.ctx, 1)
	after := time.Now()
	if err != nil || count != 1 {
		t.Fatal("failed direct build was not processed", count, err)
	}
	s.enqueueTranscriptReread(f.threadID)
	if count := strings.Count(strings.Join(logs, "\n"), "transcript reread queued thread="+f.threadID); count != 1 {
		t.Errorf("failed build logged %d queue transitions, want once", count)
	}
	if strings.Contains(strings.Join(logs, "\n"), "transcript reread dropped") {
		t.Fatal("failed build still queued for retry logged a drop")
	}
	retry := s.rereadRetry[f.threadID]
	if retry.failures != 1 || retry.after.Before(before.Add(time.Minute)) || retry.after.After(after.Add(time.Minute)) {
		t.Fatal("failed build did not record the first retry delay", retry)
	}
	if id, _ := s.takeTranscriptReread(retry.after.Add(-time.Nanosecond)); id != "" {
		t.Fatal("failed build was retaken before its backoff", id)
	}
	if id, _ := s.takeTranscriptReread(retry.after); id != f.threadID {
		t.Fatal("failed build never became eligible again", id)
	}
	current, _, err := f.store.LoadTranscriptCheckpointHeader(f.ctx, f.threadID)
	if err != nil || current.ProjectionKey != cp.ProjectionKey || current.ErrorSummary == nil {
		t.Fatal("failed build lost A or its error diagnostic", err)
	}
	var abandoned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_projections WHERE state='abandoned'`).Scan(&abandoned); err != nil || abandoned != 1 {
		t.Fatal("failed build did not abandon B", abandoned, err)
	}
}
