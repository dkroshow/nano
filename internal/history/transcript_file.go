package history

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	parseclaude "github.com/dkroshow/nano/internal/transcriptparse/claude"
	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	parsegrok "github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const parsedFileVersion = "history-file-v11" // Compact keys and lossless observation facts.

// ParsedCatalogInput supplies an independent catalog snapshot, such as Grok's
// summary beside its updates stream. The caller opens and validates both files.
type ParsedCatalogInput struct {
	Source SourceCandidate
	Reader io.Reader
	Format string
}

// ParsedFileRequest reads from zero on rebuild, or Checkpoint.ReadOffset on
// append. Ordinary JSONL EOF keeps its unfinished tail open in continuation.
// Whole JSON documents close at EOF and require a rebuild when changed.
type ParsedFileRequest struct {
	Source     SourceCandidate
	Reader     io.Reader
	Format     string
	Checkpoint *TranscriptCheckpoint
	Catalog    *ParsedCatalogInput
	SpoolDir   string
	// ReadSize bounds normalized spool batches on a deferred re-read; zero keeps normal reads.
	ReadSize int
	// File, when set, is the whole source addressed by absolute offset. The
	// checkpoint then records a digest of the bytes just before ReadOffset, so
	// a later touch that changes only the modification time can be proven
	// harmless (TranscriptSourceTailMatches).
	File io.ReaderAt
}

// ParsedFile owns a temporary normalized spool. Reconcile Metadata, reload the
// committed source guards, then pass Transcript to ApplyTranscript. Close after
// application (including on failure); neither result owns the native reader.
type ParsedFile struct {
	Metadata              ProjectionResult
	Transcript            TranscriptProjectionResult
	ParserVersion         string
	CompletionRuleVersion string
	// spool is already unlinked; only this descriptor keeps it readable.
	spool     *os.File
	spoolSize int64
}

func (p *ParsedFile) Close() error {
	if p.spool == nil {
		return nil
	}
	err := p.spool.Close()
	p.spool = nil
	return err
}

type parsedFileCheckpoint struct {
	Version            string           `json:"version"`
	Provider           Provider         `json:"provider"`
	Source             string           `json:"source"`
	ProjectionKey      string           `json:"projectionKey"`
	Format             string           `json:"format"`
	SeparateCatalog    bool             `json:"separateCatalog"`
	Reader             json.RawMessage  `json:"reader"`
	Metadata           ThreadProjection `json:"metadata"`
	LastActivity       *time.Time       `json:"lastActivity,omitempty"`
	IdentityConflicted bool             `json:"identityConflicted,omitempty"`
	// TailSHA256 digests the source bytes [TailStart, ReadOffset) as parsed.
	TailStart  int64  `json:"tailStart,omitempty"`
	TailSHA256 string `json:"tailSha256,omitempty"`
}

// ParsedFileVersions includes every module version and the history conversion
// version. Recovery can inspect these without loading a saved checkpoint body.
func ParsedFileVersions(provider Provider) (parser, completion string, err error) {
	reader, err := newModuleFileReader(provider, nil)
	if err != nil {
		return "", "", err
	}
	batch, err := reader.feed(model.Input{Source: model.SourceRef{Label: "versions", Incarnation: "versions"}})
	if err != nil {
		return "", "", err
	}
	v := batch.Versions
	return fmt.Sprintf("%s:%s:%s:%s:%s:%d", parsedFileVersion, v.Schema, v.Reader, v.Taxonomy, v.Usage, v.Checkpoint), v.Assembly, nil
}

// ProjectParsedFile performs all native parsing before any store transaction.
// Only continuation and a typed catalog reduction remain in its checkpoint;
// previously emitted batches live in the disposable sequential spool.
func ProjectParsedFile(ctx context.Context, req ParsedFileRequest) (_ *ParsedFile, err error) {
	if req.Reader == nil || req.Source.Path == "" {
		return nil, errors.New("parsed file requires a source and reader")
	}
	if req.Format == "" {
		req.Format = "jsonl"
	}
	provider := req.Source.Root.Provider
	parser, completion, err := ParsedFileVersions(provider)
	if err != nil {
		return nil, err
	}
	state := parsedFileCheckpoint{Version: parsedFileVersion, Provider: provider, Source: req.Source.Path, Format: req.Format, SeparateCatalog: req.Catalog != nil}
	metadata := newTranscriptMetadataState(provider, req.Source, nil)
	if cp := req.Checkpoint; cp != nil {
		if cp.Provider != provider || cp.ParserVersion != parser || cp.CompletionRuleVersion != completion || req.Format != "jsonl" || len(cp.ParserState) == 0 {
			return nil, ErrTranscriptChanged
		}
		state = parsedFileCheckpoint{}
		if err := json.Unmarshal(cp.ParserState, &state); err != nil {
			return nil, fmt.Errorf("decode file continuation: %w", ErrTranscriptChanged)
		}
		if state.Version != parsedFileVersion || state.Provider != provider || state.Source != req.Source.Path || state.ProjectionKey == "" || state.ProjectionKey != cp.ProjectionKey || state.Format != req.Format || state.SeparateCatalog != (req.Catalog != nil) || len(state.Reader) == 0 {
			return nil, ErrTranscriptChanged
		}
		if state.Metadata.Provider != provider {
			return nil, ErrTranscriptChanged
		}
		metadata.projection, metadata.lastActivity, metadata.identityConflicted = state.Metadata, state.LastActivity, state.IdentityConflicted
	} else {
		state.ProjectionKey, err = mintOpaqueID("ntp_")
		if err != nil {
			return nil, err
		}
	}
	reader, err := newModuleFileReader(provider, state.Reader)
	if err != nil {
		return nil, fmt.Errorf("restore file reader: %w", ErrTranscriptChanged)
	}
	input := model.Input{Format: req.Format, Source: model.SourceRef{Label: req.Source.Path, Incarnation: state.ProjectionKey}}
	initial, err := reader.feed(input)
	if err != nil {
		if req.Checkpoint != nil {
			return nil, fmt.Errorf("prepare file reader: %w", ErrTranscriptChanged)
		}
		return nil, fmt.Errorf("prepare file reader: %w", err)
	}
	if cp := req.Checkpoint; cp != nil {
		_, read, complete, lines, saveErr := reader.save()
		if saveErr != nil || read != cp.ReadOffset || complete != cp.CompleteOffset || lines != cp.CompleteLines {
			return nil, ErrTranscriptChanged
		}
	}
	spool, err := os.CreateTemp(req.SpoolDir, parsedSpoolPattern)
	if err != nil {
		return nil, fmt.Errorf("create parsed spool: %w", err)
	}
	// Unlink at once. The open descriptor keeps the spool readable, and the
	// system frees it when the descriptor closes or the process dies, so a
	// killed or restarted Nano cannot leave it behind (13.9 GiB had
	// accumulated in the temp directory by 2026-09-21).
	if err = os.Remove(spool.Name()); err != nil {
		_ = spool.Close()
		return nil, fmt.Errorf("unlink parsed spool: %w", err)
	}
	result := &ParsedFile{ParserVersion: parser, CompletionRuleVersion: completion, spool: spool}
	defer func() {
		if err != nil {
			_ = result.Close()
		}
	}()
	compressed := gzip.NewWriter(spool)
	defer compressed.Close()
	encoder := json.NewEncoder(compressed)
	var diagnostics []Diagnostic
	var summary DiagnosticSummary
	accept := func(batch model.Batch) error {
		recordParsedDiagnostics(batch, &diagnostics, &summary)
		if req.Catalog == nil {
			recordParsedDiagnostics(batch, &metadata.diagnostics, &metadata.summary)
			for _, observation := range batch.Observations {
				applyTranscriptMetadata(&metadata, observation)
			}
		}
		if len(batch.Records)+len(batch.Observations)+len(batch.Changes)+len(batch.Diagnostics) == 0 {
			return nil
		}
		// Storage already selects the latest turn snapshot in this batch.
		// Do so before encoding its growing usage/context arrays repeatedly.
		// Item updates still carry every intermediate native alias scope.
		batch.Changes = compactParsedTurnChanges(batch.Changes)
		return encoder.Encode(batch)
	}
	if err = accept(initial); err != nil {
		return nil, err
	}
	bytesRead, err := readModuleFile(ctx, req.Reader, reader, input, accept, req.ReadSize)
	if err != nil {
		return nil, fmt.Errorf("parse provider file: %w", err)
	}
	raw, readOffset, completeOffset, lines, err := reader.save()
	if err != nil {
		return nil, err
	}
	if req.Catalog != nil {
		catalog := req.Catalog
		if catalog.Reader == nil || catalog.Source.Path == "" || catalog.Source.Root.Provider != provider {
			return nil, errors.New("invalid separate catalog input")
		}
		metadata = newTranscriptMetadataState(provider, req.Source, nil)
		catalogReader, readErr := newModuleFileReader(provider, nil)
		if readErr != nil {
			return nil, readErr
		}
		incarnation, mintErr := mintOpaqueID("catalog_")
		if mintErr != nil {
			return nil, mintErr
		}
		catalogInput := model.Input{Format: catalog.Format, Source: model.SourceRef{Label: catalog.Source.Path, Incarnation: incarnation}}
		_, readErr = readModuleFile(ctx, catalog.Reader, catalogReader, catalogInput, func(batch model.Batch) error {
			for _, observation := range batch.Observations {
				applyTranscriptMetadata(&metadata, observation)
			}
			recordParsedDiagnostics(batch, &metadata.diagnostics, &metadata.summary)
			// A separate catalog supplies facts, never another transcript's rows.
			batch.Changes = nil
			return encoder.Encode(batch)
		}, req.ReadSize)
		if readErr != nil {
			return nil, fmt.Errorf("parse catalog file: %w", readErr)
		}
	}
	if err = compressed.Close(); err != nil {
		return nil, fmt.Errorf("finish parsed spool: %w", err)
	}
	info, err := spool.Stat()
	if err != nil {
		return nil, fmt.Errorf("measure parsed spool: %w", err)
	}
	result.spoolSize = info.Size()
	state.Reader, state.Metadata, state.LastActivity, state.IdentityConflicted = raw, metadata.projection, metadata.lastActivity, metadata.identityConflicted
	state.TailStart, state.TailSHA256 = 0, ""
	if req.File != nil {
		if start, digest, tailErr := sourceTailDigest(req.File, readOffset); tailErr == nil {
			state.TailStart, state.TailSHA256 = start, digest
		}
	}
	checkpoint, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	// Fallback display facts must not become input facts on the next append.
	if metadata.projection.UpdatedAt == nil {
		metadata.projection.UpdatedAt = cloneTime(metadata.lastActivity)
	}
	applyFilenameFallback(&metadata, req.Source)
	if provider == ProviderGrok && metadata.projection.NativeThreadID == "" {
		if fallback := grokSessionDirName(req.Source.Path); fallback != "" {
			metadata.projection.NativeThreadID, metadata.projection.IdentityEvidence = fallback, EvidenceFilenameFallback
		}
	}
	if metadata.projection.NativeThreadID == "" {
		addDiagnostic(&metadata.diagnostics, &metadata.summary, Diagnostic{Kind: DiagnosticMissingIdentity, Message: "provider file has no durable or filename native identity"})
	}
	result.Metadata = ProjectionResult{Projection: metadata.projection, CompleteOffset: completeOffset, CompleteLines: lines, BytesRead: bytesRead, Diagnostics: metadata.diagnostics, Summary: metadata.summary}
	result.Transcript = TranscriptProjectionResult{ProjectionKey: state.ProjectionKey, ReadOffset: readOffset, CompleteOffset: completeOffset, CompleteLines: lines, BytesRead: bytesRead, ParserState: checkpoint, Diagnostics: diagnostics, Summary: summary, parsedBatches: result.visit, recordSource: req.Source.Path}
	if provider == ProviderGrok && req.Catalog != nil {
		result.Transcript.sessionUsage = &result.Metadata.Projection.Metadata
	}
	return result, nil
}

func (p *ParsedFile) visit(yield func(model.Batch) error) error {
	if p.spool == nil {
		return errors.New("open parsed spool: spool is closed")
	}
	compressed, err := gzip.NewReader(io.NewSectionReader(p.spool, 0, p.spoolSize))
	if err != nil {
		return fmt.Errorf("open compressed spool: %w", err)
	}
	defer compressed.Close()
	decoder := json.NewDecoder(compressed)
	for {
		var batch model.Batch
		if err := decoder.Decode(&batch); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read parsed spool: %w", err)
		}
		if err := yield(batch); err != nil {
			return err
		}
	}
}

func readModuleFile(ctx context.Context, source io.Reader, reader moduleFileReader, input model.Input, accept func(model.Batch) error, sizes ...int) (int64, error) {
	readSize := 4 << 20
	if len(sizes) > 0 && sizes[0] > 0 {
		readSize = sizes[0]
	}
	buffer := make([]byte, readSize)
	var count int64
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		n, err := io.ReadFull(source, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return count, err
		}
		count += int64(n)
		input.Bytes, input.Closed = buffer[:n], input.Format == "json" && err != nil
		batch, parseErr := reader.feed(input)
		if parseErr != nil {
			return count, parseErr
		}
		if err := accept(batch); err != nil {
			return count, err
		}
		if err != nil {
			return count, nil
		}
	}
}

func recordParsedDiagnostics(batch model.Batch, diagnostics *[]Diagnostic, summary *DiagnosticSummary) {
	for _, record := range batch.Records {
		var kind DiagnosticKind
		switch record.Disposition {
		case model.RecordMalformed:
			kind = DiagnosticMalformed
			if record.Capture == model.CaptureTruncated {
				kind = DiagnosticOversizedLine
			}
		case model.RecordUnknown:
			kind = DiagnosticUnknownRow
		default:
			continue
		}
		addDiagnostic(diagnostics, summary, Diagnostic{Kind: kind, Line: int(record.Source.Line), Message: "provider record could not be fully interpreted"})
	}
}

type moduleFileReader struct {
	feed func(model.Input) (model.Batch, error)
	save func() (json.RawMessage, int64, int64, int, error)
	// compact reads nothing, which applies the reader's retention rule to a
	// stored checkpoint.
	compact func() error
}

func newModuleFileReader(provider Provider, raw json.RawMessage) (moduleFileReader, error) {
	switch provider {
	case ProviderClaude:
		var state parseclaude.State
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return moduleFileReader{}, err
			}
			if state.Version == 0 {
				return moduleFileReader{}, ErrTranscriptChanged
			}
		}
		return moduleFileReader{
			feed: func(input model.Input) (batch model.Batch, err error) {
				state, batch, err = parseclaude.ParseFile(state, input)
				return
			},
			compact: func() (err error) {
				state, _, err = parseclaude.ParseFile(state, model.Input{Source: model.SourceRef{Label: state.Source.Label, Incarnation: state.Source.Incarnation}})
				return
			},
			save: func() (json.RawMessage, int64, int64, int, error) {
				raw, err := json.Marshal(state)
				return raw, state.Framing.Offset, state.Framing.Checkpoint, completeFileLines(state.Framing.Line), err
			},
		}, nil
	case ProviderCodex:
		var state parsecodex.State
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return moduleFileReader{}, err
			}
			if state.Version == 0 {
				return moduleFileReader{}, ErrTranscriptChanged
			}
		}
		return moduleFileReader{
			feed: func(input model.Input) (batch model.Batch, err error) {
				state, batch, err = parsecodex.ParseFile(state, input)
				return
			},
			compact: func() (err error) {
				state, _, err = parsecodex.ParseFile(state, model.Input{Source: model.SourceRef{Label: state.Source.Label, Incarnation: state.Source.Incarnation}})
				return
			},
			save: func() (json.RawMessage, int64, int64, int, error) {
				raw, err := json.Marshal(state)
				return raw, state.Framing.Offset, state.Framing.Checkpoint, completeFileLines(state.Framing.Line), err
			},
		}, nil
	case ProviderGrok:
		var state parsegrok.State
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return moduleFileReader{}, err
			}
			if state.Version == 0 {
				return moduleFileReader{}, ErrTranscriptChanged
			}
		}
		return moduleFileReader{
			feed: func(input model.Input) (batch model.Batch, err error) {
				state, batch, err = parsegrok.ParseFile(state, input)
				return
			},
			compact: func() (err error) {
				if state.Format == "json" {
					return nil // a whole document is read once; nothing accrues
				}
				state, _, err = parsegrok.ParseFile(state, model.Input{Source: model.SourceRef{Label: state.Source.Label, Incarnation: state.Source.Incarnation}})
				return
			},
			save: func() (json.RawMessage, int64, int64, int, error) {
				raw, err := json.Marshal(state)
				if state.Format == "json" {
					return raw, state.DocumentBytes, state.DocumentBytes, 1, err
				}
				return raw, state.Framing.Offset, state.Framing.Checkpoint, completeFileLines(state.Framing.Line), err
			},
		}, nil
	default:
		return moduleFileReader{}, fmt.Errorf("unsupported history provider %q", provider)
	}
}

func completeFileLines(next int64) int {
	if next < 1 {
		return 0
	}
	return int(next - 1)
}
