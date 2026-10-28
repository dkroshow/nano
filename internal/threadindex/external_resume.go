package threadindex

import (
	"context"
	"errors"

	"github.com/dkroshow/nano/internal/history"
)

// VerifyExternalResumeSource re-opens and re-parses the current provider file.
// Catalog membership alone is not availability proof: the index can lag a
// replacement or purge. This path is read-only toward both provider files and
// catalog storage; ParsedFile owns only a disposable normalization spool.
func (s *Service) VerifyExternalResumeSource(ctx context.Context, catalogID, provider, nativeID string) (history.ThreadProjection, error) {
	sources, err := s.store.LoadThreadSources(ctx, catalogID)
	if err != nil {
		return history.ThreadProjection{}, err
	}
	var lastErr error
	for _, source := range sources {
		if source.Status != history.SourcePresent || source.Projection == nil ||
			source.Projection.Provider != history.Provider(provider) ||
			source.Projection.NativeThreadID != nativeID ||
			source.Projection.IdentityEvidence != history.EvidenceNativeDurable {
			continue
		}
		prepared, err := s.prepareTranscriptFile(ctx, source.Source, source, true, history.TranscriptCheckpoint{}, false, false)
		if err != nil {
			lastErr = err
			continue
		}
		projection := prepared.File.Metadata.Projection
		summary := prepared.File.Metadata.Summary
		closeErr := prepared.File.Close()
		if closeErr != nil {
			return history.ThreadProjection{}, closeErr
		}
		if summary.IdentityConflicts != 0 || projection.Provider != history.Provider(provider) ||
			projection.NativeThreadID != nativeID || projection.IdentityEvidence != history.EvidenceNativeDurable {
			lastErr = errors.New("current provider source does not prove the requested durable identity")
			continue
		}
		return projection, nil
	}
	if lastErr != nil {
		return history.ThreadProjection{}, lastErr
	}
	return history.ThreadProjection{}, errors.New("no present provider source proves the requested durable identity")
}
