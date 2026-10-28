package threadindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// claudeScopeFixture projects Claude files under one project directory and
// records every source and transcript at the reader version before v34, as
// the live store held 6,015 of them.
type claudeScopeFixture struct {
	ctx       context.Context
	store     *history.Store
	service   *Service
	root      history.SourceRoot
	project   string
	threadIDs map[string]string
	stale     string
	current   string
	logMu     sync.Mutex
	logged    []string
}

func newClaudeScopeFixture(t *testing.T, write func(project string) int) *claudeScopeFixture {
	t.Helper()
	f := &claudeScopeFixture{ctx: context.Background(), threadIDs: map[string]string{}}
	rootPath := filepath.Join(t.TempDir(), "projects")
	f.project = filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(f.project, 0o700); err != nil {
		t.Fatal(err)
	}
	total := write(f.project)
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	f.root = history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}
	service, err := newTestCatalogService(f.ctx, store, Options{
		Roots: []history.SourceRoot{f.root}, DisableWatcher: true,
		Logf: func(format string, args ...any) {
			f.logMu.Lock()
			f.logged = append(f.logged, fmt.Sprintf(format, args...))
			f.logMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	f.service = service
	service.RequestRefresh()
	waitForCatalogWithin(t, 30*time.Second, func() bool {
		page, err := service.ListThreads(f.ctx, ListQuery{Archived: "include", Limit: total + 10})
		if err != nil || len(page.Items) != total {
			return false
		}
		for _, item := range page.Items {
			if _, exists, err := store.LoadTranscriptCheckpoint(f.ctx, item.ID); err != nil || !exists {
				return false
			}
			f.threadIDs[item.NativeID] = item.ID
		}
		return true
	})
	persisted, err := store.EnsureRoot(f.ctx, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.current = history.CurrentTranscriptParserVersion(history.ProviderClaude)
	f.stale = strings.Replace(f.current, "claude-reader-v36", "claude-reader-v35", 1)
	if f.stale == f.current {
		t.Fatalf("current Claude reader %q is no longer v36; the marker rule under test has moved on", f.current)
	}
	entries, err := os.ReadDir(f.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(f.project, entry.Name())
		stored, ok, err := store.LoadSource(f.ctx, persisted.ID, path)
		if err != nil || !ok {
			t.Fatalf("stored source = %v, %v", ok, err)
		}
		threadID := f.threadIDs[strings.TrimSuffix(entry.Name(), ".jsonl")]
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := history.ProjectParsedFile(f.ctx, history.ParsedFileRequest{Source: stored.Source, Reader: file, SpoolDir: t.TempDir()})
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		metadata := parsed.Metadata
		if _, err := store.ApplySource(f.ctx, history.SourceProjection{Source: stored.Source, Result: &metadata, ParserVersion: f.stale, Status: history.SourcePresent, IndexedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		stored, _, _ = store.LoadSource(f.ctx, persisted.ID, path)
		checkpoint, _, err := store.LoadTranscriptCheckpoint(f.ctx, threadID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApplyTranscript(f.ctx, history.TranscriptApplyRequest{
			Mode: history.TranscriptRebuild, ThreadID: threadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
			Provider: history.ProviderClaude, ParserVersion: f.stale, CompletionRuleVersion: parsed.CompletionRuleVersion,
			ExpectedProjectionKey: checkpoint.ProjectionKey, Result: parsed.Transcript,
		}); err != nil {
			t.Fatal(err)
		}
		parsed.Close()
	}
	if candidates, err := store.ListTranscriptParserBackfillCandidates(f.ctx, total+10); err != nil || len(candidates) != total {
		t.Fatalf("fixture candidates = %d, %v; want %d behind", len(candidates), err, total)
	}
	return f
}

func (f *claudeScopeFixture) version(t *testing.T, nativeID string) string {
	t.Helper()
	checkpoint, exists, err := f.store.LoadTranscriptCheckpoint(f.ctx, f.threadIDs[nativeID])
	if err != nil || !exists {
		t.Fatalf("checkpoint = %v, %v", exists, err)
	}
	return checkpoint.ParserVersion
}

func (f *claudeScopeFixture) candidates(t *testing.T) int {
	t.Helper()
	candidates, err := f.store.ListTranscriptParserBackfillCandidates(f.ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(candidates)
}

// Reader v34 (merged Claude prompts) queued every saved Claude source for a
// re-read, about a minute each and four days in all, though only sources
// carrying mergedSendUuids could change. An upgrade that names its marker
// re-reads only the sources that carry it; the rest are marked current from
// one pass over their bytes. A source marked current keeps its older
// projection, which must still answer saved-prompt reads: refusing every
// projection an older reader built left each unaffected Claude thread with
// no saved answers until its file next changed. The marked source carries a
// captured row the current upgrade changes (a sent slash command, as Claude
// writes it, spacing included). Boundary: runTranscriptParserBackfillBatch
// and ReadSavedUserRows on a store holding two Claude sources one reader
// behind.
func TestParserBackfillMarksUnaffectedSourcesWithoutRereading(t *testing.T) {
	plainID, markedID := "77777777-7777-4777-8777-777777777771", "77777777-7777-4777-8777-777777777772"
	f := newClaudeScopeFixture(t, func(project string) int {
		plain := fmt.Sprintf("{\"type\":\"user\",\"uuid\":\"u-plain\",\"promptId\":\"p-plain\",\"sessionId\":%[1]q,\"timestamp\":\"2026-10-19T09:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"Plain prompt\"}}\n"+
			"{\"type\":\"assistant\",\"uuid\":\"a-plain\",\"sessionId\":%[1]q,\"timestamp\":\"2026-10-19T09:00:01Z\",\"message\":{\"id\":\"m-plain\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[{\"type\":\"text\",\"text\":\"Plain answer\"}],\"stop_reason\":\"end_turn\"}}\n", plainID)
		if err := os.WriteFile(filepath.Join(project, plainID+".jsonl"), []byte(plain), 0o600); err != nil {
			t.Fatal(err)
		}
		markedPath := filepath.Join(project, markedID+".jsonl")
		writeClaudeCatalogFixture(t, markedPath, markedID, "2026-10-19T09:00:00Z", "First prompt")
		var marked string
		for _, line := range scopeFixtureLines(t) {
			if strings.Contains(string(line), "<command-name>") {
				marked = strings.ReplaceAll(string(line), "94f881e3-d06d-5b44-814c-194b17d7aada", markedID) + "\n"
			}
		}
		file, err := os.OpenFile(markedPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(marked); err != nil {
			t.Fatal(err)
		}
		file.Close()
		return 2
	})
	plainPrompt := history.SavedUserKeys{NativeEventIDs: []string{"u-plain"}}
	if _, err := f.store.ReadSavedUserRows(f.ctx, f.threadIDs[plainID], plainPrompt); !errors.Is(err, history.ErrSavedReaderOutdated) {
		t.Fatalf("saved read of an older reader's projection before any check = %v; want outdated", err)
	}
	inspected, _, err := f.service.runTranscriptParserBackfillBatch(f.ctx, 2)
	if err != nil || inspected != 2 {
		t.Fatalf("batch inspected = %d, %v; want 2", inspected, err)
	}
	if got := f.version(t, plainID); got != f.stale {
		t.Fatalf("the source without the marker was re-read: parser version %q", got)
	}
	if got := f.version(t, markedID); got != f.current {
		t.Fatalf("the source with the marker was not re-read: parser version %q", got)
	}
	if got := f.candidates(t); got != 0 {
		t.Fatalf("candidates after the batch = %d; want none (the unmarked source is current without a re-read)", got)
	}
	if rows, err := f.store.ReadSavedUserRows(f.ctx, f.threadIDs[plainID], plainPrompt); err != nil || len(rows) != 1 {
		t.Fatalf("saved read of the source proved unaffected = %d rows, %v; want its prompt", len(rows), err)
	}
	f.logMu.Lock()
	defer f.logMu.Unlock()
	found := false
	for _, line := range f.logged {
		if strings.Contains(line, "transcript parser backfill batch") && strings.Contains(line, "unaffected=1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("batch log did not report the unaffected source: %q", f.logged)
	}
}

// A batch reads a bounded number of candidate files to classify them. The
// ones it did not reach must wait, not fall through to a full re-read: with
// the bound at 64, a 65th marker-free source went to the ordinary rebuild,
// the very waste this rule prevents.
func TestParserBackfillLeavesUnscannedSourcesPending(t *testing.T) {
	total := unaffectedScanBatch + 2
	f := newClaudeScopeFixture(t, func(project string) int {
		for i := 0; i < total; i++ {
			id := fmt.Sprintf("66666666-6666-4666-8666-%012d", i)
			writeClaudeCatalogFixture(t, filepath.Join(project, id+".jsonl"), id, "2026-10-19T09:00:00Z", fmt.Sprintf("Prompt %d", i))
		}
		return total
	})
	inspected, _, err := f.service.runTranscriptParserBackfillBatch(f.ctx, 2)
	if err != nil || inspected != unaffectedScanBatch {
		t.Fatalf("first batch inspected = %d, %v; want %d marked and none re-read", inspected, err, unaffectedScanBatch)
	}
	for nativeID := range f.threadIDs {
		if got := f.version(t, nativeID); got != f.stale {
			t.Fatalf("source %s past the scan bound was re-read instead of waiting", nativeID)
		}
	}
	if got := f.candidates(t); got != 2 {
		t.Fatalf("candidates after one batch = %d; want the 2 unscanned sources", got)
	}
	if inspected, _, err := f.service.runTranscriptParserBackfillBatch(f.ctx, 2); err != nil || inspected != 2 {
		t.Fatalf("second batch inspected = %d, %v; want the remaining 2 marked", inspected, err)
	}
	if got := f.candidates(t); got != 0 {
		t.Fatalf("candidates after two batches = %d; want none", got)
	}
}

func scopeFixtureLines(t *testing.T) [][]byte {
	t.Helper()
	data, err := os.ReadFile("../app/testdata/live_text/claude-sent-command.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSpace(data), []byte("\n"))
}
