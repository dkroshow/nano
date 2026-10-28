package app

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func (s *threadCatalogService) requestSupersededCollection() {
	select {
	case s.supersededCollection <- struct{}{}:
	default:
	}
}

// runSupersededProjectionCollector starts after the store opens. It collects
// derived generations in small, independently committed batches with an
// explicit yield between them. A periodic pass covers projections superseded
// by live rebuilds after startup.
func (s *threadCatalogService) runSupersededProjectionCollector(ctx context.Context) {
	defer close(s.supersededDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	ticker := time.NewTicker(catalogMaintenanceInterval)
	defer ticker.Stop()
	for {
		if err := s.collectSupersededProjections(ctx); err != nil && !errors.Is(err, context.Canceled) {
			s.logf("superseded projection collection: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.supersededCollection:
		case <-ticker.C:
		}
	}
}

// runParserStateCompaction starts after the store opens and rewrites stored
// checkpoints once, in small, independently committed batches with a yield
// between them. A checkpoint written since the readers' retention rule changed
// is slim already, so there is no recurring sweep; a checkpoint it did not
// reach (an error, a shutdown) waits for the next boot.
func (s *threadCatalogService) runParserStateCompaction(ctx context.Context) {
	defer close(s.compactionDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	if err := s.compactParserState(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logf("parser state compaction: %v", err)
	}
}

func (s *threadCatalogService) compactParserState(ctx context.Context) error {
	var projections, skipped int
	var before, after int64
	started := time.Now()
	for {
		batch, err := s.store.CompactParserStateBatch(ctx, 32)
		projections, skipped, before, after = projections+batch.Projections, skipped+batch.Skipped, before+batch.BytesBefore, after+batch.BytesAfter
		if err != nil || !batch.Remaining {
			if projections+skipped > 0 {
				s.logf("parser state compaction: projections=%d skipped=%d bytes=%d->%d elapsed=%s", projections, skipped, before, after, time.Since(started))
			}
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *threadCatalogService) collectSupersededProjections(ctx context.Context) error {
	for pass := 1; ; pass++ {
		started := time.Now()
		batch, err := s.supersededCollector.CollectSupersededProjectionsBatch(ctx, 64)
		// A pass that found nothing is the common case; only work is news.
		if batch.Collected > 0 || len(batch.Progress) > 0 {
			s.logf("history collector: pass=%d collected=%d items=%d elapsed=%s", pass, batch.Collected, batch.Items, time.Since(started))
		}
		for _, progress := range batch.Progress {
			s.logf("history collector: pass=%d projection=%d rows=%s", pass, progress.ProjectionID, progress.RowsSummary())
		}
		if err != nil {
			return err
		}
		if !batch.Remaining {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// runConversationSearchScheduler owns one automatic historical-build attempt
// for this process. Each Store batch is independently committed before this
// goroutine yields, so catalog scans and transcript ingestion may acquire the
// history write lock between batches. Lifecycle cancellation is checked only
// between Store calls: shutdown therefore waits for the current bounded
// transaction rather than turning it into a partial write.
func (s *threadCatalogService) runConversationSearchScheduler(ctx context.Context) {
	defer close(s.searchDone)
	select {
	case <-ctx.Done():
		return
	case <-s.catalogReady:
	}
	retryFailedAfterRestart := true
	for {
		if ctx.Err() != nil {
			return
		}
		statuses, err := s.searchBackfill.ConversationSearchStatuses(context.Background())
		if err != nil {
			s.logf("conversation search scheduler could not read state: %v", err)
			return
		}
		modes, start, corpusReset := conversationSearchSchedulerModes(statuses, retryFailedAfterRestart)
		if len(modes) == 0 {
			return
		}
		if start {
			if _, err := s.searchBackfill.StartConversationSearchBackfill(context.Background(), history.ConversationSearchBackfillStart{Modes: modes, CorpusReset: corpusReset}); err != nil {
				s.logf("conversation search scheduler could not start modes=%s: %v", conversationSearchModeNames(modes), err)
				s.failConversationSearchBuild(modes, err)
				return
			}
			if corpusReset {
				s.logf("conversation search scheduler started fresh corpus build modes=%s", conversationSearchModeNames(modes))
			} else {
				s.logf("conversation search scheduler started rebuild modes=%s", conversationSearchModeNames(modes))
			}
		} else {
			s.logf("conversation search scheduler resumed modes=%s", conversationSearchModeNames(modes))
		}
		if !s.runConversationSearchBuild(ctx, modes) {
			return
		}
		if start {
			retryFailedAfterRestart = false
		}
	}
}

// conversationSearchSchedulerModes selects at most one checkpoint-compatible
// build. An existing building mode always resumes first. On process startup,
// failed modes are deliberately reset once; a failure produced by this process
// instead stops the scheduler to avoid an unbounded retry loop.
func conversationSearchSchedulerModes(statuses []history.ConversationSearchStatus, retryFailedAfterRestart bool) ([]history.ConversationSearchMode, bool, bool) {
	var building, reset []history.ConversationSearchMode
	ready := false
	for _, status := range statuses {
		switch status.State {
		case history.ConversationSearchBuilding:
			building = append(building, status.Mode)
		case history.ConversationSearchNeedsBuild:
			reset = append(reset, status.Mode)
		case history.ConversationSearchFailed:
			if retryFailedAfterRestart {
				reset = append(reset, status.Mode)
			}
		case history.ConversationSearchReady:
			ready = true
		}
	}
	if len(building) > 0 {
		return building, false, false
	}
	if len(reset) == 0 {
		return nil, false, false
	}
	corpusReset := !ready && hasBothConversationSearchSchedulerModes(reset)
	return reset, true, corpusReset
}

func hasBothConversationSearchSchedulerModes(modes []history.ConversationSearchMode) bool {
	if len(modes) != 2 {
		return false
	}
	seen := map[history.ConversationSearchMode]bool{}
	for _, mode := range modes {
		seen[mode] = true
	}
	return seen[history.ConversationSearchLiteral] && seen[history.ConversationSearchTerms]
}

func conversationSearchModeNames(modes []history.ConversationSearchMode) string {
	values := make([]string, len(modes))
	for index, mode := range modes {
		values[index] = string(mode)
	}
	return strings.Join(values, ",")
}

func (s *threadCatalogService) runConversationSearchBuild(ctx context.Context, modes []history.ConversationSearchMode) bool {
	var lastMilestone int64 = -1
	staleLogged := false
	for {
		if ctx.Err() != nil {
			return false
		}
		progress, err := s.searchBackfill.RunConversationSearchBackfillBatch(context.Background(), history.ConversationSearchBackfillBatch{Modes: modes, BatchSize: s.searchBatchSize})
		if errors.Is(err, history.ErrConversationSearchReconciliationStale) {
			if !staleLogged {
				s.logf("conversation search scheduler reconciliation became stale modes=%s; retrying", conversationSearchModeNames(modes))
				staleLogged = true
			}
			if !waitConversationSearchSchedulerRetry(ctx) {
				return false
			}
			continue
		}
		if err != nil {
			s.failConversationSearchBuild(modes, err)
			return false
		}
		staleLogged = false
		if progress.Complete {
			s.logf("conversation search scheduler completed modes=%s", conversationSearchModeNames(modes))
			return true
		}
		for _, status := range progress.Modes {
			milestone := status.IndexedCount / conversationSearchProgressInterval
			if milestone > lastMilestone {
				lastMilestone = milestone
				s.logf("conversation search scheduler progress modes=%s indexed=%d", conversationSearchModeNames(modes), status.IndexedCount)
			}
		}
	}
}

func waitConversationSearchSchedulerRetry(ctx context.Context) bool {
	timer := time.NewTimer(conversationSearchStaleRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *threadCatalogService) failConversationSearchBuild(modes []history.ConversationSearchMode, cause error) {
	statuses, statusErr := s.searchBackfill.ConversationSearchStatuses(context.Background())
	if statusErr != nil {
		s.logf("conversation search scheduler failed modes=%s: %v (could not read failure state: %v)", conversationSearchModeNames(modes), cause, statusErr)
		return
	}
	building := make([]history.ConversationSearchMode, 0, len(modes))
	for _, status := range statuses {
		for _, mode := range modes {
			if status.Mode == mode && status.State == history.ConversationSearchBuilding {
				building = append(building, mode)
			}
		}
	}
	if len(building) > 0 {
		if _, err := s.searchBackfill.FailConversationSearchBackfill(context.Background(), building, cause); err != nil {
			s.logf("conversation search scheduler failed modes=%s: %v (could not persist failure: %v)", conversationSearchModeNames(modes), cause, err)
			return
		}
	}
	s.logf("conversation search scheduler failed modes=%s: %v", conversationSearchModeNames(modes), cause)
}

func (s *threadCatalogService) runUsageBackfillScheduler(ctx context.Context) {
	defer close(s.usageDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	for {
		var rebuilt int
		var worked time.Duration
		var err error
		if s.usageBackfillEnabled || s.hasDirectTranscriptReread(time.Now()) {
			rebuilt, worked, err = s.RunTranscriptParserBackfillBatch(ctx, s.usageBackfillBatch)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			s.logf("transcript usage backfill: %v", err)
		}
		// Preserve the ordinary small backfill pacing; the shared lane paces chunks.
		wait := catalogMaintenanceInterval
		if rebuilt > 0 {
			wait = 2 * worked
		}
		if retryWait, ok := s.nextTranscriptRereadRetry(time.Now()); ok {
			wait = min(wait, retryWait)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.rereadWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// RunTranscriptParserBackfillBatch rebuilds at most batchSize parser-behind
// transcripts whose canonical source is still present. It returns how many it
// inspected and how long it worked once admitted, not counting its waits for
// projectionMu.
func (s *threadCatalogService) RunTranscriptParserBackfillBatch(ctx context.Context, batchSize int) (int, time.Duration, error) {
	if !s.waitTranscriptRebuildStartup(ctx) {
		return 0, 0, ctx.Err()
	}
	s.rereadBuilderMu.Lock()
	defer s.rereadBuilderMu.Unlock()
	if batchSize <= 0 {
		batchSize = defaultTranscriptBackfillBatchSize
	}
	if rebuilt, worked, handled, err := s.runDirectTranscriptRereads(ctx, batchSize); handled {
		return rebuilt, worked, err
	}
	// Explicit callers still inspect the backlog, even with its automatic loop off.

	// A batch never starts during a worker pass or while attached work or an
	// explicit recovery is queued: it waits for the pass that takes the work,
	// then checks again. Under a steady stream of file changes the backfill
	// waits; freshness always goes first. Once started it reads and parses
	// without projectionMu and takes it only to commit each source, so worker
	// passes keep committing while a batch parses.
	for {
		s.projectionMu.Lock()
		s.queueMu.Lock()
		queued, scanned := len(s.dirtyOrder)+len(s.dirtyBackground) > 0 || !s.recoveryAt.IsZero(), s.scanned
		s.queueMu.Unlock()
		s.projectionMu.Unlock()
		if !queued {
			break
		}
		select {
		case <-scanned:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	// Checked after admission, so a pause made while this batch waited holds it.
	if s.backfillPauseFile != "" {
		// Anything but a clear "not there" holds the batch: the backfill can wait.
		if _, err := os.Stat(s.backfillPauseFile); !errors.Is(err, os.ErrNotExist) {
			s.logf("transcript parser backfill paused: %s (stat: %v)", s.backfillPauseFile, err)
			return 0, 0, nil
		}
	}
	started := time.Now()
	candidates, err := s.store.ListTranscriptParserBackfillCandidates(ctx, -1)
	if err != nil {
		return 0, time.Since(started), err
	}
	candidates, unaffected := s.markUnaffectedTranscripts(ctx, candidates, time.Now().UTC())
	priorities := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		priorities[candidate.ThreadID] = s.transcriptRepairPriority(candidate)
	}
	prioritizeTranscriptBackfillCandidates(candidates, priorities)
	if len(candidates) > batchSize {
		candidates = candidates[:batchSize]
	}
	rebuilt := 0
	repaired := 0
	var waited, held time.Duration
	now := time.Now().UTC()
	for _, candidate := range candidates {
		if s.transcriptRereadCooling(candidate.ThreadID, time.Now()) || s.deferredTranscriptReread(candidate.ThreadID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return rebuilt, time.Since(started) - waited, err
		}
		checkpoint, exists, err := s.store.LoadTranscriptCheckpointHeader(ctx, candidate.ThreadID)
		if err != nil {
			s.logf("transcript usage backfill checkpoint %s: %v", candidate.ThreadID, err)
			continue
		}
		base, prepared, prepareErr := s.prepareTranscriptSource(ctx, candidate.Source, candidate.Source.Source, true, checkpoint, exists)
		if errors.Is(prepareErr, history.ErrTranscriptChanged) {
			continue
		}
		if prepareErr != nil && base.ID == 0 {
			// The source row itself could not be read: nothing to check a
			// commit or an error mark against.
			s.logf("transcript usage backfill source %s: %v", candidate.ThreadID, prepareErr)
			continue
		}
		if prepareErr == nil && shouldDeferTranscriptReread(base, prepared.Source, checkpoint, exists) {
			outcome, advance, buildErr := s.buildPreparedTranscriptReread(ctx, candidate, checkpoint, base, prepared, false, time.Time{}, now)
			prepared.File.Close()
			s.publishApplied(ctx, advance)
			if outcome != backfillSkipped {
				rebuilt++
			}
			if buildErr != nil && !errors.Is(buildErr, history.ErrTranscriptRebuildPreempted) && !errors.Is(buildErr, history.ErrTranscriptChanged) {
				s.logf("transcript reread: %v", buildErr)
			}
			if s.hasDirectTranscriptReread(time.Now()) {
				break
			}
			continue
		}
		lockStart := time.Now()
		s.projectionMu.Lock()
		locked := time.Now()
		waited += locked.Sub(lockStart)
		outcome, advance := s.commitBackfill(ctx, candidate, checkpoint, exists, base, prepared, prepareErr, now)
		s.projectionMu.Unlock()
		held += time.Since(locked)
		if prepared != nil {
			prepared.File.Close()
		}
		s.publishApplied(ctx, advance)
		switch outcome {
		case backfillRepaired:
			s.rereadMu.Lock()
			delete(s.rereadRetry, candidate.ThreadID)
			delete(s.rereadAbandons, candidate.ThreadID)
			s.rereadMu.Unlock()
			repaired++
			rebuilt++
		case backfillMarked:
			if shouldDeferTranscriptReread(base, candidate.Source.Source, checkpoint, exists) {
				s.recordTranscriptRereadFailure(candidate.ThreadID, time.Now())
			}
			rebuilt++
		}
	}
	worked := time.Since(started) - waited
	if rebuilt+unaffected > 0 {
		s.logf("transcript parser backfill batch inspected=%d repaired=%d unaffected=%d elapsed=%s locked=%s lock_wait=%s", rebuilt+unaffected, repaired, unaffected, worked, held, waited)
	}
	return rebuilt + unaffected, worked, nil
}

// unaffectedScanBatch bounds how many candidate files one batch reads to
// decide whether a reader upgrade affects them; the scheduler's wait of
// twice the work keeps the backfill's duty time to a third. Candidates past
// the bound are left for the next batch: never handed to a full re-read
// unclassified.
const unaffectedScanBatch = 64

// markUnaffectedTranscripts records, for candidates whose upgrade names a
// marker, those whose file lacks it, and returns the candidates that still
// need a re-read: the ones with the marker, those a marker cannot classify,
// and those it could not read. Candidates beyond this batch's bound are not
// returned; they wait. A newly recorded proof makes the thread's saved
// transcript the current reader's, as a re-read would, so it is announced as
// a re-read's commit is: a reader waiting for it wakes.
func (s *threadCatalogService) markUnaffectedTranscripts(ctx context.Context, candidates []history.TranscriptParserBackfillCandidate, now time.Time) ([]history.TranscriptParserBackfillCandidate, int) {
	rest := candidates[:0]
	marked, scanned := 0, 0
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		target := history.CurrentTranscriptParserVersion(candidate.Provider)
		marker, ok := history.TranscriptUpgradeMarker(candidate.ParserVersion, target)
		if !ok || candidate.CompletionRuleVersion != history.CurrentTranscriptCompletionRuleVersion(candidate.Provider) {
			rest = append(rest, candidate)
			continue
		}
		if scanned == unaffectedScanBatch {
			continue // waits for the next batch, unclassified
		}
		scanned++
		found, err := history.SourceContainsMarker(candidate.Source.Source.Path, marker)
		if err != nil || found {
			rest = append(rest, candidate)
			continue
		}
		recorded, err := s.store.RecordTranscriptParserUnaffected(ctx, candidate, target, candidate.CompletionRuleVersion, now)
		if err != nil {
			s.logf("transcript parser backfill unaffected %s: %v", candidate.ThreadID, err)
			rest = append(rest, candidate)
			continue
		}
		if recorded {
			s.notifyTranscriptChanged(transcriptChange{ThreadID: candidate.ThreadID, Provider: string(candidate.Provider), NativeThreadID: candidate.NativeThreadID})
		}
		marked++
	}
	return rest, marked
}

func prioritizeTranscriptBackfillCandidates(candidates []history.TranscriptParserBackfillCandidate, priorities map[string]int) {
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := priorities[candidates[i].ThreadID], priorities[candidates[j].ThreadID]
		if left != right {
			return left < right
		}
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].ThreadID < candidates[j].ThreadID
	})
}

// backfillOutcome is what one candidate's commit did.
type backfillOutcome int

const (
	// backfillSkipped wrote nothing; the source stays a candidate while it is
	// still behind.
	backfillSkipped backfillOutcome = iota
	backfillRepaired
	// backfillMarked marked the transcript with its rebuild error.
	backfillMarked
)

// commitBackfill runs under projectionMu with what a batch prepared without
// it: base and checkpoint are the source row and transcript checkpoint read
// before parsing. It writes only if it can read both again and they are
// unchanged: a worker pass may have committed a newer source row or
// transcript meanwhile. An older parse committed over a newer source row
// reads as a replaced file, and an error mark would land on the newer
// transcript, so an unchanged check is required for the commit and for the
// mark alike.
func (s *threadCatalogService) commitBackfill(ctx context.Context, candidate history.TranscriptParserBackfillCandidate, checkpoint history.TranscriptCheckpoint, exists bool, base history.StoredSource, prepared *preparedTranscript, prepareErr error, indexedAt time.Time) (backfillOutcome, transcriptAdvance) {
	current, found, err := s.store.LoadSource(ctx, candidate.Source.RootID, candidate.Source.Source.Path)
	var header history.TranscriptCheckpoint
	var hasHeader bool
	if err == nil {
		header, hasHeader, err = s.store.LoadTranscriptCheckpointHeader(ctx, candidate.ThreadID)
	}
	if err != nil {
		s.logf("transcript usage backfill check %s: %v", candidate.ThreadID, err)
		return backfillSkipped, transcriptAdvance{}
	}
	if !found || !reflect.DeepEqual(current, base) || hasHeader != exists || !reflect.DeepEqual(header, checkpoint) {
		return backfillSkipped, transcriptAdvance{}
	}
	advance, err := transcriptAdvance{}, prepareErr
	if err == nil {
		advance, err = s.commitTranscriptSource(ctx, base, prepared, indexedAt)
	}
	switch {
	case errors.Is(err, history.ErrTranscriptChanged):
		return backfillSkipped, advance
	case err != nil:
		s.logf("transcript usage backfill rebuild %s: %v", candidate.ThreadID, err)
		summary := strings.TrimSpace(err.Error())
		if len(summary) > 480 {
			summary = summary[:480]
		}
		if markErr := s.store.MarkTranscriptError(ctx, candidate.ThreadID, candidate.Source.ID, "usage backfill: "+summary, indexedAt); markErr != nil {
			s.logf("transcript usage backfill skip %s: %v", candidate.ThreadID, markErr)
			return backfillSkipped, advance
		}
		return backfillMarked, advance
	default:
		return backfillRepaired, advance
	}
}

// refreshAll is the explicit recovery walk over every root, then names.
func (s *threadCatalogService) refreshAll(ctx context.Context, mode catalogScanMode) []catalogRootScanStats {
	stats := make([]catalogRootScanStats, 0, len(s.roots))
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return stats
		}
		rootStats, err := s.refreshRoot(ctx, root, mode, nil, nil)
		stats = append(stats, rootStats)
		if err != nil {
			s.logf("thread history scan failed for %s: %v", root.Root.Path, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return stats
	}
	if err := s.refreshThreadNames(ctx); err != nil {
		s.logf("thread names refresh failed: %v", err)
	}
	return stats
}
