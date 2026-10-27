package history

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedFileSpoolBoundsRepeatedTurnUsageSnapshots(t *testing.T) {
	var native strings.Builder
	native.WriteString(`{"type":"user","sessionId":"native","message":{"content":"Prompt"}}` + "\n")
	for i := 0; i < 256; i++ {
		fmt.Fprintf(&native, `{"type":"assistant","sessionId":"native","uuid":"event-%d","message":{"id":"response-%d","content":[{"type":"text","text":"Answer"}],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", i, i)
	}
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	source := testSource(root, "thread.jsonl", "native", ThreadMain, nil)
	parsed, err := ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source.Source, Reader: strings.NewReader(native.String()), SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	compressed, err := gzip.NewReader(io.NewSectionReader(parsed.spool, 0, parsed.spoolSize))
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	// This fixture's native data and evidence are small. Repeating each
	// growing turn summary made its serialization quadratic in responses.
	limit := int64(native.Len()) * 128
	encoded, err := io.Copy(io.Discard, io.LimitReader(compressed, limit+1))
	if err != nil || encoded > limit {
		t.Fatalf("repeated turn snapshots inflated spool: native=%d encoded=%d limit=%d err=%v", native.Len(), encoded, limit, err)
	}
	items, observations := make(map[string]bool), make(map[string]bool)
	var final model.Turn
	if err := parsed.visit(func(batch model.Batch) error {
		for _, obs := range batch.Observations {
			observations[obs.ID] = true
		}
		for _, change := range batch.Changes {
			if change.Item != nil {
				items[change.Item.ID] = true
			}
			if change.Turn != nil {
				final = *change.Turn
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(items) != 257 || len(observations) < 257 || len(final.UsageObservations) != 256 || final.Usage == nil || final.Usage.Input == nil || *final.Usage.Input != 256 {
		t.Fatalf("bounded spool lost evidence: items=%d observations=%d usageSamples=%d", len(items), len(observations), len(final.UsageObservations))
	}
	t.Logf("native=%d encoded=%d; 257 items and 256 usage samples retained", native.Len(), encoded)
}

func TestParsedFilePartialTailRestoresThroughStore(t *testing.T) {
	ctx := context.Background()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	source := testSource(root, "thread.jsonl", "native", ThreadMain, nil)
	raw := []byte(`{"type":"user","uuid":"prompt","sessionId":"native","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"A prompt"}}` + "\n" + `{"type":"assistant","uuid":"answer","sessionId":"native","timestamp":"2026-01-01T00:01:00Z","message":{"id":"message","role":"assistant","content":[{"type":"text","text":"An answer"}],"stop_reason":"end_turn"}}` + "\n")
	boundary := bytes.IndexByte(raw, '\n') + 1
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var checkpoint *TranscriptCheckpoint
	var threadID string
	start := 0
	for _, end := range []int{boundary + 8, boundary + 10, len(raw)} {
		input := &countedParsedReader{Reader: bytes.NewReader(raw[start:end])}
		source.Source.Size = int64(end)
		result, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source.Source, Reader: input, Checkpoint: checkpoint, SpoolDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		source.Result, source.ParserVersion = &result.Metadata, result.ParserVersion
		applied, err := store.ApplySource(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		threadID = applied.ThreadID
		stored, exists, err := store.LoadSource(ctx, mustRootID(t, store, root), source.Source.Path)
		if err != nil || !exists {
			t.Fatal("source did not reconcile", err)
		}
		mode, expected := TranscriptRebuild, ""
		if checkpoint != nil {
			mode, expected = TranscriptAppend, checkpoint.ProjectionKey
		}
		_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: mode, ThreadID: threadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: root.Provider, ParserVersion: result.ParserVersion, CompletionRuleVersion: result.CompletionRuleVersion, ExpectedProjectionKey: expected, Result: result.Transcript})
		if err != nil {
			t.Fatal(err)
		}
		if input.bytes != end-start {
			t.Fatalf("read %d bytes, want exactly %d", input.bytes, end-start)
		}
		cp, exists, err := store.LoadTranscriptCheckpoint(ctx, threadID)
		if err != nil || !exists || cp.ReadOffset != int64(end) {
			t.Fatalf("read progress: %+v %v", cp, err)
		}
		var storedState, projectedState any
		if err := json.Unmarshal(cp.ParserState, &storedState); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(result.Transcript.ParserState, &projectedState); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(storedState, projectedState) {
			t.Fatal("storage changed provider continuation state")
		}
		checkpoints, err := store.LoadTranscriptCheckpointsByRoot(ctx, mustRootID(t, store, root))
		if err != nil || !bytes.Equal(checkpoints[threadID].ParserState, cp.ParserState) {
			t.Fatalf("batch checkpoint load differs from individual load: %v", err)
		}
		wantLines, wantOffset := 1, boundary
		if end == len(raw) {
			wantLines, wantOffset = 2, len(raw)
		}
		if cp.CompleteLines != wantLines || cp.CompleteOffset != int64(wantOffset) {
			t.Fatalf("completed boundary: %+v", cp)
		}
		old, err := catalogFixtureOracle(t, ProviderClaude, ProjectionRequest{Source: source.Source, Reader: bytes.NewReader(raw[:end])})
		if err != nil || !reflect.DeepEqual(result.Metadata.Projection, old.Projection) {
			t.Fatalf("append metadata differs from complete prefix: %v %v", catalogFieldDifferences(old.Projection, result.Metadata.Projection), err)
		}
		checkpoint = &cp
		if err := result.Close(); err != nil {
			t.Fatal(err)
		}
		if result.spool != nil {
			t.Fatal("closed spool remains open")
		}
		if err := result.Transcript.parsedBatches(func(model.Batch) error { t.Fatal("closed spool emitted"); return nil }); err == nil {
			t.Fatal("closed spool silently accepted")
		}
		start = end
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: threadID})
	if err != nil || len(page.Items) != 2 || page.Items[0].Text != "A prompt" || page.Items[1].Text != "An answer" {
		t.Fatalf("restored transcript: %+v %v", page, err)
	}
	var line int
	if err := store.db.QueryRow(`SELECT source_line FROM conversation_items WHERE item_id=?`, page.Items[1].ID).Scan(&line); err != nil || line != 2 {
		t.Fatalf("stored source line=%d err=%v", line, err)
	}
}

type countedParsedReader struct {
	io.Reader
	bytes int
}

func (r *countedParsedReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func TestParsedFileRejectsMismatchedContinuationBeforeReading(t *testing.T) {
	ctx := context.Background()
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/fixture/rollout.jsonl"}
	result, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: strings.NewReader(`{"type":`), SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	base := TranscriptCheckpoint{Provider: source.Root.Provider, ProjectionKey: result.Transcript.ProjectionKey, ParserVersion: result.ParserVersion, CompletionRuleVersion: result.CompletionRuleVersion, ReadOffset: result.Transcript.ReadOffset, ParserState: result.Transcript.ParserState}
	for _, mutate := range []func(*TranscriptCheckpoint){
		func(cp *TranscriptCheckpoint) { cp.ReadOffset-- },
		func(cp *TranscriptCheckpoint) { cp.CompleteOffset++ },
		func(cp *TranscriptCheckpoint) { cp.CompleteLines++ },
		func(cp *TranscriptCheckpoint) { cp.ProjectionKey = "other" },
		func(cp *TranscriptCheckpoint) { cp.ParserVersion = "old" },
		func(cp *TranscriptCheckpoint) { cp.Provider = ProviderClaude },
		func(cp *TranscriptCheckpoint) { cp.ParserState = []byte(`{}`) },
		func(cp *TranscriptCheckpoint) { cp.ParserState = []byte(`broken`) },
		func(cp *TranscriptCheckpoint) {
			var saved parsedFileCheckpoint
			_ = json.Unmarshal(cp.ParserState, &saved)
			saved.Reader = []byte(`{}`)
			cp.ParserState, _ = json.Marshal(saved)
		},
		func(cp *TranscriptCheckpoint) {
			var saved parsedFileCheckpoint
			_ = json.Unmarshal(cp.ParserState, &saved)
			var reader map[string]json.RawMessage
			_ = json.Unmarshal(saved.Reader, &reader)
			reader["version"] = []byte(`-1`)
			saved.Reader, _ = json.Marshal(reader)
			cp.ParserState, _ = json.Marshal(saved)
		},
	} {
		cp := base
		mutate(&cp)
		reader := &countedParsedReader{Reader: strings.NewReader("never read")}
		_, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: reader, Checkpoint: &cp, SpoolDir: t.TempDir()})
		if !errors.Is(err, ErrTranscriptChanged) || reader.bytes != 0 {
			t.Fatalf("bad checkpoint read native input: bytes=%d err=%v", reader.bytes, err)
		}
	}
}

func TestParsedFileSpoolSurvivesSourceClosureAndRollsBackReadFailure(t *testing.T) {
	ctx := context.Background()
	raw, err := os.ReadFile("../transcriptparse/codex/testdata/catalog.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	native := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(native, raw, 0600); err != nil {
		t.Fatal(err)
	}
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: native}
	file, err := os.Open(native)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: file, SpoolDir: t.TempDir()})
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if err := os.Rename(native, native+".archived"); err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := 0; i < 2; i++ {
		var buffer bytes.Buffer
		records, items := 0, 0
		err := result.Transcript.parsedBatches(func(batch model.Batch) error {
			records += len(batch.Records)
			for _, change := range batch.Changes {
				if change.Item != nil {
					items++
				}
			}
			return json.NewEncoder(&buffer).Encode(batch)
		})
		if err != nil || records == 0 || items == 0 {
			t.Fatalf("independent spool: records=%d items=%d %v", records, items, err)
		}
		if i == 0 {
			first = append([]byte(nil), buffer.Bytes()...)
		} else if !bytes.Equal(first, buffer.Bytes()) {
			t.Fatal("spool retry changed batches")
		}
	}
	failure := errors.New("native read failure")
	spoolDir := t.TempDir()
	_, err = ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: io.MultiReader(bytes.NewReader(raw), parsedFailureReader{failure}), SpoolDir: spoolDir})
	if !errors.Is(err, failure) {
		t.Fatalf("lost native read error: %v", err)
	}
	entries, err := os.ReadDir(spoolDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed parse left spool", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = ProjectParsedFile(canceled, ParsedFileRequest{Source: source, Reader: bytes.NewReader(raw), SpoolDir: spoolDir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	entries, err = os.ReadDir(spoolDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("canceled parse left spool", err)
	}
}

type parsedFailureReader struct{ err error }

func (r parsedFailureReader) Read([]byte) (int, error) { return 0, r.err }

func TestParsedFileGrokSeparateCatalogAndDocumentClosure(t *testing.T) {
	ctx := context.Background()
	updates, err := os.ReadFile("../transcriptparse/grok/testdata/late-replay-after-eviction.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	summary := []byte(`{"info":{"id":"session","cwd":"/workspace"},"updated_at":"2026-01-01T00:00:00Z","generated_title":"Catalog title"}`)
	source := SourceCandidate{Root: SourceRoot{Provider: ProviderGrok}, Path: "/fixture/session/updates.jsonl"}
	catalogSource := source
	catalogSource.Path = "/fixture/session/summary.json"
	result, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: source, Reader: bytes.NewReader(updates), Catalog: &ParsedCatalogInput{Source: catalogSource, Reader: bytes.NewReader(summary), Format: "json"}, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	old, err := catalogFixtureOracle(t, ProviderGrok, ProjectionRequest{Source: catalogSource, Reader: bytes.NewReader(summary)})
	if err != nil || !reflect.DeepEqual(result.Metadata.Projection, old.Projection) {
		t.Fatalf("summary catalog differs: %v %v", catalogFieldDifferences(old.Projection, result.Metadata.Projection), err)
	}
	scopes := map[string]bool{}
	items := 0
	err = result.Transcript.parsedBatches(func(batch model.Batch) error {
		for _, obs := range batch.Observations {
			scopes[obs.Source.Label] = true
		}
		for _, change := range batch.Changes {
			if change.Item != nil {
				items++
			}
		}
		return nil
	})
	if err != nil || !scopes[source.Path] || !scopes[catalogSource.Path] || items == 0 {
		t.Fatalf("separate inputs lost: %v items=%d err=%v", scopes, items, err)
	}
	doc, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: catalogSource, Reader: bytes.NewReader(summary), Format: "json", SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if doc.Transcript.CompleteOffset != int64(len(summary)) || doc.Transcript.ReadOffset != int64(len(summary)) || doc.Transcript.CompleteLines != 1 {
		t.Fatal("document was not closed at EOF")
	}
	if !reflect.DeepEqual(doc.Metadata.Projection, old.Projection) {
		t.Fatal("standalone document differs from summary")
	}
}

func TestParsedFileKeepsDiagnosticsAndEmptySourceContinuation(t *testing.T) {
	for _, provider := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok} {
		t.Run(string(provider), func(t *testing.T) {
			source := SourceCandidate{Root: SourceRoot{Provider: provider}, Path: "/fixture/empty.jsonl"}
			result, err := ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source, Reader: strings.NewReader("invalid\n"), SpoolDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer result.Close()
			if result.Metadata.Summary.Malformed != 1 || result.Transcript.Summary.Malformed != 1 {
				t.Fatal("malformed source was lost from diagnostic summaries")
			}
			empty, err := ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source, Reader: strings.NewReader(""), SpoolDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer empty.Close()
			if empty.Transcript.ReadOffset != 0 || empty.Transcript.CompleteLines != 0 || len(empty.Transcript.ParserState) == 0 {
				t.Fatal("empty source has no valid continuation")
			}
			checkpoint := TranscriptCheckpoint{Provider: provider, ProjectionKey: empty.Transcript.ProjectionKey, ParserVersion: empty.ParserVersion, CompletionRuleVersion: empty.CompletionRuleVersion, ParserState: empty.Transcript.ParserState}
			resumed, err := ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source, Reader: strings.NewReader("\n"), Checkpoint: &checkpoint, SpoolDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			if resumed.Transcript.ReadOffset != 1 || resumed.Transcript.CompleteLines != 1 {
				t.Fatal("empty-source append lost its first line")
			}
		})
	}
}

func TestParsedFileDevRowsRemainSeparateFromModelItems(t *testing.T) {
	ctx := context.Background()
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	metadata := testSource(root, "thread.jsonl", "native", ThreadMain, nil)
	raw := `{"type":"user","uuid":"event","sessionId":"native","message":{"role":"user","content":"Visible message"}}` + "\n" + `{"type":"custom-title","customTitle":"Catalog title"}` + "\ninvalid\n"
	parsed, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: metadata.Source, Reader: strings.NewReader(raw), SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	modelItems := map[string]bool{}
	if err := parsed.Transcript.parsedBatches(func(batch model.Batch) error {
		for _, change := range batch.Changes {
			if change.Item != nil {
				modelItems[change.Item.ID] = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(modelItems) != 1 {
		t.Fatal("reader invented model items for source-only records")
	}
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	metadata.Result, metadata.ParserVersion = &parsed.Metadata, parsed.ParserVersion
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: root.Provider, ParserVersion: parsed.ParserVersion, CompletionRuleVersion: parsed.CompletionRuleVersion, ExpectNoProjection: true, Result: parsed.Transcript})
	if err != nil {
		t.Fatal(err)
	}
	var links, models int
	if err := store.db.QueryRow(`SELECT SUM(CASE WHEN parsed_json='{}' THEN 1 ELSE 0 END),SUM(CASE WHEN parsed_json!='{}' THEN 1 ELSE 0 END) FROM conversation_items`).Scan(&links, &models); err != nil || links != 2 || models != 1 {
		t.Fatalf("history links=%d model items=%d err=%v", links, models, err)
	}
	page, err := store.ReadTranscript(ctx, TranscriptReadRequest{ThreadID: applied.ThreadID})
	if err != nil || len(page.Items) != 3 || page.Items[0].Text != "Visible message" || page.Items[2].Category != TranscriptCategory("unclassified") {
		t.Fatalf("Dev source order/items=%d err=%v", len(page.Items), err)
	}
}
