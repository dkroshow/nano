package threadindex

import (
	"context"
	"errors"
	"time"
)

func (s *Service) requestSupersededCollection() {
	select {
	case s.supersededCollection <- struct{}{}:
	default:
	}
}

// runSupersededProjectionCollector starts after the store opens. It collects
// derived generations in small, independently committed batches with an
// explicit yield between them. A periodic pass covers projections superseded
// by live rebuilds after startup.
func (s *Service) runSupersededProjectionCollector(ctx context.Context) {
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
func (s *Service) runParserStateCompaction(ctx context.Context) {
	defer close(s.compactionDone)
	if !s.waitTranscriptRebuildStartup(ctx) {
		return
	}
	if err := s.compactParserState(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logf("parser state compaction: %v", err)
	}
}

func (s *Service) compactParserState(ctx context.Context) error {
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

func (s *Service) collectSupersededProjections(ctx context.Context) error {
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
