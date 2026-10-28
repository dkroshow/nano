package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

type preparedTranscript struct {
	File                  *history.ParsedFile
	Source                history.SourceCandidate
	Mode                  history.TranscriptProjectionMode
	ExpectedProjectionKey string
	HadCheckpoint         bool
	// Started and Done bound this preparation: open, read, parse and spool.
	Started, Done time.Time
}

func canAppendParsedFile(checkpoint history.TranscriptCheckpoint, stored history.StoredSource, current history.SourceCandidate) bool {
	parser, completion, err := history.ParsedFileVersions(current.Root.Provider)
	return err == nil && checkpoint.ErrorSummary == nil && checkpoint.ProjectionKey != "" &&
		checkpoint.ThreadID == stored.ThreadID && checkpoint.SourceID == stored.ID && checkpoint.SourceGeneration == stored.Generation &&
		checkpoint.Provider == current.Root.Provider && checkpoint.ParserVersion == parser && checkpoint.CompletionRuleVersion == completion &&
		checkpoint.CompleteOffset >= 0 && checkpoint.ReadOffset >= checkpoint.CompleteOffset && checkpoint.ReadOffset <= current.Size &&
		!(current.Root.Provider == history.ProviderGrok && filepath.Ext(current.Path) == ".json")
}

// verifyTouch lets a rebuild that was forced only by a modification-time touch
// become a zero-byte append when the saved tail digest proves the bytes are
// unchanged. Deliberate rebuilds (repair, parser upgrades) never pass it.
func (s *threadCatalogService) prepareTranscriptFile(ctx context.Context, discovered history.SourceCandidate, stored history.StoredSource, forceRebuild bool, header history.TranscriptCheckpoint, hasCheckpoint bool, verifyTouch bool) (*preparedTranscript, error) {
	started := time.Now()
	file, candidate, err := openDiscoveredSource(discovered)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Only the previously proven immutable parent edge may survive a missing
	// sidecar. Mutable catalog values are reduced from the chosen source again.
	if candidate.Root.Provider == history.ProviderGrok && candidate.ParentNativeID == "" && stored.Projection != nil && stored.Projection.Kind == history.ThreadSubagent {
		candidate.ParentNativeID = stored.Projection.ParentNativeID
	}
	prepared := &preparedTranscript{Source: candidate, Mode: history.TranscriptRebuild, HadCheckpoint: hasCheckpoint, Started: started}
	if hasCheckpoint {
		prepared.ExpectedProjectionKey = header.ProjectionKey
	}
	request := history.ParsedFileRequest{Source: candidate, Format: "jsonl", File: file}
	if candidate.Root.Provider == history.ProviderGrok && candidate.ParentNativeID != "" {
		request.Source.Kind = history.ThreadSubagent
	}
	start := int64(0)
	if hasCheckpoint && !forceRebuild && !transcriptSourceChanged(stored, candidate) && canAppendParsedFile(header, stored, candidate) {
		checkpoint, exists, loadErr := s.store.LoadTranscriptCheckpointForAppend(ctx, stored.ThreadID)
		if loadErr != nil {
			return nil, loadErr
		}
		if !exists || checkpoint.ProjectionKey != header.ProjectionKey || checkpoint.ReadOffset != header.ReadOffset || !canAppendParsedFile(checkpoint, stored, candidate) {
			return nil, history.ErrTranscriptChanged
		}
		request.Checkpoint = &checkpoint
		prepared.Mode = history.TranscriptAppend
		start = checkpoint.ReadOffset
	} else if verifyTouch && hasCheckpoint && transcriptSourceTouchedOnly(stored, candidate) && canAppendParsedFile(header, stored, candidate) {
		// A proven touch keeps the projection; anything unproven rebuilds as before.
		checkpoint, exists, loadErr := s.store.LoadTranscriptCheckpoint(ctx, stored.ThreadID)
		if loadErr != nil {
			return nil, loadErr
		}
		if exists && checkpoint.ProjectionKey == header.ProjectionKey && checkpoint.ReadOffset == candidate.Size &&
			canAppendParsedFile(checkpoint, stored, candidate) && history.TranscriptSourceTailMatches(checkpoint, file) {
			request.Checkpoint = &checkpoint
			prepared.Mode = history.TranscriptAppend
			start = checkpoint.ReadOffset
		}
	}
	if candidate.Root.Provider == history.ProviderGrok {
		if filepath.Ext(candidate.Path) == ".json" {
			request.Format = "json"
		} else if filepath.Base(candidate.Path) == "updates.jsonl" {
			summaryPath := filepath.Join(filepath.Dir(candidate.Path), "summary.json")
			summary, openErr := os.Open(summaryPath)
			if openErr != nil {
				return nil, fmt.Errorf("open catalog summary: %w", openErr)
			}
			defer summary.Close()
			info, statErr := summary.Stat()
			if statErr != nil {
				return nil, statErr
			}
			catalogSource := candidate
			catalogSource.Path, catalogSource.Size = summaryPath, info.Size()
			request.Catalog = &history.ParsedCatalogInput{Source: catalogSource, Reader: io.NewSectionReader(summary, 0, info.Size()), Format: "json"}
		}
	}
	if forceRebuild && hasCheckpoint && shouldDeferTranscriptReread(stored, candidate, header, true) {
		request.ReadSize = rereadReadSize
	}
	request.Reader = readProgressFrom(ctx, io.NewSectionReader(file, start, candidate.Size-start))
	parsed, err := history.ProjectParsedFile(ctx, request)
	if err != nil {
		return nil, err
	}
	prepared.File, prepared.Done = parsed, time.Now()
	return prepared, nil
}

func (s *threadCatalogService) projectSource(ctx context.Context, discovered history.SourceCandidate, stored history.StoredSource, exists bool, header history.TranscriptCheckpoint, hasCheckpoint bool, indexedAt time.Time) (history.SourceProjection, *preparedTranscript, error) {
	force := exists && transcriptSourceChanged(stored, discovered)
	prepared, err := s.prepareTranscriptFile(ctx, discovered, stored, force, header, hasCheckpoint, true)
	if err != nil {
		return history.SourceProjection{}, nil, err
	}
	if prepared.Mode == history.TranscriptAppend && discovered.Root.Provider != history.ProviderGrok {
		mergeAppendDiagnostics(&prepared.File.Metadata, stored)
	}
	// A rebuild of a file changed in place, as discovered or as opened, must
	// not commit its source row beside the old transcript as if that were
	// current. A proven touch appends; a parser-only rebuild saw no change.
	rewritten := prepared.Mode == history.TranscriptRebuild && (force || exists && transcriptSourceChanged(stored, prepared.Source))
	return history.SourceProjection{Source: prepared.Source, Result: &prepared.File.Metadata, ParserVersion: prepared.File.ParserVersion, Status: history.SourcePresent, IndexedAt: indexedAt, Rewritten: rewritten}, prepared, nil
}

// transcriptAdvance is what one transcript apply committed: source offsets
// and complete records within its projection, when its preparation ran and
// when it was saved. A rebuild starts from zero. change is the notification
// the caller publishes once it has recorded the apply (publishApplied).
type transcriptAdvance struct {
	applied        bool
	change         transcriptChange
	prepareStart   time.Time
	prepareEnd     time.Time
	savedAt        time.Time
	rebuild        bool
	projectionKey  string
	startOffset    int64
	endOffset      int64
	completeOffset int64
	records        int
	bytesRead      int64
	diagnostics    int
}

func (s *threadCatalogService) applyPreparedTranscript(ctx context.Context, stored history.StoredSource, checkpoint history.TranscriptCheckpoint, exists bool, prepared *preparedTranscript, indexedAt time.Time) (transcriptAdvance, error) {
	if exists != prepared.HadCheckpoint || (exists && checkpoint.ProjectionKey != prepared.ExpectedProjectionKey) || transcriptSourceChanged(stored, prepared.Source) {
		return transcriptAdvance{}, history.ErrTranscriptChanged
	}
	if prepared.Mode == history.TranscriptAppend && !canAppendParsedFile(checkpoint, stored, prepared.Source) {
		return transcriptAdvance{}, history.ErrTranscriptChanged
	}
	result := prepared.File.Transcript
	advance := transcriptAdvance{
		prepareStart: prepared.Started, prepareEnd: prepared.Done,
		rebuild: prepared.Mode == history.TranscriptRebuild, endOffset: max(result.ReadOffset, result.CompleteOffset),
		completeOffset: result.CompleteOffset, records: result.CompleteLines, bytesRead: result.BytesRead, diagnostics: result.Summary.Total,
	}
	if !advance.rebuild {
		advance.startOffset, advance.records = checkpoint.ReadOffset, result.CompleteLines-checkpoint.CompleteLines
	}
	applied, err := s.store.ApplyTranscript(ctx, prepared.transcriptApplyRequest(stored, indexedAt))
	if err != nil {
		return transcriptAdvance{}, err
	}
	advance.applied, advance.projectionKey, advance.savedAt = applied.Changed, applied.ProjectionKey, time.Now()
	if applied.Changed {
		nativeID := ""
		if stored.Projection != nil {
			nativeID = stored.Projection.NativeThreadID
		}
		advance.change = transcriptChange{ThreadID: stored.ThreadID, Provider: string(prepared.Source.Root.Provider), NativeThreadID: nativeID}
	}
	return advance, nil
}

// publishApplied tells transcript consumers and catalog subscribers about a
// committed apply. Callers record the apply first, so anything the
// notification starts sees that record.
func (s *threadCatalogService) publishApplied(ctx context.Context, advance transcriptAdvance) {
	if advance.change.ThreadID == "" {
		return
	}
	s.notifyTranscriptChanged(advance.change)
	if version, err := s.store.CatalogVersion(ctx); err == nil {
		s.publishCatalogThreads(version, []string{advance.change.ThreadID})
	}
}

func (s *threadCatalogService) projectTranscriptSource(ctx context.Context, stored history.StoredSource, discovered history.SourceCandidate, forceRebuild bool, checkpoint history.TranscriptCheckpoint, exists bool, indexedAt time.Time) (transcriptAdvance, error) {
	if exists && !forceRebuild && canAppendParsedFile(checkpoint, stored, discovered) && discovered.Size == checkpoint.ReadOffset {
		return transcriptAdvance{}, nil
	}
	// Version-only transcript repairs stay with the bounded background scheduler.
	if exists && !forceRebuild && checkpoint.ParserVersion != parserVersion(discovered.Root.Provider) && discovered.Size == checkpoint.ReadOffset {
		return transcriptAdvance{}, nil
	}
	current, prepared, err := s.prepareTranscriptSource(ctx, stored, discovered, forceRebuild, checkpoint, exists)
	if err != nil {
		return transcriptAdvance{}, err
	}
	defer prepared.File.Close()
	return s.commitTranscriptSource(ctx, current, prepared, indexedAt)
}

// prepareTranscriptSource reads and parses a source against its current
// catalog row and writes nothing. It returns the row it read, also with an
// error from reading the file, so a caller that commits later can check that
// the row is still the same.
func (s *threadCatalogService) prepareTranscriptSource(ctx context.Context, stored history.StoredSource, discovered history.SourceCandidate, forceRebuild bool, checkpoint history.TranscriptCheckpoint, exists bool) (history.StoredSource, *preparedTranscript, error) {
	current, found, err := s.store.LoadSource(ctx, stored.RootID, stored.Source.Path)
	if err != nil {
		return history.StoredSource{}, nil, err
	}
	if !found || transcriptSourceChanged(current, discovered) {
		return current, nil, history.ErrTranscriptChanged
	}
	prepared, err := s.prepareTranscriptFile(ctx, discovered, current, forceRebuild, checkpoint, exists, false)
	if err != nil {
		return current, nil, err
	}
	if prepared.Mode == history.TranscriptAppend && discovered.Root.Provider != history.ProviderGrok {
		mergeAppendDiagnostics(&prepared.File.Metadata, current)
	}
	return current, prepared, nil
}

// commitTranscriptSource saves a prepared source row and then its transcript.
// stored is the row the preparation read.
func (s *threadCatalogService) commitTranscriptSource(ctx context.Context, stored history.StoredSource, prepared *preparedTranscript, indexedAt time.Time) (transcriptAdvance, error) {
	metadata := history.SourceProjection{Source: prepared.Source, Result: &prepared.File.Metadata, ParserVersion: prepared.File.ParserVersion, Status: history.SourcePresent, IndexedAt: indexedAt}
	if _, err := s.store.ApplySource(ctx, metadata); err != nil {
		return transcriptAdvance{}, err
	}
	committed, found, err := s.store.LoadSource(ctx, stored.RootID, stored.Source.Path)
	if err != nil {
		return transcriptAdvance{}, err
	}
	if !found || committed.ThreadID == "" || !committed.CanonicalTranscriptSource {
		return transcriptAdvance{}, history.ErrTranscriptChanged
	}
	after, hasAfter, err := s.store.LoadTranscriptCheckpointHeader(ctx, committed.ThreadID)
	if err != nil {
		return transcriptAdvance{}, err
	}
	return s.applyPreparedTranscript(ctx, committed, after, hasAfter, prepared, indexedAt)
}

func (prepared *preparedTranscript) transcriptApplyRequest(stored history.StoredSource, indexedAt time.Time) history.TranscriptApplyRequest {
	return history.TranscriptApplyRequest{
		Mode: prepared.Mode, ThreadID: stored.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: prepared.Source.Root.Provider,
		ParserVersion: prepared.File.ParserVersion, CompletionRuleVersion: prepared.File.CompletionRuleVersion,
		ExpectedProjectionKey: prepared.ExpectedProjectionKey, ExpectNoProjection: !prepared.HadCheckpoint, IndexedAt: indexedAt, Result: prepared.File.Transcript,
	}
}
