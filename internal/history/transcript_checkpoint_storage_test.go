package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestTranscriptCheckpointAppendWritesAreBounded(t *testing.T) {
	registerCountingDriver(t)
	measure := func(entries int) int64 {
		t.Helper()
		ctx := context.Background()
		store, err := openStore(countingDriverName, filepath.Join(t.TempDir(), "history.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: t.TempDir()}
		metadata := testSource(root, "thread.jsonl", fmt.Sprintf("native-%d", entries), ThreadMain, nil)
		applied, err := store.ApplySource(ctx, metadata)
		if err != nil {
			t.Fatal(err)
		}
		source, exists, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
		if err != nil || !exists {
			t.Fatalf("load source: exists=%v err=%v", exists, err)
		}
		initial := syntheticCheckpoint(entries)
		written, err := store.ApplyTranscript(ctx, TranscriptApplyRequest{
			Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID,
			SourceGeneration: source.Generation, Provider: ProviderCodex,
			ParserVersion: "bounded-write", CompletionRuleVersion: "bounded-write",
			Result: TranscriptProjectionResult{ProjectionKey: "checkpoint", CompleteOffset: 1, ReadOffset: 1, CompleteLines: 1, ParserState: initial},
		})
		if err != nil {
			t.Fatal(err)
		}
		atomic.StoreInt64(&stmtArgumentBytes, 0)
		_, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{
			Mode: TranscriptAppend, ThreadID: applied.ThreadID, SourceID: source.ID,
			SourceGeneration: source.Generation, Provider: ProviderCodex,
			ParserVersion: "bounded-write", CompletionRuleVersion: "bounded-write",
			ExpectedProjectionKey: written.ProjectionKey,
			Result:                TranscriptProjectionResult{ProjectionKey: written.ProjectionKey, CompleteOffset: 2, ReadOffset: 2, CompleteLines: 2, ParserState: syntheticCheckpoint(entries + 1)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return atomic.LoadInt64(&stmtArgumentBytes)
	}

	small := measure(8)
	large := measure(2048)
	t.Logf("one-entry append writes: 8-entry checkpoint=%d bytes, 2048-entry checkpoint=%d bytes", small, large)
	if large > small+32<<10 {
		t.Fatalf("one-entry append write grew with checkpoint: small=%d bytes large=%d bytes", small, large)
	}
}

func TestTranscriptCheckpointMigrationResumesOldShape(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "history.sqlite")
	store, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	root := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: t.TempDir()}
	metadata := testSource(root, "thread.jsonl", "native", ThreadMain, nil)
	initialRaw := `{"type":"user","uuid":"prompt","sessionId":"native","message":{"role":"user","content":"Prompt"}}` + "\n"
	appendRaw := `{"type":"assistant","uuid":"answer","sessionId":"native","message":{"id":"message","role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn"}}` + "\n"
	initial, err := ProjectParsedFile(ctx, ParsedFileRequest{Source: metadata.Source, Reader: strings.NewReader(initialRaw), SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Close()
	metadata.Result, metadata.ParserVersion = &initial.Metadata, initial.ParserVersion
	applied, err := store.ApplySource(ctx, metadata)
	if err != nil {
		t.Fatal(err)
	}
	source, exists, err := store.LoadSource(ctx, mustRootID(t, store, root), metadata.Source.Path)
	if err != nil || !exists {
		t.Fatalf("load source: exists=%v err=%v", exists, err)
	}
	if _, err = store.ApplyTranscript(ctx, TranscriptApplyRequest{Mode: TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: source.ID, SourceGeneration: source.Generation, Provider: ProviderClaude, ParserVersion: initial.ParserVersion, CompletionRuleVersion: initial.CompletionRuleVersion, Result: initial.Transcript}); err != nil {
		t.Fatal(err)
	}
	legacyState, err := encodeTranscriptContinuation(initial.Transcript.ParserState)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`UPDATE conversation_projections SET parser_state_json=?`, legacyState); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`DROP TABLE conversation_parser_state_entries`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`DELETE FROM schema_migrations WHERE version=24`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	wantCheckpoint := TranscriptCheckpoint{Provider: ProviderClaude, ProjectionKey: initial.Transcript.ProjectionKey, ParserVersion: initial.ParserVersion, CompletionRuleVersion: initial.CompletionRuleVersion, CompleteOffset: initial.Transcript.CompleteOffset, ReadOffset: initial.Transcript.ReadOffset, CompleteLines: initial.Transcript.CompleteLines, ParserState: initial.Transcript.ParserState}
	want := resumedParsedBytes(t, metadata.Source, appendRaw, wantCheckpoint)
	store, err = Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkpoint, exists, err := store.LoadTranscriptCheckpoint(ctx, applied.ThreadID)
	if err != nil || !exists {
		t.Fatalf("load migrated checkpoint: exists=%v err=%v", exists, err)
	}
	got := resumedParsedBytes(t, metadata.Source, appendRaw, checkpoint)
	if !bytes.Equal(got, want) {
		t.Fatalf("resumed parsed output changed across migration\nwant %s\n got %s", want, got)
	}
}

func syntheticCheckpoint(entries int) []byte {
	items := make(map[string]any, entries)
	for index := 0; index < entries; index++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("checkpoint-entry-%d", index)))
		items[fmt.Sprintf("item-%06d", index)] = map[string]string{"payload": strings.Repeat(hex.EncodeToString(seed[:]), 8)}
	}
	raw, _ := json.Marshal(map[string]any{
		"version": "history-file-v11", "provider": "codex", "source": "test", "projectionKey": "checkpoint", "format": "jsonl",
		"reader": map[string]any{"version": 22, "items": items}, "metadata": map[string]any{},
	})
	return raw
}

func resumedParsedBytes(t *testing.T, source SourceCandidate, appended string, checkpoint TranscriptCheckpoint) []byte {
	t.Helper()
	result, err := ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source, Reader: strings.NewReader(appended), Checkpoint: &checkpoint, SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	var batches []model.Batch
	if err := result.visit(func(batch model.Batch) error {
		batches = append(batches, batch)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Transcript TranscriptProjectionResult `json:"transcript"`
		Batches    []model.Batch              `json:"batches"`
	}{result.Transcript, batches})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
