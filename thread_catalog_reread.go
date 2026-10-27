package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

const (
	// Provisional; set from the active-thread copy test.
	rereadInlineMaxBytes = 1 << 20
	// Provisional; set from the active-thread copy test.
	rereadReadSize = 1 << 20
)

type pendingReread struct {
	since   time.Time
	running bool
}

type rereadRetry struct {
	failures int
	after    time.Time
}

func shouldDeferTranscriptReread(stored history.StoredSource, candidate history.SourceCandidate, checkpoint history.TranscriptCheckpoint, exists bool) bool {
	return exists && stored.CanonicalTranscriptSource && checkpoint.SourceID == stored.ID &&
		(checkpoint.ParserVersion != parserVersion(candidate.Root.Provider) || checkpoint.ErrorSummary != nil) &&
		sameSourceIdentity(stored.Source, candidate) && !transcriptSourceChanged(stored, candidate) && candidate.Size > rereadInlineMaxBytes &&
		!(candidate.Root.Provider == history.ProviderGrok && filepath.Ext(candidate.Path) == ".json")
}

func (s *threadCatalogService) initializeTranscriptRebuilds(ctx context.Context) bool {
	delay := time.Second
	for {
		if err := s.store.AbandonBuildingTranscripts(ctx); err == nil {
			close(s.rereadReady)
			return true
		} else {
			s.logf("transcript rebuild startup abandon failed: %v", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		delay = min(2*delay, time.Minute)
	}
}

func (s *threadCatalogService) waitTranscriptRebuildStartup(ctx context.Context) bool {
	// Services constructed directly by existing synchronous callers have no loops.
	if s.rereadReady == nil {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.rereadReady:
		return true
	}
}

func (s *threadCatalogService) enqueueTranscriptReread(threadID string) {
	s.rereadMu.Lock()
	if s.rereads == nil {
		s.rereads = make(map[string]*pendingReread)
	}
	if s.rereads[threadID] == nil {
		s.rereads[threadID] = &pendingReread{since: time.Now()}
		s.rereadQueue = append(s.rereadQueue, threadID)
		s.logf("transcript reread queued thread=%s", threadID)
	}
	s.rereadMu.Unlock()
	select {
	case s.rereadWake <- struct{}{}:
	default:
	}
}

func (s *threadCatalogService) deferredTranscriptReread(threadID string) bool {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	return s.rereads[threadID] != nil || threadID != "" && s.rereadBuilding == threadID
}

func (s *threadCatalogService) hasDirectTranscriptReread(now time.Time) bool {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	for _, id := range s.rereadQueue {
		if pending := s.rereads[id]; pending != nil && !pending.running && !now.Before(s.rereadRetry[id].after) {
			return true
		}
	}
	return false
}

func (s *threadCatalogService) takeTranscriptReread(now time.Time) (string, time.Time) {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	for _, id := range s.rereadQueue {
		if pending := s.rereads[id]; pending != nil && !pending.running && !now.Before(s.rereadRetry[id].after) {
			pending.running = true
			return id, pending.since
		}
	}
	return "", time.Time{}
}

func (s *threadCatalogService) finishTranscriptReread(threadID string, retry bool, dropReason ...string) {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	if retry {
		if pending := s.rereads[threadID]; pending != nil {
			pending.running = false
		}
		return
	}
	if len(dropReason) > 0 && s.rereads[threadID] != nil {
		s.logf("transcript reread dropped thread=%s reason=%s", threadID, dropReason[0])
	}
	delete(s.rereads, threadID)
	for i, id := range s.rereadQueue {
		if id == threadID {
			s.rereadQueue = append(s.rereadQueue[:i], s.rereadQueue[i+1:]...)
			break
		}
	}
	delete(s.rereadRetry, threadID)
	delete(s.rereadAbandons, threadID)
}

func (s *threadCatalogService) recordTranscriptRereadFailure(threadID string, now time.Time) {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	if s.rereadRetry == nil {
		s.rereadRetry = make(map[string]rereadRetry)
	}
	retry := s.rereadRetry[threadID]
	retry.failures++
	delay := time.Minute
	for i := 1; i < retry.failures && delay < time.Hour; i++ {
		delay = min(2*delay, time.Hour)
	}
	retry.after = now.Add(delay)
	s.rereadRetry[threadID] = retry
}

func (s *threadCatalogService) transcriptRereadCooling(threadID string, now time.Time) bool {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	return now.Before(s.rereadRetry[threadID].after)
}

func (s *threadCatalogService) nextTranscriptRereadRetry(now time.Time) (time.Duration, bool) {
	s.rereadMu.Lock()
	defer s.rereadMu.Unlock()
	var next time.Time
	for _, id := range s.rereadQueue {
		pending := s.rereads[id]
		if pending == nil || pending.running {
			continue
		}
		after := s.rereadRetry[id].after
		if !now.Before(after) {
			return 0, true
		}
		if next.IsZero() || after.Before(next) {
			next = after
		}
	}
	return max(0, next.Sub(now)), !next.IsZero()
}

func (s *threadCatalogService) runDirectTranscriptRereads(ctx context.Context, limit int) (int, time.Duration, bool, error) {
	started := time.Now()
	count := 0
	for count < limit {
		id, queuedAt := s.takeTranscriptReread(time.Now())
		if id == "" {
			break
		}
		checkpoint, exists, err := s.store.LoadTranscriptCheckpointHeader(ctx, id)
		var sources []history.StoredSource
		if err == nil && exists {
			sources, err = s.store.LoadThreadSources(ctx, id)
		}
		var source history.StoredSource
		for _, candidate := range sources {
			if candidate.ID == checkpoint.SourceID && candidate.Status == history.SourcePresent {
				source = candidate
				break
			}
		}
		if err != nil || !exists || source.ID == 0 {
			reason := "removed"
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				reason = "canceled"
			} else if err != nil {
				reason = "error"
			}
			s.finishTranscriptReread(id, false, reason)
			if err != nil {
				return count, time.Since(started), true, err
			}
			continue
		}
		candidate, found, err := history.DiscoverFile(ctx, source.Source.Root, source.Source.Path)
		if err == nil && (!found || !shouldDeferTranscriptReread(source, candidate, checkpoint, true)) {
			reason := "superseded"
			if !found {
				reason = "removed"
			}
			s.finishTranscriptReread(id, false, reason)
			s.RequestPathRefresh([]string{source.Source.Path})
			count++
			continue
		}
		item := history.TranscriptParserBackfillCandidate{ThreadID: id, Source: source, Provider: source.Source.Root.Provider, ParserVersion: checkpoint.ParserVersion, CompletionRuleVersion: checkpoint.CompletionRuleVersion}
		base := source
		var prepared *preparedTranscript
		if err == nil {
			base, prepared, err = s.prepareTranscriptSource(ctx, source, candidate, true, checkpoint, true)
		}
		outcome := backfillSkipped
		advance := transcriptAdvance{}
		if err == nil {
			outcome, advance, err = s.buildPreparedTranscriptReread(ctx, item, checkpoint, base, prepared, true, queuedAt, time.Now().UTC())
		} else {
			s.projectionMu.Lock()
			outcome, advance = s.commitBackfill(ctx, item, checkpoint, true, base, prepared, err, time.Now().UTC())
			s.projectionMu.Unlock()
			if outcome == backfillMarked {
				s.recordTranscriptRereadFailure(id, time.Now())
			}
		}
		if prepared != nil {
			prepared.File.Close()
		}
		s.publishApplied(ctx, advance)
		retry := outcome == backfillSkipped || outcome == backfillMarked
		s.finishTranscriptReread(id, retry)
		if !retry && s.holdsAttachedSource(source.Source.Path) {
			// An attachment's catch-up waits on this file: one targeted pass
			// now proves the reread checkpoint current.
			s.RequestPathRefresh([]string{source.Source.Path})
		}
		count++
		if ctx.Err() != nil {
			return count, time.Since(started), true, ctx.Err()
		}
	}
	return count, time.Since(started), count > 0, nil
}

// buildPreparedTranscriptReread holds projectionMu only while validating and
// switching. The spool's items and search documents commit under writeMu alone.
func (s *threadCatalogService) buildPreparedTranscriptReread(ctx context.Context, candidate history.TranscriptParserBackfillCandidate, checkpoint history.TranscriptCheckpoint, base history.StoredSource, prepared *preparedTranscript, direct bool, queuedAt, indexedAt time.Time) (backfillOutcome, transcriptAdvance, error) {
	started := prepared.Started
	s.rereadMu.Lock()
	s.rereadBuilding = candidate.ThreadID
	s.rereadMu.Unlock()
	defer func() { s.rereadMu.Lock(); s.rereadBuilding = ""; s.rereadMu.Unlock() }()
	preempt := func() bool { return !direct && s.hasDirectTranscriptReread(time.Now()) }
	build, err := s.store.BeginTranscriptRebuild(ctx, prepared.transcriptApplyRequest(base, indexedAt), checkpoint)
	if err == nil {
		err = s.store.WriteTranscriptRebuild(ctx, build, preempt)
	}
	if err == nil {
		err = s.store.IndexTranscriptRebuild(ctx, build, prepared.File.Metadata.Projection.Kind, preempt)
	}
	var applied history.TranscriptApplyResult
	reason := "write"
	if err == nil {
		s.projectionMu.Lock()
		err = s.checkTranscriptRereadBase(ctx, candidate, base, checkpoint)
		reason = "source-or-checkpoint"
		if err == nil {
			_, err = s.store.ApplySource(ctx, history.SourceProjection{Source: prepared.Source, Result: &prepared.File.Metadata, ParserVersion: prepared.File.ParserVersion, Status: history.SourcePresent, IndexedAt: indexedAt}, history.ApplySourceOptions{SkipThreadSearch: candidate.ThreadID})
			reason = "source-apply"
		}
		if err == nil {
			committed, found, loadErr := s.store.LoadSource(ctx, base.RootID, base.Source.Path)
			err = loadErr
			if err == nil && (!found || committed.ThreadID != candidate.ThreadID) {
				err = history.ErrTranscriptChanged
			}
			reason = "thread-reassigned"
		}
		if err == nil {
			applied, err = s.store.SwitchTranscriptRebuild(ctx, build)
			reason = "switch-check"
		}
		s.projectionMu.Unlock()
	}
	if err != nil {
		if build != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			abandonErr := s.store.AbandonTranscriptRebuild(cleanupCtx, build)
			cancel()
			if abandonErr != nil {
				s.logf("transcript reread abandon failed thread=%s: %v", candidate.ThreadID, abandonErr)
				return backfillSkipped, transcriptAdvance{}, errors.Join(err, fmt.Errorf("abandon shadow: %w", abandonErr))
			}
			s.requestSupersededCollection()
		}
		if errors.Is(err, history.ErrTranscriptRebuildPreempted) {
			s.logf("transcript reread abandoned thread=%s reason=preempted", candidate.ThreadID)
			return backfillSkipped, transcriptAdvance{}, err
		}
		if errors.Is(err, history.ErrTranscriptChanged) {
			s.rereadMu.Lock()
			if s.rereadAbandons == nil {
				s.rereadAbandons = make(map[string]int)
			}
			s.rereadAbandons[candidate.ThreadID]++
			abandons := s.rereadAbandons[candidate.ThreadID]
			s.rereadMu.Unlock()
			s.logf("transcript reread abandoned thread=%s reason=%s consecutive=%d", candidate.ThreadID, reason, abandons)
			if abandons >= 3 {
				s.logf("warning: transcript reread repeated abandons thread=%s reason=%s", candidate.ThreadID, reason)
			}
			return backfillSkipped, transcriptAdvance{}, err
		}
		if ctx.Err() == nil {
			s.projectionMu.Lock()
			outcome, _ := s.commitBackfill(ctx, candidate, checkpoint, true, base, nil, err, indexedAt)
			s.projectionMu.Unlock()
			s.recordTranscriptRereadFailure(candidate.ThreadID, time.Now())
			return outcome, transcriptAdvance{}, err
		}
		return backfillSkipped, transcriptAdvance{}, err
	}
	s.rereadMu.Lock()
	delete(s.rereadAbandons, candidate.ThreadID)
	delete(s.rereadRetry, candidate.ThreadID)
	s.rereadMu.Unlock()
	s.logf("transcript reread switched thread=%s queue_wait=%s build_elapsed=%s read_offset=%d", candidate.ThreadID, func() time.Duration {
		if queuedAt.IsZero() {
			return 0
		}
		return started.Sub(queuedAt)
	}(), time.Since(started), prepared.File.Transcript.ReadOffset)
	s.RequestPathRefresh([]string{prepared.Source.Path})
	s.requestSupersededCollection()
	nativeID := ""
	if base.Projection != nil {
		nativeID = base.Projection.NativeThreadID
	}
	return backfillRepaired, transcriptAdvance{applied: applied.Changed, change: transcriptChange{ThreadID: candidate.ThreadID, Provider: string(candidate.Provider), NativeThreadID: nativeID}, prepareStart: prepared.Started, prepareEnd: prepared.Done, savedAt: time.Now(), rebuild: true, projectionKey: applied.ProjectionKey, endOffset: prepared.File.Transcript.ReadOffset, completeOffset: prepared.File.Transcript.CompleteOffset, records: prepared.File.Transcript.CompleteLines, bytesRead: prepared.File.Transcript.BytesRead, diagnostics: prepared.File.Transcript.Summary.Total}, nil
}

func (s *threadCatalogService) checkTranscriptRereadBase(ctx context.Context, candidate history.TranscriptParserBackfillCandidate, base history.StoredSource, checkpoint history.TranscriptCheckpoint) error {
	current, found, err := s.store.LoadSource(ctx, base.RootID, base.Source.Path)
	if err != nil {
		return err
	}
	header, exists, err := s.store.LoadTranscriptCheckpointHeader(ctx, candidate.ThreadID)
	if err != nil {
		return err
	}
	if !found || !reflect.DeepEqual(current, base) || !exists || !reflect.DeepEqual(header, checkpoint) {
		return history.ErrTranscriptChanged
	}
	return nil
}
