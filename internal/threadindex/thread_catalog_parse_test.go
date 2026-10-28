package threadindex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func TestThreadCatalogPreparedFileAppliesAfterSourceCloses(t *testing.T) {
	ctx := context.Background()
	root := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}
	path := filepath.Join(root.Path, "11111111-1111-4111-8111-111111111111.jsonl")
	raw := `{"type":"user","uuid":"event","sessionId":"native","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"Prepared once"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
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
	discovery, err := history.DiscoverRoot(ctx, root)
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatal("discover test source", err)
	}
	service := &Service{store: store, hub: newCatalogHub(), transcriptChanges: make(map[string]*pendingTranscriptChange)}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	input, prepared, err := service.projectSource(ctx, discovery.Candidates[0], history.StoredSource{}, false, history.TranscriptCheckpoint{}, false, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.File.Close()
	if input.ParserVersion != history.CurrentTranscriptParserVersion(root.Provider) {
		t.Fatal("catalog did not use the module version")
	}
	if _, err := store.ReconcileRoot(ctx, history.RootReconciliation{RootID: persisted.ID, ObservedPaths: []string{path}, Sources: []history.SourceProjection{input}, State: history.IndexReady, CompletedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	stored, exists, err := store.LoadSource(ctx, persisted.ID, path)
	if err != nil || !exists {
		t.Fatal("reconciled source missing", err)
	}
	checkpoint, exists, err := store.LoadTranscriptCheckpointHeader(ctx, stored.ThreadID)
	if err != nil || exists {
		t.Fatal("unexpected first checkpoint", err)
	}
	if err := os.Rename(path, path+".archived"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.applyPreparedTranscript(ctx, stored, checkpoint, false, prepared, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadTranscript(ctx, history.TranscriptReadRequest{ThreadID: stored.ThreadID})
	if err != nil || len(page.Items) != 1 || page.Items[0].Text != "Prepared once" {
		t.Fatalf("prepared source view: items=%d err=%v", len(page.Items), err)
	}
	repair, err := service.loadTranscriptRepairState(ctx, persisted.ID, catalogScanRecovery)
	if err != nil {
		t.Fatal(err)
	}
	header, exists, err := repair.checkpoint(ctx, stored.ThreadID)
	if err != nil || !exists || len(header.ParserState) != 0 || header.ReadOffset != int64(len(raw)) {
		t.Fatalf("repair loaded a body or lost progress: exists=%t stateBytes=%d read=%d err=%v", exists, len(header.ParserState), header.ReadOffset, err)
	}
	full, exists, err := store.LoadTranscriptCheckpoint(ctx, stored.ThreadID)
	var state struct {
		Version string          `json:"version"`
		Reader  json.RawMessage `json:"reader"`
	}
	if err != nil || !exists || json.Unmarshal(full.ParserState, &state) != nil || state.Version == "" || len(state.Reader) == 0 {
		t.Fatal("module continuation was not persisted", err)
	}
	// A prepared first writer cannot replace a projection committed meanwhile.
	if _, err := service.applyPreparedTranscript(ctx, stored, header, true, prepared, time.Now().UTC()); !errors.Is(err, history.ErrTranscriptChanged) {
		t.Fatalf("stale first writer accepted: %v", err)
	}
}

func TestThreadCatalogParsedAppendUsesReadBoundaryAndAllGuards(t *testing.T) {
	provider := history.ProviderClaude
	parser, completion, err := history.ParsedFileVersions(provider)
	if err != nil {
		t.Fatal(err)
	}
	source := history.SourceCandidate{Root: history.SourceRoot{Provider: provider}, Path: "/fixture/source.jsonl", Size: 120}
	stored := history.StoredSource{ID: 10, ThreadID: "thread", Generation: 3, Source: source}
	checkpoint := history.TranscriptCheckpoint{ProjectionKey: "projection", ThreadID: stored.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: provider, ParserVersion: parser, CompletionRuleVersion: completion, CompleteOffset: 90, ReadOffset: 100}
	if !canAppendParsedFile(checkpoint, stored, source) {
		t.Fatal("valid pending-tail checkpoint rejected")
	}
	for _, mutate := range []func(*history.TranscriptCheckpoint){
		func(cp *history.TranscriptCheckpoint) { cp.ReadOffset = 121 },
		func(cp *history.TranscriptCheckpoint) { cp.ReadOffset = 89 },
		func(cp *history.TranscriptCheckpoint) { cp.SourceGeneration++ },
		func(cp *history.TranscriptCheckpoint) { cp.SourceID++ },
		func(cp *history.TranscriptCheckpoint) { cp.ThreadID = "other" },
		func(cp *history.TranscriptCheckpoint) { cp.ParserVersion = "old" },
		func(cp *history.TranscriptCheckpoint) { cp.CompletionRuleVersion = "old" },
		func(cp *history.TranscriptCheckpoint) { text := "failed"; cp.ErrorSummary = &text },
	} {
		changed := checkpoint
		mutate(&changed)
		if canAppendParsedFile(changed, stored, source) {
			t.Fatal("invalid checkpoint accepted for append")
		}
	}
}
