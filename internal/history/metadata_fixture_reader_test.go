package history

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// This fixture driver exercises the typed catalog reducer over module output.
// Production file/store continuation is covered by transcript_file_test.go.
type metadataFixtureReader struct{ provider Provider }

func (r metadataFixtureReader) Project(ctx context.Context, req ProjectionRequest) (ProjectionResult, error) {
	if req.Reader == nil || req.StartOffset < 0 || req.PriorCompleteLines < 0 || req.MaxLineBytes < 0 || req.Source.Root.Provider != "" && req.Source.Root.Provider != r.provider {
		return ProjectionResult{}, fmt.Errorf("invalid metadata fixture request")
	}
	state := newTranscriptMetadataState(r.provider, req.Source, req.Previous)
	state.identityConflicted = req.PriorIdentityConflicted
	input := model.Input{Source: model.SourceRef{Label: req.Source.Path, Incarnation: "fixture"}, MaxRowBytes: int(req.MaxLineBytes)}
	if r.provider == ProviderGrok {
		if req.StartOffset != 0 {
			return ProjectionResult{}, fmt.Errorf("whole-document fixture cannot append")
		}
		input.Format = "json"
		if filepath.Base(req.Source.Path) == "updates.jsonl" {
			file, err := os.Open(filepath.Join(filepath.Dir(req.Source.Path), "summary.json"))
			if err != nil {
				return ProjectionResult{}, err
			}
			defer file.Close()
			req.Reader = file
		}
	}
	reader, err := newModuleFileReader(r.provider, nil)
	if err != nil {
		return ProjectionResult{}, err
	}
	read, err := readModuleFile(ctx, req.Reader, reader, input, func(batch model.Batch) error {
		for _, observation := range batch.Observations {
			applyTranscriptMetadata(&state, observation)
		}
		recordParsedDiagnostics(batch, &state.diagnostics, &state.summary)
		return nil
	})
	if err != nil {
		return ProjectionResult{}, err
	}
	_, _, complete, lines, err := reader.save()
	if err != nil {
		return ProjectionResult{}, err
	}
	if state.projection.UpdatedAt == nil {
		state.projection.UpdatedAt = cloneTime(state.lastActivity)
	}
	applyFilenameFallback(&state, req.Source)
	if r.provider == ProviderGrok {
		if state.projection.NativeThreadID == "" {
			state.projection.NativeThreadID = grokSessionDirName(req.Source.Path)
			state.projection.IdentityEvidence = EvidenceFilenameFallback
		}
		if filepath.Base(req.Source.Path) == "updates.jsonl" {
			complete = req.Source.Size
		}
	}
	if state.projection.NativeThreadID == "" {
		addDiagnostic(&state.diagnostics, &state.summary, Diagnostic{Kind: DiagnosticMissingIdentity, Message: "fixture source has no identity"})
	}
	return ProjectionResult{Projection: state.projection, CompleteOffset: req.StartOffset + complete,
		CompleteLines: req.PriorCompleteLines + lines, BytesRead: read, Diagnostics: state.diagnostics, Summary: state.summary}, nil
}

// ProjectionRequest supports both full rebuilds and append projection. For an
// append, Reader starts at StartOffset and Previous is the committed prior
// projection. The caller must request a rebuild after truncation/replacement
// or parser-version change.
type ProjectionRequest struct {
	Source      SourceCandidate
	Reader      io.Reader
	StartOffset int64
	// PriorCompleteLines makes diagnostics from append projections retain
	// absolute source line numbers.
	PriorCompleteLines int
	// PriorIdentityConflicted keeps an append quarantined after an earlier
	// durable identity conflict in the same source generation.
	PriorIdentityConflicted bool
	// MaxLineBytes overrides the module default when positive. It exists
	// primarily for controlled tests; zero selects the production default.
	MaxLineBytes int64
	Previous     *ThreadProjection
}
