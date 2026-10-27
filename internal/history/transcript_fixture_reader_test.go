package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Existing storage tests need a materialized view to mutate and insert. Build
// that view through the real module, spool and SQLite conversion first. This
// driver contains no native transcript interpretation.
type transcriptFixtureReader struct {
	t        *testing.T
	provider Provider
	saved    map[string]*transcriptFixtureProjection
}

type transcriptFixtureProjection struct {
	store      *Store
	source     SourceProjection
	input      SourceCandidate
	threadID   string
	checkpoint TranscriptCheckpoint
}

func newTranscriptFixture(t *testing.T, provider Provider) *transcriptFixtureReader {
	t.Helper()
	return &transcriptFixtureReader{t: t, provider: provider, saved: map[string]*transcriptFixtureProjection{}}
}

func (r *transcriptFixtureReader) ParserVersion() string {
	parser, _, err := ParsedFileVersions(r.provider)
	if err != nil {
		r.t.Fatal(err)
	}
	return parser
}

func (r *transcriptFixtureReader) CompletionRuleVersion() string {
	_, completion, err := ParsedFileVersions(r.provider)
	if err != nil {
		r.t.Fatal(err)
	}
	return completion
}

func (r *transcriptFixtureReader) ProjectTranscript(ctx context.Context, req TranscriptProjectionRequest) (TranscriptProjectionResult, error) {
	r.t.Helper()
	if req.Reader == nil {
		return TranscriptProjectionResult{}, fmt.Errorf("fixture requires a reader")
	}
	raw, err := io.ReadAll(req.Reader)
	if err != nil {
		return TranscriptProjectionResult{}, err
	}
	input := req.Source
	input.Root.Provider = r.provider
	if input.Path == "" {
		input.Path = "/fixture/transcript.jsonl"
	}
	var prior *transcriptFixtureProjection
	var checkpoint *TranscriptCheckpoint
	if len(req.PreviousState) != 0 {
		prior = r.saved[req.ProjectionKey]
		if prior == nil {
			return TranscriptProjectionResult{}, fmt.Errorf("fixture has no persisted projection")
		}
		cp := prior.checkpoint
		cp.ParserState = req.PreviousState
		checkpoint, input = &cp, prior.input
		// The retired request begins at the completed boundary. The module
		// already retains the unfinished prefix in its continuation.
		skip := cp.ReadOffset - req.StartOffset
		if skip < 0 || skip > int64(len(raw)) {
			return TranscriptProjectionResult{}, ErrTranscriptChanged
		}
		raw = raw[skip:]
	}
	parseReq := ParsedFileRequest{Source: input, Reader: bytes.NewReader(raw), Checkpoint: checkpoint, SpoolDir: r.t.TempDir()}
	if r.provider == ProviderGrok {
		if filepath.Ext(input.Path) == ".json" {
			parseReq.Format = "json"
		} else if filepath.Base(input.Path) == "updates.jsonl" {
			path := filepath.Join(filepath.Dir(input.Path), "summary.json")
			file, err := os.Open(path)
			if err != nil && !os.IsNotExist(err) {
				return TranscriptProjectionResult{}, err
			}
			if err == nil {
				defer file.Close()
				source := input
				source.Path = path
				parseReq.Catalog = &ParsedCatalogInput{Source: source, Reader: file, Format: "json"}
			}
		}
	}
	parsed, err := ProjectParsedFile(ctx, parseReq)
	if err != nil {
		return TranscriptProjectionResult{}, err
	}
	defer parsed.Close()
	if prior == nil {
		root := SourceRoot{Provider: r.provider, Kind: RootCodexSessions, Path: r.t.TempDir()}
		if r.provider == ProviderClaude {
			root.Kind = RootClaudeProjects
		} else if r.provider == ProviderGrok {
			root.Kind = RootGrokSessions
		}
		native := req.NativeThreadID
		if native == "" {
			native = parsed.Metadata.Projection.NativeThreadID
		}
		if native == "" {
			native = "fixture-native"
		}
		store, err := Open(filepath.Join(r.t.TempDir(), "history.sqlite"))
		if err != nil {
			return TranscriptProjectionResult{}, err
		}
		r.t.Cleanup(func() { store.Close() })
		prior = &transcriptFixtureProjection{store: store, source: testSource(root, "fixture.jsonl", native, ThreadMain, nil), input: input}
	}
	prior.source.Source.Size = parsed.Transcript.ReadOffset
	prior.source.IndexedAt = time.Unix(200, 0)
	applied, err := prior.store.ApplySource(ctx, prior.source)
	if err != nil {
		return TranscriptProjectionResult{}, err
	}
	prior.threadID = applied.ThreadID
	stored, exists, err := prior.store.LoadSource(ctx, mustRootID(r.t, prior.store, prior.source.Source.Root), prior.source.Source.Path)
	if err != nil || !exists {
		return TranscriptProjectionResult{}, fmt.Errorf("fixture source unavailable: %v", err)
	}
	mode, expected := TranscriptRebuild, ""
	if checkpoint != nil {
		mode, expected = TranscriptAppend, checkpoint.ProjectionKey
	}
	_, err = prior.store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: mode, ThreadID: applied.ThreadID,
		SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: r.provider, ParserVersion: parsed.ParserVersion,
		CompletionRuleVersion: parsed.CompletionRuleVersion, ExpectedProjectionKey: expected, Result: parsed.Transcript})
	if err != nil {
		return TranscriptProjectionResult{}, err
	}
	cp, exists, err := prior.store.LoadTranscriptCheckpoint(ctx, applied.ThreadID)
	if err != nil || !exists {
		return TranscriptProjectionResult{}, fmt.Errorf("fixture checkpoint unavailable: %v", err)
	}
	prior.checkpoint = cp
	r.saved[cp.ProjectionKey] = prior
	result := parsed.Transcript
	result.parsedBatches = nil
	tx, err := prior.store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var projection int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_projections WHERE projection_key=?`, cp.ProjectionKey).Scan(&projection); err != nil {
		return result, err
	}
	items, _, err := readTranscriptItemRange(ctx, tx, projection, 0, math.MaxInt64, nil)
	if err != nil {
		return result, err
	}
	for _, item := range items {
		if item.ProjectedSeq <= req.PriorProjectedSeq {
			result.ItemPatches = append(result.ItemPatches, TranscriptItemPatch{ItemID: item.ID, NativeItemID: item.NativeItemID, Phase: item.Phase, NativeFinal: item.IsFinalAnswer})
			continue
		}
		err := tx.QueryRowContext(ctx, `SELECT source_key,source_line,COALESCE(source_row_type,''),COALESCE(source_payload_type,''),source_line_sha256,emission_index FROM conversation_items WHERE projection_id=? AND item_id=?`, projection, item.ID).Scan(&item.SourceKey, &item.SourceLine, &item.SourceRowType, &item.SourcePayloadType, &item.SourceLineSHA256, &item.EmissionIndex)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, item)
	}
	// Read the stored view, including fields retained by later partial updates.
	// Usage-only turns belong in this test DTO even when the UI hides them.
	rows, err := tx.QueryContext(ctx, `SELECT turn_key, native_turn_id, ordinal, start_seq, end_seq, completion_seq,
status, completion_evidence, final_item_id, finality_source, finality_confidence, started_at, completed_at,
duration_ms, config_json, usage_input, usage_cached, usage_output, usage_reasoning, usage_total, context_window,
usage_cache_read, usage_cache_write, usage_native_json, usage_contract,
COALESCE(usage_model,''), COALESCE(usage_effort,''), COALESCE(usage_service_tier,'')
FROM conversation_turns WHERE projection_id=? AND parsed_json!='{}' ORDER BY ordinal`, projection)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var turn TranscriptTurn
		var native, final, finalSource, confidence, started, completed sql.NullString
		var completion, duration sql.NullInt64
		var input, cached, output, reasoning, total, window, cacheRead, cacheWrite, contract sql.NullInt64
		var config, usageNative []byte
		err := rows.Scan(&turn.Key, &native, &turn.Ordinal, &turn.StartSeq, &turn.EndSeq, &completion,
			&turn.Status, &turn.CompletionEvidence, &final, &finalSource, &confidence, &started, &completed,
			&duration, &config, &input, &cached, &output, &reasoning, &total, &window,
			&cacheRead, &cacheWrite, &usageNative, &contract, &turn.Model, &turn.Effort, &turn.ServiceTier)
		if err != nil {
			rows.Close()
			return result, err
		}
		turn.NativeTurnID, turn.FinalItemID = stringFromNull(native), stringFromNull(final)
		turn.FinalitySource, turn.FinalityConfidence = stringFromNull(finalSource), stringFromNull(confidence)
		turn.CompletionSeq, turn.DurationMS = int64FromNull(completion), int64FromNull(duration)
		turn.Config = append(json.RawMessage(nil), config...)
		turn.Usage = scanTurnUsage(input, cached, output, reasoning, total, window, cacheRead, cacheWrite, usageNative, contract)
		if turn.StartedAt, err = parseNullTime(started); err != nil {
			rows.Close()
			return result, err
		}
		if turn.CompletedAt, err = parseNullTime(completed); err != nil {
			rows.Close()
			return result, err
		}
		result.Turns = append(result.Turns, turn)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	for _, item := range result.Items {
		if item.ContentRef == "" {
			continue
		}
		body, err := prior.store.LoadTranscriptBody(ctx, applied.ThreadID, item.ID)
		if err != nil {
			return result, err
		}
		result.Bodies = append(result.Bodies, TranscriptBody{ItemID: item.ID, ContentRef: body.ContentRef, Text: body.Text,
			JSON: body.JSON, OriginalBytes: body.OriginalBytes, StoredBytes: body.StoredBytes, Truncated: body.Truncated})
	}
	return result, nil
}

// TranscriptProjectionRequest starts at a transcript checkpoint, which is
// deliberately unrelated to the metadata checkpoint on source_files.
type TranscriptProjectionRequest struct {
	Source SourceCandidate
	Reader io.Reader
	// NativeThreadID is the provider's stable conversation identity. It is
	// deliberately supplied by the catalog after metadata reconciliation rather
	// than inferred from a projection generation or source path.
	NativeThreadID string
	// ProjectionKey is empty for a rebuild (the adapter mints it) and is the
	// committed key for an append. It namespaces all opaque item/turn IDs.
	ProjectionKey      string
	StartOffset        int64
	PriorCompleteLines int
	PriorProjectedSeq  int64
	PreviousState      json.RawMessage
	MaxLineBytes       int64
}

// The detailed diagnostic store is independent of the framing summary.
func (r *transcriptFixtureReader) nativeDiagnostics(key string) []model.Diagnostic {
	r.t.Helper()
	rows, err := r.saved[key].store.db.Query(`SELECT diagnostic_json FROM conversation_parse_diagnostics ORDER BY id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var diagnostics []model.Diagnostic
	for rows.Next() {
		var raw []byte
		var diagnostic model.Diagnostic
		if err := rows.Scan(&raw); err != nil {
			r.t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &diagnostic); err != nil {
			r.t.Fatal(err)
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return diagnostics
}
