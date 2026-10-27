package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func TestCatalogKeepsProjectionOnModTimeOnlyTouch(t *testing.T) {
	ctx := context.Background()
	root := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}
	project := filepath.Join(root.Path, "-home-user-touch")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "5d5b8f1e-3f7a-4a58-9d1e-2b1c0e7f4a11.jsonl")
	body := `{"type":"user","sessionId":"5d5b8f1e-3f7a-4a58-9d1e-2b1c0e7f4a11","uuid":"u1","message":{"content":"Prompt"}}` + "\n" +
		`{"type":"assistant","sessionId":"5d5b8f1e-3f7a-4a58-9d1e-2b1c0e7f4a11","uuid":"a1","message":{"id":"m1","content":[{"type":"text","text":"Answer one"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	service := &threadCatalogService{store: store, hub: newCatalogHub(), transcriptChanges: make(map[string]*pendingTranscriptChange)}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	service.projectSourceFn = service.projectSource
	service.logf = func(string, ...any) {}
	refresh := func() (history.StoredSource, history.TranscriptCheckpoint) {
		t.Helper()
		if _, err := service.refreshRoot(ctx, persisted, catalogScanRecovery, nil, nil); err != nil {
			t.Fatal(err)
		}
		stored, exists, err := store.LoadSource(ctx, persisted.ID, path)
		if err != nil || !exists {
			t.Fatal("source absent", err)
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpointHeader(ctx, stored.ThreadID)
		if err != nil || !exists || checkpoint.ErrorSummary != nil {
			t.Fatalf("checkpoint: exists=%t error=%v err=%v", exists, checkpoint.ErrorSummary, err)
		}
		return stored, checkpoint
	}
	touch := func(at time.Time) {
		t.Helper()
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}

	_, first := refresh()
	touchedAt := time.Now().Add(time.Hour).Truncate(time.Second)
	touch(touchedAt)
	stored, afterTouch := refresh()
	if afterTouch.ProjectionKey != first.ProjectionKey {
		t.Fatalf("a modification-time touch rebuilt the thread: projection %s -> %s", first.ProjectionKey, afterTouch.ProjectionKey)
	}
	if afterTouch.CompleteLines != first.CompleteLines {
		t.Fatalf("touch changed parsed lines: %d -> %d", first.CompleteLines, afterTouch.CompleteLines)
	}
	if !stored.Source.ModTime.Equal(touchedAt) {
		t.Fatalf("stored modification time %s was not advanced to %s, so the touch would repeat", stored.Source.ModTime, touchedAt)
	}

	rewritten := strings.Replace(body, "Answer one", "Answer two", 1)
	if len(rewritten) != len(body) {
		t.Fatal("fixture rewrite must keep the size")
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(touchedAt.Add(time.Hour))
	_, afterRewrite := refresh()
	if afterRewrite.ProjectionKey == afterTouch.ProjectionKey {
		t.Fatal("a same-size in-place rewrite was treated as a harmless touch")
	}
}
