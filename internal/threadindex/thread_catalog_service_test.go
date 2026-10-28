package threadindex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

func TestThreadCatalogServiceIndexesPagesAndRepairsMissingSources(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	firstID := "11111111-1111-4111-8111-111111111111"
	secondID := "22222222-2222-4222-8222-222222222222"
	firstPath := filepath.Join(projectPath, firstID+".jsonl")
	writeClaudeCatalogFixture(t, firstPath, firstID, "2026-07-10T10:00:00Z", "First catalog prompt")
	writeClaudeCatalogFixture(t, filepath.Join(projectPath, secondID+".jsonl"), secondID, "2026-07-10T11:00:00Z", "Second catalog prompt")

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs
	service.SetTranscriptChanged(func(context.Context, TranscriptChange) {})

	waitForCatalog(t, func() bool {
		status, err := service.IndexStatus(context.Background())
		return err == nil && status.State == "ready" && len(status.Roots) == 1 && status.Roots[0].SourceCount == 2 &&
			status.TranscriptChangeBacklog == 0 && status.TranscriptChangeInFlight == 0
	})
	if status, err := service.IndexStatus(context.Background()); err != nil || status.TranscriptChangeWorkers != transcriptChangeWorkerCount || status.TranscriptChangeBacklog != 0 || status.TranscriptChangeInFlight != 0 {
		t.Fatalf("idle transcript change status = %#v, err=%v", status, err)
	}

	firstPage, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 1 || firstPage.NextCursor == nil || firstPage.Items[0].NativeID != secondID {
		t.Fatalf("first page = %#v", firstPage)
	}
	secondPage, err := service.ListThreads(context.Background(), ListQuery{
		Archived: "include", Limit: 1, Cursor: *firstPage.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.Items[0].NativeID != firstID || secondPage.Items[0].ID == firstPage.Items[0].ID {
		t.Fatalf("second page = %#v", secondPage)
	}
	exactPage, err := service.ListThreads(context.Background(), ListQuery{
		ThreadID: firstID, Archived: "include", Limit: 10,
	})
	if err != nil || len(exactPage.Items) != 1 || exactPage.Items[0].NativeID != firstID || exactPage.NextCursor != nil {
		t.Fatalf("exact thread ID page = %#v, %v", exactPage, err)
	}

	detail, err := service.LoadThread(context.Background(), secondPage.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Preview == nil || *detail.Preview != "First catalog prompt" || detail.Sources == nil || len(*detail.Sources) != 1 {
		t.Fatalf("thread detail = %#v", detail)
	}
	if detail.Metadata.Priority.State != "unsupported" || detail.Metadata.Model.Value == nil || *detail.Metadata.Model.Value != "claude-test" {
		t.Fatalf("canonical metadata = %#v", detail.Metadata)
	}
	statusBefore, err := service.IndexStatus(context.Background())
	if err != nil || len(statusBefore.Roots) != 1 || statusBefore.Roots[0].LastCompletedAt == nil {
		t.Fatalf("index status before repair = %#v, %v", statusBefore, err)
	}
	completedBefore := *statusBefore.Roots[0].LastCompletedAt
	if !service.RequestRefresh() {
		t.Fatal("steady-state refresh request was not accepted")
	}
	waitForCatalog(t, func() bool {
		status, err := service.IndexStatus(context.Background())
		return err == nil && len(status.Roots) == 1 && status.Roots[0].LastCompletedAt != nil &&
			*status.Roots[0].LastCompletedAt != completedBefore
	})
	steadyPage, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if steadyPage.CatalogVersion != firstPage.CatalogVersion {
		t.Fatalf("unchanged repair advanced catalog version: before %d, after %d", firstPage.CatalogVersion, steadyPage.CatalogVersion)
	}

	// Read methods are store-only: removing a native file does not trigger a
	// filesystem scan until the background refresh is explicitly requested.
	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	beforeRepair, err := service.LoadThread(context.Background(), detail.ID)
	if err != nil || beforeRepair.Source.Status != "present" {
		t.Fatalf("read path unexpectedly scanned native roots: %#v, %v", beforeRepair, err)
	}
	priorVersion := steadyPage.CatalogVersion
	if !service.RequestRefresh() {
		t.Fatal("refresh request was not accepted")
	}
	waitForCatalog(t, func() bool {
		item, err := service.LoadThread(context.Background(), detail.ID)
		return err == nil && item.Source.Status == "source_missing"
	})
	afterRepair, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if afterRepair.CatalogVersion <= priorVersion {
		t.Fatalf("catalog version did not advance: before %d, after %d", priorVersion, afterRepair.CatalogVersion)
	}
}

func TestThreadCatalogServiceReprojectsUnchangedMetadataAfterParserVersionBump(t *testing.T) {
	ctx := context.Background()
	rootPath := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootPath, "rollout-2026-08-08T09-00-00-guardian-thread.jsonl")
	contents := `{"timestamp":"2026-08-08T09:00:00Z","type":"session_meta","payload":{"id":"guardian-thread","parent_thread_id":"parent-thread","source":{"subagent":{"other":"guardian"}}}}` + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}
	persistedRoot, err := store.EnsureRoot(ctx, rootSpec)
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := history.DiscoverRoot(ctx, rootSpec)
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatalf("discover source = %#v, %v", discovery.Candidates, err)
	}
	parsed, err := history.ProjectParsedFile(ctx, history.ParsedFileRequest{
		Source: discovery.Candidates[0], Reader: strings.NewReader(contents), SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	oldResult := parsed.Metadata
	// Simulate the last metadata parser's historical misclassification while
	// retaining the same source identity, size, and mtime.
	oldResult.Projection.Kind = history.ThreadSubagent
	if _, err := store.ApplySource(ctx, history.SourceProjection{
		Source: discovery.Candidates[0], Result: &oldResult, ParserVersion: "codex-metadata-v5",
		Status: history.SourcePresent, IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	service, err := newTestCatalogService(ctx, store, Options{
		Roots: []history.SourceRoot{rootSpec}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	waitForCatalog(t, func() bool {
		stored, ok, err := store.LoadSource(ctx, persistedRoot.ID, path)
		if err != nil || !ok || stored.ParserVersion != parserVersion(history.ProviderCodex) {
			return false
		}
		page, err := service.ListThreads(ctx, ListQuery{IncludeGuardians: true, Archived: "include", Limit: 10})
		return err == nil && len(page.Items) == 1 && page.Items[0].Kind == string(history.ThreadGuardian) &&
			page.Items[0].ParentNativeID == "parent-thread"
	})
}

func TestThreadCatalogServiceOverlaysNamesAndSurfacesMalformedFile(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "77777777-7777-4777-8777-777777777777"
	writeClaudeCatalogFixture(t, filepath.Join(projectPath, nativeID+".jsonl"), nativeID, "2026-07-21T10:00:00Z", "Named thread prompt")

	// A temp-dir truth file — never the real ~/.nanod/thread-names.json.
	namesPath := filepath.Join(t.TempDir(), "thread-names.json")
	good := fmt.Sprintf(`{"version":1,"names":[{"provider":"claude","nativeThreadId":%q,"name":"influence graph classifier","provenance":"manual","updatedAt":"2026-07-21T18:00:00Z"}]}`, nativeID)
	if err := os.WriteFile(namesPath, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots:     []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
		NamesPath: namesPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var threadID string
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != 1 || page.Items[0].Name == nil {
			return false
		}
		threadID = page.Items[0].ID
		return page.Items[0].Name.Value == "influence graph classifier"
	})
	listItem, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
	if err != nil || listItem.Items[0].Name.Provenance != "manual" || listItem.Items[0].Name.Evidence != "user_assigned" {
		t.Fatalf("list name overlay = %#v, %v", listItem.Items, err)
	}
	detail, err := service.LoadThread(context.Background(), threadID)
	if err != nil || detail.Name == nil || detail.Name.Value != "influence graph classifier" ||
		detail.Name.Evidence != "user_assigned" || detail.Name.Provenance != "manual" {
		t.Fatalf("detail name overlay = %#v, %v", detail.Name, err)
	}
	if status, err := service.IndexStatus(context.Background()); err != nil || status.NamesError != nil {
		t.Fatalf("healthy names status = %#v, %v", status, err)
	}

	// A malformed edit must fail loudly, keep serving the catalog, retain the
	// last good name, and never rewrite the truth file.
	if err := os.WriteFile(namesPath, []byte(`{"version":1,"names":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !service.RequestRefresh() {
		t.Fatal("malformed-file refresh was not accepted")
	}
	waitForCatalog(t, func() bool {
		status, err := service.IndexStatus(context.Background())
		return err == nil && status.State == "degraded" && status.NamesError != nil
	})
	afterMalformed, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
	if err != nil || len(afterMalformed.Items) != 1 || afterMalformed.Items[0].Name == nil ||
		afterMalformed.Items[0].Name.Value != "influence graph classifier" {
		t.Fatalf("catalog stopped serving last-good names after malformed edit: %#v, %v", afterMalformed.Items, err)
	}
	raw, err := os.ReadFile(namesPath)
	if err != nil || string(raw) != `{"version":1,"names":[` {
		t.Fatalf("nano rewrote the malformed truth file: %q, %v", string(raw), err)
	}
}

func TestThreadCatalogServiceUsesAppendCheckpointAndRebuildsReplacement(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "33333333-3333-4333-8333-333333333333"
	path := filepath.Join(projectPath, nativeID+".jsonl")
	initial := claudeCatalogUserRow(nativeID, "2026-07-10T12:00:00Z", "Append checkpoint")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rootSpec := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{rootSpec},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var persistedRoot history.PersistedRoot
	var transcriptThreadID, initialTranscriptKey string
	waitForCatalog(t, func() bool {
		roots, err := store.ListRoots(context.Background())
		if err != nil || len(roots) != 1 {
			return false
		}
		persistedRoot = roots[0]
		source, ok, err := store.LoadSource(context.Background(), roots[0].ID, path)
		return err == nil && ok && source.CompleteLines == 1 && source.Generation == 1
	})
	waitForCatalog(t, func() bool {
		source, ok, err := store.LoadSource(context.Background(), persistedRoot.ID, path)
		if err != nil || !ok || source.ThreadID == "" {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), source.ThreadID)
		if err != nil || !exists || checkpoint.CompleteLines != 1 || checkpoint.LatestSeq == 0 {
			return false
		}
		transcriptThreadID, initialTranscriptKey = source.ThreadID, checkpoint.ProjectionKey
		return initialTranscriptKey != ""
	})

	appendFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	assistant := fmt.Sprintf("{\"type\":\"assistant\",\"sessionId\":%q,\"timestamp\":\"2026-07-10T12:01:00Z\",\"message\":{\"role\":\"assistant\",\"model\":\"claude-appended\",\"content\":[]}}\n", nativeID)
	if _, err := appendFile.WriteString(strings.TrimSuffix(assistant, "\n")); err != nil {
		appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	if !service.RequestRefresh() {
		t.Fatal("incomplete append refresh was not accepted")
	}
	waitForCatalog(t, func() bool {
		source, ok, err := store.LoadSource(context.Background(), persistedRoot.ID, path)
		if err != nil || !ok || source.Source.Size != int64(len(initial)+len(assistant)-1) ||
			source.CompleteLines != 1 || source.Generation != 1 ||
			(source.Projection != nil && source.Projection.Metadata.Model.Value != nil) {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), transcriptThreadID)
		return err == nil && exists && checkpoint.ProjectionKey == initialTranscriptKey &&
			checkpoint.CompleteLines == 1
	})
	appendFile, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendFile.WriteString("\n"); err != nil {
		appendFile.Close()
		t.Fatal(err)
	}
	if err := appendFile.Close(); err != nil {
		t.Fatal(err)
	}
	if !service.RequestRefresh() {
		t.Fatal("completed append refresh was not accepted")
	}
	waitForCatalog(t, func() bool {
		source, ok, err := store.LoadSource(context.Background(), persistedRoot.ID, path)
		if err != nil || !ok || source.CompleteLines != 2 || source.Generation != 1 ||
			source.Projection == nil || source.Projection.Metadata.Model.Value == nil ||
			*source.Projection.Metadata.Model.Value != "claude-appended" {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), transcriptThreadID)
		return err == nil && exists && checkpoint.ProjectionKey == initialTranscriptKey &&
			checkpoint.CompleteLines == 2
	})

	// A new inode at the same path is a new source generation and must be
	// projected from byte zero rather than from the old append checkpoint.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement := claudeCatalogUserRow(nativeID, "2026-07-10T13:00:00Z", "Replacement projection")
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if !service.RequestRefresh() {
		t.Fatal("replacement refresh was not accepted")
	}
	waitForCatalog(t, func() bool {
		source, ok, err := store.LoadSource(context.Background(), persistedRoot.ID, path)
		if err != nil || !ok || source.Generation != 2 || source.CompleteLines != 1 ||
			source.Projection == nil || source.Projection.Preview == nil ||
			*source.Projection.Preview != "Replacement projection" {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), transcriptThreadID)
		return err == nil && exists && checkpoint.SourceGeneration == 2 &&
			checkpoint.CompleteLines == 1 && checkpoint.ProjectionKey != initialTranscriptKey
	})
}

func TestThreadCatalogServiceIndexesCodexMetadata(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "sessions")
	dayPath := filepath.Join(rootPath, "2026", "07", "10")
	if err := os.MkdirAll(dayPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "44444444-4444-4444-8444-444444444444"
	path := filepath.Join(dayPath, "rollout-2026-07-10T14-00-00-"+nativeID+".jsonl")
	fixture := fmt.Sprintf("{\"timestamp\":\"2026-07-10T14:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":\"/workspace/codex\",\"cli_version\":\"0.150.0\",\"git\":{\"branch\":\"feature/explorer\"}}}\n", nativeID) +
		"{\"timestamp\":\"2026-07-10T14:01:00Z\",\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-test\",\"effort\":\"high\",\"approval_policy\":\"on-request\",\"collaboration_mode\":{\"settings\":{\"service_tier\":\"priority\"}}}}\n" +
		"{\"timestamp\":\"2026-07-10T14:02:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Inspect Codex history.\"}}\n"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var page Page
	waitForCatalog(t, func() bool {
		var err error
		page, err = service.ListThreads(context.Background(), ListQuery{
			Archived: "include", Limit: 10,
		})
		return err == nil && len(page.Items) == 1
	})
	item := page.Items[0]
	if item.NativeID != nativeID || item.Preview == nil || *item.Preview != "Inspect Codex history." ||
		item.Metadata.Model.Value == nil || *item.Metadata.Model.Value != "gpt-test" ||
		item.Metadata.Priority.Value == nil || *item.Metadata.Priority.Value != "priority" {
		t.Fatalf("Codex catalog item = %#v", item)
	}
}

func TestTranscriptParserBackfillRebuildsUnchangedCodexV8ServiceTier(t *testing.T) {
	ctx := context.Background()
	rootPath := filepath.Join(t.TempDir(), "sessions")
	dayPath := filepath.Join(rootPath, "2026", "08", "25")
	if err := os.MkdirAll(dayPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "77777777-7777-4777-8777-777777777777"
	path := filepath.Join(dayPath, "rollout-2026-08-25T22-29-54-"+nativeID+".jsonl")
	fixture := fmt.Sprintf("{\"timestamp\":\"2026-08-25T22:29:54.000Z\",\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", nativeID) +
		"{\"timestamp\":\"2026-08-25T22:29:54.540Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"thread_settings_applied\",\"thread_settings\":{\"service_tier\":\"priority\"}}}\n" +
		"{\"timestamp\":\"2026-08-25T22:29:54.568Z\",\"type\":\"turn_context\",\"payload\":{\"turn_id\":\"tier-turn\",\"service_tier\":\"default\"}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Prompt\"}}\n"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}
	service, err := newTestCatalogService(ctx, store, Options{
		Roots: []history.SourceRoot{root}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var threadID string
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(ctx, ListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != 1 {
			return false
		}
		threadID = page.Items[0].ID
		return true
	})
	persistedRoot, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, persistedRoot.ID, path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %t, %v", stored, ok, err)
	}
	var checkpoint history.TranscriptCheckpoint
	var exists bool
	waitForCatalog(t, func() bool {
		checkpoint, exists, err = store.LoadTranscriptCheckpoint(ctx, threadID)
		return err == nil && exists
	})
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := history.ProjectParsedFile(ctx, history.ParsedFileRequest{Source: stored.Source, Reader: file, SpoolDir: t.TempDir()})
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	defer parsed.Close()
	if _, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: threadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: history.ProviderCodex, ParserVersion: "codex-transcript-v8", CompletionRuleVersion: parsed.CompletionRuleVersion,
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: parsed.Transcript,
	}); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(ctx, 10)
	if err != nil || len(candidates) != 1 || candidates[0].ThreadID != threadID || candidates[0].ParserVersion != "codex-transcript-v8" {
		t.Fatalf("v8 backfill candidates = %#v, %v", candidates, err)
	}
	rebuilt, _, err := service.runTranscriptParserBackfillBatch(ctx, 1)
	if err != nil || rebuilt != 1 {
		t.Fatalf("backfill rebuilt = %d, %v", rebuilt, err)
	}
	checkpoint, exists, err = store.LoadTranscriptCheckpoint(ctx, threadID)
	if err != nil || !exists || checkpoint.ParserVersion != history.CurrentTranscriptParserVersion(history.ProviderCodex) {
		t.Fatalf("module checkpoint version=%s, exists=%t, err=%v", checkpoint.ParserVersion, exists, err)
	}
	page, err := service.LoadThreadUsage(ctx, threadID, 0, 10)
	if err != nil || len(page.Turns) != 1 || page.Turns[0].ServiceTier != "priority" {
		t.Fatalf("backfilled usage = %#v, %v", page, err)
	}
}

func TestTranscriptParserBackfillMarksFailedRebuildSoLaterBatchesSkipIt(t *testing.T) {
	ctx := context.Background()
	rootPath := filepath.Join(t.TempDir(), "sessions")
	dayPath := filepath.Join(rootPath, "2026", "08", "25")
	if err := os.MkdirAll(dayPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "88888888-8888-4888-8888-888888888888"
	path := filepath.Join(dayPath, "rollout-2026-08-25T22-29-54-"+nativeID+".jsonl")
	fixture := fmt.Sprintf("{\"timestamp\":\"2026-08-25T22:29:54.000Z\",\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", nativeID) +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"Prompt\"}}\n"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}
	service, err := newTestCatalogService(ctx, store, Options{
		Roots: []history.SourceRoot{root}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var threadID string
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(ctx, ListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != 1 {
			return false
		}
		threadID = page.Items[0].ID
		return true
	})
	persistedRoot, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, persistedRoot.ID, path)
	if err != nil || !ok {
		t.Fatalf("stored source = %#v, %t, %v", stored, ok, err)
	}
	var checkpoint history.TranscriptCheckpoint
	var exists bool
	waitForCatalog(t, func() bool {
		checkpoint, exists, err = store.LoadTranscriptCheckpoint(ctx, threadID)
		return err == nil && exists
	})
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := history.ProjectParsedFile(ctx, history.ParsedFileRequest{Source: stored.Source, Reader: file, SpoolDir: t.TempDir()})
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	defer parsed.Close()
	if _, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: threadID, SourceID: stored.ID, SourceGeneration: stored.Generation,
		Provider: history.ProviderCodex, ParserVersion: "codex-transcript-v8", CompletionRuleVersion: parsed.CompletionRuleVersion,
		ExpectedProjectionKey: checkpoint.ProjectionKey, Result: parsed.Transcript,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rebuilt, _, err := service.runTranscriptParserBackfillBatch(ctx, 1)
	if err != nil || rebuilt != 1 {
		t.Fatalf("failed rebuild = %d, %v", rebuilt, err)
	}
	candidates, err := store.ListTranscriptParserBackfillCandidates(ctx, 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("failed rebuild still a candidate = %#v, %v", candidates, err)
	}
	checkpoint, exists, err = store.LoadTranscriptCheckpoint(ctx, threadID)
	if err != nil || !exists || checkpoint.ErrorSummary == nil || !strings.Contains(*checkpoint.ErrorSummary, "usage backfill:") {
		t.Fatalf("error_summary = %#v, %t, %v", checkpoint.ErrorSummary, exists, err)
	}
}

// TestCodexReaderBumpFullyRebuildsOlderSourcesInPriorityOrder catches a
// backfill that skips older sources, publishes an authoritative repair, loses
// priority order, or waits for the whole corpus before publishing. Boundary:
// runTranscriptParserBackfillBatch with the catalog's published transcript
// changes and saved checkpoints.
func TestCodexReaderBumpFullyRebuildsOlderSourcesInPriorityOrder(t *testing.T) {
	for _, oldReader := range []string{"codex-reader-v34", "codex-reader-v36"} {
		t.Run(oldReader, func(t *testing.T) { codexOlderReaderBackfill(t, oldReader) })
	}
}

func codexOlderReaderBackfill(t *testing.T, oldReader string) {
	ctx := context.Background()
	rootPath := filepath.Join(t.TempDir(), "sessions")
	dayPath := filepath.Join(rootPath, "2026", "09", "15")
	if err := os.MkdirAll(dayPath, 0o700); err != nil {
		t.Fatal(err)
	}
	type fixture struct {
		id, path, prompt string
		affected         bool
	}
	fixtures := []fixture{
		{id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", prompt: "open target", affected: true},
		{id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", prompt: "attached target", affected: true},
		{id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", prompt: "healthy repeat", affected: false},
	}
	for index := range fixtures {
		f := &fixtures[index]
		f.path = filepath.Join(dayPath, fmt.Sprintf("rollout-2026-09-15T20-0%d-00-%s.jsonl", index, f.id))
		turn := "turn-" + f.id
		response := fmt.Sprintf(`{"timestamp":"2026-09-15T20:0%d:00Z","type":"response_item","payload":{"type":"message","id":"response-%d","role":"user","content":[{"type":"input_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":%q}}}`+"\n", index, index, f.prompt, turn)
		second := strings.Replace(response, fmt.Sprintf(`"response-%d"`, index), fmt.Sprintf(`"response-%d-repeat"`, index), 1)
		if f.affected {
			second = fmt.Sprintf(`{"timestamp":"2026-09-15T20:0%d:01Z","type":"event_msg","payload":{"type":"item_completed","turn_id":%q,"item":{"type":"UserMessage","id":"completed-%d","content":[{"type":"text","text":%q}]}}}`+"\n", index, turn, index, f.prompt)
		}
		if err := os.WriteFile(f.path, []byte(fmt.Sprintf(`{"timestamp":"2026-09-15T20:0%d:00Z","type":"session_meta","payload":{"id":%q}}`+"\n", index, f.id)+response+second), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: rootPath}
	var logMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logMu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}
	service, err := newTestCatalogService(ctx, store, Options{Roots: []history.SourceRoot{root}, DisableWatcher: true, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetTranscriptChanged(func(context.Context, TranscriptChange) {})
	service.RequestRefresh() // the explicit discovery a new store needs
	waitForCatalog(t, func() bool {
		page, err := service.ListThreads(ctx, ListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != len(fixtures) {
			return false
		}
		for _, item := range page.Items {
			if _, exists, err := store.LoadTranscriptCheckpointHeader(ctx, item.ID); err != nil || !exists {
				return false
			}
		}
		return true
	})
	waitTranscriptChangeDrain(t, service)
	persistedRoot, err := store.EnsureRoot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	threadByNative := map[string]string{}
	page, err := service.ListThreads(ctx, ListQuery{Archived: "include", Limit: 10})
	if err != nil || len(page.Items) != len(fixtures) {
		t.Fatalf("catalog items=%d err=%v", len(page.Items), err)
	}
	for _, item := range page.Items {
		threadByNative[item.NativeID] = item.ID
	}
	current := history.CurrentTranscriptParserVersion(history.ProviderCodex)
	old := strings.Replace(current, "codex-reader-v37", oldReader, 1)
	if old == current {
		t.Fatalf("current parser %q is not reader v37", current)
	}
	for _, f := range fixtures {
		stored, ok, err := store.LoadSource(ctx, persistedRoot.ID, f.path)
		if err != nil || !ok {
			t.Fatalf("load source %s: %t %v", f.id, ok, err)
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpointHeader(ctx, stored.ThreadID)
		if err != nil || !exists {
			t.Fatalf("checkpoint %s: %t %v", f.id, exists, err)
		}
		file, err := os.Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := history.ProjectParsedFile(ctx, history.ParsedFileRequest{Source: stored.Source, Reader: file, SpoolDir: t.TempDir()})
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{Mode: history.TranscriptRebuild, ThreadID: stored.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: history.ProviderCodex, ParserVersion: old, CompletionRuleVersion: parsed.CompletionRuleVersion, ExpectedProjectionKey: checkpoint.ProjectionKey, Result: parsed.Transcript}); err != nil {
			parsed.Close()
			t.Fatal(err)
		}
		parsed.Close()
	}
	service.SetTranscriptRepairPriority(func(candidate history.TranscriptParserBackfillCandidate) int {
		switch candidate.NativeThreadID {
		case fixtures[0].id:
			return 0
		case fixtures[1].id:
			return 1
		default:
			return 2
		}
	})
	changed := make(chan TranscriptChange, 4)
	service.SetTranscriptChanged(func(_ context.Context, change TranscriptChange) { changed <- change })
	if inspected, _, err := service.runTranscriptParserBackfillBatch(ctx, 1); err != nil || inspected != 1 {
		t.Fatalf("first batch=%d err=%v", inspected, err)
	}
	select {
	case change := <-changed:
		if change.NativeThreadID != fixtures[0].id {
			t.Fatalf("first published change=%#v, want a full rebuild of the first priority", change)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first rebuild did not publish independently")
	}
	remaining, err := store.ListTranscriptParserBackfillCandidates(ctx, 10)
	if err != nil || len(remaining) != 2 {
		t.Fatalf("first publication waited for corpus: remaining=%d err=%v", len(remaining), err)
	}
	for range 2 {
		if inspected, _, err := service.runTranscriptParserBackfillBatch(ctx, 1); err != nil || inspected != 1 {
			t.Fatalf("later batch=%d err=%v", inspected, err)
		}
	}
	// The full rebuild also rewrites the source without serialization twins,
	// after the two sources with twins.
	for _, f := range fixtures[1:] {
		select {
		case change := <-changed:
			if change.NativeThreadID != f.id {
				t.Fatalf("published change=%#v, want a full rebuild of %s", change, f.id)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("rebuild of %s did not publish", f.id)
		}
	}
	waitTranscriptChangeDrain(t, service)
	for _, f := range fixtures {
		checkpoint, exists, err := store.LoadTranscriptCheckpointHeader(ctx, threadByNative[f.id])
		if err != nil || !exists || checkpoint.ParserVersion != current {
			t.Fatalf("%s checkpoint version=%q exists=%t err=%v, want the full rebuild stamped current", f.id, checkpoint.ParserVersion, exists, err)
		}
	}
	for _, f := range fixtures[:2] {
		transcript, err := store.ReadTranscript(ctx, history.TranscriptReadRequest{ThreadID: threadByNative[f.id], Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, item := range transcript.Items {
			if item.Text == f.prompt {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s prompt count=%d", f.id, count)
		}
	}
	appendFile, err := os.OpenFile(fixtures[2].path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = appendFile.WriteString(`{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"append stayed correct"}]}}` + "\n")
	appendFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	service.requestPathRefresh([]string{fixtures[2].path})
	waitForCatalogWithin(t, 5*time.Second, func() bool {
		checkpoint, ok, err := store.LoadTranscriptCheckpointHeader(ctx, threadByNative[fixtures[2].id])
		return err == nil && ok && checkpoint.ParserVersion == current
	})
	logMu.Lock()
	joinedLogs := strings.Join(logs, "\n")
	logMu.Unlock()
	if !strings.Contains(joinedLogs, "inspected=1 repaired=1") {
		t.Fatalf("backfill log omitted the full rebuild: %q", joinedLogs)
	}
}

func TestThreadTranscriptFixedSnapshotBodyAndETagStableAcrossAppend(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "55555555-5555-4555-8555-555555555555"
	path := filepath.Join(projectPath, nativeID+".jsonl")
	var fixture strings.Builder
	fixture.WriteString(claudeCatalogUserRow(nativeID, "2026-07-10T15:00:00Z", "Long turn"))
	for index := 1; index <= 130; index++ {
		fixture.WriteString(fmt.Sprintf("{\"type\":\"assistant\",\"sessionId\":%q,\"timestamp\":\"2026-07-10T15:00:01Z\",\"message\":{\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", nativeID, fmt.Sprintf("Reply %03d", index)))
	}
	if err := os.WriteFile(path, []byte(fixture.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var threadID, olderCursor string
	waitForCatalogWithin(t, time.Minute, func() bool {
		page, err := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		if err != nil || len(page.Items) != 1 {
			return false
		}
		response, err := service.LoadTranscript(context.Background(), page.Items[0].ID, TranscriptQuery{
			Limit: 40, View: DefaultThreadTranscriptView,
		})
		if err != nil || response.Page.NextBeforeCursor == nil {
			return false
		}
		threadID, olderCursor = page.Items[0].ID, *response.Page.NextBeforeCursor
		return true
	})

	server := NewHandlers(service)
	target := "/api/v1/threads/" + threadID + "/transcript?limit=40&before=" + olderCursor
	requestPage := func(ifNoneMatch string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		if ifNoneMatch != "" {
			request.Header.Set("If-None-Match", ifNoneMatch)
		}
		server.HandleThreadTranscript(recorder, request, threadID)
		return recorder
	}
	before := requestPage("")
	if before.Code != http.StatusOK || before.Header().Get("ETag") == "" {
		t.Fatalf("initial older page = %d %s", before.Code, before.Body.String())
	}
	beforeBody, beforeETag := before.Body.String(), before.Header().Get("ETag")

	resultRow := fmt.Sprintf("{\"type\":\"result\",\"sessionId\":%q,\"timestamp\":\"2026-07-10T15:01:00Z\",\"result\":\"done\"}\n", nativeID)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(resultRow); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	service.RequestRefresh()
	waitForCatalog(t, func() bool {
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), threadID)
		return err == nil && exists && checkpoint.CompleteLines == 132
	})

	after := requestPage("")
	if after.Code != http.StatusOK || after.Body.String() != beforeBody || after.Header().Get("ETag") != beforeETag {
		t.Fatalf("fixed snapshot drifted after append: status=%d etag=%q/%q bodyEqual=%v",
			after.Code, beforeETag, after.Header().Get("ETag"), after.Body.String() == beforeBody)
	}
	conditional := requestPage(beforeETag)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("stable conditional response = %d %q", conditional.Code, conditional.Body.String())
	}
}

func TestTranscriptDevEvidencePreservesExactNativeTextAndRejectsReplacement(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "66666666-6666-4666-8666-666666666666"
	path := filepath.Join(projectPath, nativeID+".jsonl")
	raw := ` { "type" : "user", "sessionId" : "` + nativeID + `", "large" : 900719925474099312345, "timestamp" : "2026-07-10T16:00:00Z", "message" : { "role" : "user", "content" : [ { "type" : "text", "text" : "Exact evidence" } ] } } `
	housekeeping := `{"type":"custom-title","customTitle":"Evidence test"}`
	invalid := []byte{0xff, 'x'}
	fixture := append([]byte(raw+"\n"+housekeeping+"\n"), invalid...)
	fixture = append(fixture, '\n')
	if err := os.WriteFile(path, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs
	var threadID, itemID, invalidItemID string
	waitForCatalog(t, func() bool {
		page, listErr := service.ListThreads(context.Background(), ListQuery{Archived: "include", Limit: 10})
		if listErr != nil || len(page.Items) != 1 {
			return false
		}
		response, loadErr := service.LoadTranscript(context.Background(), page.Items[0].ID, TranscriptQuery{
			Limit: 10, View: DevThreadTranscriptView, HiddenSpecified: true,
		})
		if loadErr != nil || len(response.Items) != 3 {
			return false
		}
		threadID = page.Items[0].ID
		for _, item := range response.Items {
			if item.Category == "messages" {
				itemID = item.ID
			}
			if item.Category == "unclassified" && item.ProjectedSeq == response.Page.EndSeq {
				invalidItemID = item.ID
			}
		}
		if itemID == "" || invalidItemID == "" {
			return false
		}
		enhanced, enhancedErr := service.LoadTranscript(context.Background(), threadID, TranscriptQuery{
			Limit: 1, View: DefaultThreadTranscriptView, HiddenSpecified: true,
			HiddenCategories: []string{"reasoning", "toolCall", "toolResult", "system", "unclassified"},
		})
		if enhancedErr != nil || len(enhanced.Items) != 1 || enhanced.Items[0].Category != "messages" {
			return false
		}
		return true
	})
	ref, err := store.LoadTranscriptEvidenceRef(context.Background(), threadID, itemID)
	if err != nil || !ref.HasByteRange || ref.ByteStart != 0 || ref.ByteLength != int64(len(raw)+1) {
		t.Fatalf("module source range missing: %+v %v", ref, err)
	}
	retainedBefore, err := service.LoadTranscript(context.Background(), threadID, TranscriptQuery{Limit: 10, View: DevThreadTranscriptView, HiddenSpecified: true})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := service.LoadTranscriptEvidence(context.Background(), threadID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Raw == nil || *evidence.Raw != raw || !evidence.ValidJSON || !evidence.ValidUTF8 || evidence.RawBytes != len(raw) ||
		evidence.SourceLine != 1 || evidence.Provider != "claude" {
		t.Fatalf("evidence = %#v", evidence)
	}
	invalidEvidence, err := service.LoadTranscriptEvidence(context.Background(), threadID, invalidItemID)
	if err != nil {
		t.Fatal(err)
	}
	if invalidEvidence.Raw != nil || invalidEvidence.ValidUTF8 || invalidEvidence.ValidJSON ||
		invalidEvidence.RawBase64 != base64.StdEncoding.EncodeToString(invalid) {
		t.Fatalf("invalid UTF-8 evidence = %#v", invalidEvidence)
	}
	rewritten := append([]byte(strings.ReplaceAll(raw, "Exact evidence", "Other evidence")+"\n"+housekeeping+"\n"), invalid...)
	rewritten = append(rewritten, '\n')
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("rewrite regression did not preserve the source inode")
	}
	if _, err := service.LoadTranscriptEvidence(context.Background(), threadID, itemID); !errors.Is(err, errTranscriptEvidenceUnavailable) {
		t.Fatalf("same-inode rewritten evidence error = %v", err)
	}
	// Missing native evidence must not remove the already retained conversation.
	assertUnavailable := func(label string) {
		t.Helper()
		response := httptest.NewRecorder()
		(NewHandlers(service)).HandleThreadTranscriptEvidence(response, httptest.NewRequest(http.MethodGet, "/", nil), threadID, itemID)
		if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), `"evidence_unavailable"`) {
			t.Fatalf("%s: evidence response=%d %s", label, response.Code, response.Body.String())
		}
		retainedAfter, err := service.LoadTranscript(context.Background(), threadID, TranscriptQuery{Limit: 10, View: DevThreadTranscriptView, HiddenSpecified: true})
		if err != nil || len(retainedAfter.Items) != len(retainedBefore.Items) {
			t.Fatalf("%s: retained rows unavailable: %v", label, err)
		}
		for i, item := range retainedBefore.Items {
			if retainedAfter.Items[i].ID != item.ID || retainedAfter.Items[i].Text != item.Text {
				t.Fatalf("%s: retained view changed", label)
			}
		}
	}
	assertUnavailable("same-inode rewrite")
	if err := os.WriteFile(path, fixture[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	assertUnavailable("truncated")
	if err := os.Rename(path, path+".missing"); err != nil {
		t.Fatal(err)
	}
	assertUnavailable("missing")

}

func TestReadTranscriptEvidenceLineHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readTranscriptEvidenceLine(ctx, strings.NewReader(strings.Repeat("row\n", 1000)), 1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled evidence scan error = %v", err)
	}
}

// TestThreadCatalogHintRetriesFailedCheckpoint catches a transcript failure
// that stays failed until a full walk: with no periodic sweep, a held file's
// retry or next hint must repair an errored checkpoint even when the file has
// not changed. Boundary: a targeted pass naming the unchanged source.
func TestThreadCatalogHintRetriesFailedCheckpoint(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	path := filepath.Join(projectPath, nativeID+".jsonl")
	writeClaudeCatalogFixture(t, path, nativeID, "2026-08-01T09:00:00Z", "Recovery floor prompt")

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.requestPathRefresh([]string{path})

	var threadID string
	var sourceID int64
	waitForCatalog(t, func() bool {
		roots, err := store.ListRoots(context.Background())
		if err != nil || len(roots) != 1 {
			return false
		}
		source, ok, err := store.LoadSource(context.Background(), roots[0].ID, path)
		if err != nil || !ok || source.ThreadID == "" {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), source.ThreadID)
		if err != nil || !exists || checkpoint.ErrorSummary != nil {
			return false
		}
		threadID, sourceID = source.ThreadID, source.ID
		return true
	})

	// Degrade the checkpoint without touching the provider file.
	if err := store.MarkTranscriptError(context.Background(), threadID, sourceID, "injected transcript failure", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	service.requestPathRefresh([]string{path})
	waitForCatalog(t, func() bool {
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), threadID)
		return err == nil && exists && checkpoint.ErrorSummary == nil && checkpoint.CompleteLines == 2
	})
}

func TestThreadCatalogGrokSummaryToUpdatesPublishesTerminalTurn(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		name := "full"
		if targeted {
			name = "targeted"
		}
		t.Run(name, func(t *testing.T) {
			rootPath := filepath.Join(t.TempDir(), "sessions")
			sessionPath := filepath.Join(rootPath, "-workspace", "11111111-1111-4111-8111-111111111111")
			if err := os.MkdirAll(sessionPath, 0o700); err != nil {
				t.Fatal(err)
			}
			summaryPath := filepath.Join(sessionPath, "summary.json")
			updatesPath := filepath.Join(sessionPath, "updates.jsonl")
			if err := os.WriteFile(summaryPath, []byte(`{"info":{"id":"11111111-1111-4111-8111-111111111111","cwd":"/workspace"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			updates := `{"timestamp":1893456000,"method":"session/update","params":{"sessionId":"11111111-1111-4111-8111-111111111111","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Hi"}}}}` + "\n" +
				`{"timestamp":1893456001,"method":"session/update","params":{"sessionId":"11111111-1111-4111-8111-111111111111","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"The complete saved answer."}},"_meta":{"promptId":"p1"}}}` + "\n" +
				`{"timestamp":1893456002,"method":"_x.ai/session/update","params":{"sessionId":"11111111-1111-4111-8111-111111111111","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":8,"outputTokens":4,"totalTokens":12}}}}` + "\n"

			store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			service, err := newTestCatalogService(context.Background(), store, Options{
				Roots: []history.SourceRoot{{Provider: history.ProviderGrok, Kind: history.RootGrokSessions, Path: rootPath}}, DisableWatcher: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			service.RequestRefresh() // the explicit discovery a new store needs

			var threadID string
			waitForCatalog(t, func() bool {
				threads, err := store.ListThreads(context.Background(), history.ListThreadsParams{Limit: 10})
				if err != nil || len(threads.Items) != 1 {
					return false
				}
				threadID = threads.Items[0].ID
				page, err := store.ReadTranscript(context.Background(), history.TranscriptReadRequest{ThreadID: threadID})
				return err == nil && len(page.Items) == 1
			})
			if err := os.WriteFile(updatesPath, []byte(updates), 0o600); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if targeted {
				service.requestPathRefresh([]string{updatesPath})
			} else {
				service.RequestRefresh()
			}
			waitForCatalog(t, func() bool {
				page, err := store.ReadTranscript(context.Background(), history.TranscriptReadRequest{ThreadID: threadID})
				return err == nil && page.Projection.State == history.TranscriptReady && page.Projection.ErrorSummary == nil &&
					len(page.Turns) == 1 && page.Turns[0].Status == history.TurnCompleted &&
					len(page.Items) == 2 && page.Items[1].Text == "The complete saved answer."
			})
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("terminal Grok projection published after %s, want within 5s", elapsed)
			}
		})
	}
}

// TestThreadCatalogFreshnessRepairUsesCommittedSourceState pins the correctness
// constraint behind the targeted reads: a freshness cycle's transcript repair
// must see what ReconcileRoot just committed — a source row that did not exist
// before the commit, and a generation the commit advanced — not the state the
// scan read before it.
func TestThreadCatalogFreshnessRepairUsesCommittedSourceState(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "projects")
	projectPath := filepath.Join(rootPath, "-workspace")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	firstID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	secondID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	firstPath := filepath.Join(projectPath, firstID+".jsonl")
	secondPath := filepath.Join(projectPath, secondID+".jsonl")
	writeClaudeCatalogFixture(t, firstPath, firstID, "2026-08-01T10:00:00Z", "Committed state prompt")

	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newTestCatalogService(context.Background(), store, Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: rootPath}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.RequestRefresh() // the explicit discovery a new store needs

	var rootID int64
	waitForCatalog(t, func() bool {
		roots, err := store.ListRoots(context.Background())
		if err != nil || len(roots) != 1 {
			return false
		}
		rootID = roots[0].ID
		source, ok, err := store.LoadSource(context.Background(), rootID, firstPath)
		return err == nil && ok && source.ThreadID != ""
	})

	// A source discovered in this very cycle has no committed row until
	// ReconcileRoot commits, so its transcript only exists if the repair read
	// the post-commit state.
	writeClaudeCatalogFixture(t, secondPath, secondID, "2026-08-01T11:00:00Z", "Newly discovered prompt")
	service.RequestRefresh()
	waitForCatalog(t, func() bool {
		source, ok, err := store.LoadSource(context.Background(), rootID, secondPath)
		if err != nil || !ok || source.ThreadID == "" {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), source.ThreadID)
		return err == nil && exists && checkpoint.ErrorSummary == nil && checkpoint.CompleteLines == 2
	})

	// A replacement advances the committed generation, and ApplyTranscript is
	// guarded on it: a repair carrying the pre-commit generation cannot land.
	firstSource, _, err := store.LoadSource(context.Background(), rootID, firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	writeClaudeCatalogFixture(t, firstPath, firstID, "2026-08-01T12:00:00Z", "Replacement after commit")
	service.RequestRefresh()
	waitForCatalog(t, func() bool {
		source, ok, err := store.LoadSource(context.Background(), rootID, firstPath)
		if err != nil || !ok || source.Generation != firstSource.Generation+1 {
			return false
		}
		checkpoint, exists, err := store.LoadTranscriptCheckpoint(context.Background(), source.ThreadID)
		return err == nil && exists && checkpoint.ErrorSummary == nil &&
			checkpoint.SourceGeneration == source.Generation
	})
}

// catalogScanLog keeps the one line per explicit recovery walk a service
// emits, so a test can count walks without reaching into the worker.
type catalogScanLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *catalogScanLog) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if !strings.HasPrefix(line, "thread catalog recovery ") {
		return
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

func (l *catalogScanLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func TestCanAppendProjectionRejectsSameSizeRewrite(t *testing.T) {
	mtime := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	stored := history.StoredSource{
		Source:         history.SourceCandidate{Size: 100, ModTime: mtime, Device: 1, Inode: 2, Kind: history.ThreadMain},
		CompleteOffset: 100, ParserVersion: "parser-v1", Projection: &history.ThreadProjection{},
	}
	rewritten := stored.Source
	rewritten.ModTime = mtime.Add(time.Second)
	if canAppendProjection(stored, rewritten, "parser-v1") {
		t.Fatal("same-size in-place rewrite was treated as an append")
	}
	if !transcriptSourceChanged(stored, rewritten) {
		t.Fatal("same-size in-place rewrite did not force a transcript rebuild")
	}
	appended := rewritten
	appended.Size++
	if !canAppendProjection(stored, appended, "parser-v1") {
		t.Fatal("same-inode growth was not treated as an append")
	}
	if transcriptSourceChanged(stored, appended) {
		t.Fatal("same-inode growth unnecessarily forced a transcript rebuild")
	}
}

func TestCanAppendProjectionRebuildsGrokMetadata(t *testing.T) {
	mtime := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	stored := history.StoredSource{
		Source: history.SourceCandidate{
			Root: history.SourceRoot{Provider: history.ProviderGrok},
			Path: "/sessions/thread/updates.jsonl", Size: 100, ModTime: mtime,
			Device: 1, Inode: 2, Kind: history.ThreadMain,
		},
		CompleteOffset: 100, ParserVersion: "grok-metadata-v2", Projection: &history.ThreadProjection{},
	}
	appended := stored.Source
	appended.Size++
	appended.ModTime = mtime.Add(time.Second)
	if canAppendProjection(stored, appended, stored.ParserVersion) {
		t.Fatal("Grok metadata must re-read rewritten summary.json instead of appending")
	}
}

func TestCanAppendTranscriptProjectionRetriesDegradedCheckpointAsRebuild(t *testing.T) {
	parser, completion, err := history.ParsedFileVersions(history.ProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	current := history.SourceCandidate{Root: history.SourceRoot{Provider: history.ProviderClaude}, Size: 100}
	stored := history.StoredSource{ID: 7, ThreadID: "nth_one", Generation: 2}
	checkpoint := history.TranscriptCheckpoint{
		ProjectionKey: "ntp_one", ThreadID: stored.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: history.ProviderClaude,
		ParserVersion: parser, CompletionRuleVersion: completion,
		CompleteOffset: current.Size, ReadOffset: current.Size,
	}
	if !canAppendParsedFile(checkpoint, stored, current) {
		t.Fatal("healthy transcript checkpoint was not append-compatible")
	}
	summary := "prior rebuild failed"
	checkpoint.ErrorSummary = &summary
	if canAppendParsedFile(checkpoint, stored, current) {
		t.Fatal("degraded transcript checkpoint skipped its rebuild retry")
	}
}

func TestTranscriptBackfillPriorityOrdersOpenAttachedThenRecent(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	candidates := []history.TranscriptParserBackfillCandidate{
		{ThreadID: "ordinary-old", UpdatedAt: now.Add(-time.Hour)},
		{ThreadID: "ordinary-new", UpdatedAt: now},
		{ThreadID: "attached", UpdatedAt: now.Add(-2 * time.Hour)},
		{ThreadID: "open", UpdatedAt: now.Add(-3 * time.Hour)},
	}
	priorities := map[string]int{
		"open": 0, "attached": 1, "ordinary-old": 2, "ordinary-new": 2,
	}

	prioritizeTranscriptBackfillCandidates(candidates, priorities)

	want := []string{"open", "attached", "ordinary-new", "ordinary-old"}
	for i, candidate := range candidates {
		if candidate.ThreadID != want[i] {
			t.Fatalf("candidate %d = %q, want %q", i, candidate.ThreadID, want[i])
		}
	}
}

func TestThreadCatalogAppendRetainsBoundedPriorDiagnostics(t *testing.T) {
	stored := history.StoredSource{
		Diagnostics:       make([]history.Diagnostic, 20),
		DiagnosticSummary: history.DiagnosticSummary{Total: 20, Malformed: 20},
	}
	result := history.ProjectionResult{
		Diagnostics: make([]history.Diagnostic, 10),
		Summary:     history.DiagnosticSummary{Total: 10, UnknownRows: 10},
	}
	mergeAppendDiagnostics(&result, stored)
	if len(result.Diagnostics) != maxThreadCatalogDiagnosticDetails || result.Summary.Total != 30 ||
		result.Summary.Malformed != 20 || result.Summary.UnknownRows != 10 || result.Summary.Dropped != 5 {
		t.Fatalf("merged diagnostics = %d, %#v", len(result.Diagnostics), result.Summary)
	}
}

func TestThreadCatalogCursorRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"not-base64!", "e30", "eyJpZCI6InRoIiwidW5rbm93biI6MX0", string(make([]byte, maxThreadCatalogCursorBytes+1))} {
		if _, err := DecodeThreadCatalogCursor(value); !errors.Is(err, ErrInvalidThreadCatalogCursor) {
			t.Fatalf("cursor %q was accepted", value)
		}
	}
}

func writeClaudeCatalogFixture(t *testing.T, path, nativeID, timestamp, prompt string) {
	t.Helper()
	content := claudeCatalogUserRow(nativeID, timestamp, prompt) +
		fmt.Sprintf("{\"type\":\"assistant\",\"sessionId\":%q,\"timestamp\":%q,\"message\":{\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[]}}\n", nativeID, timestamp)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func claudeCatalogUserRow(nativeID, timestamp, prompt string) string {
	return fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q,\"timestamp\":%q,\"cwd\":\"/workspace/catalog\",\"gitBranch\":\"feature/catalog\",\"message\":{\"role\":\"user\",\"content\":%q}}\n", nativeID, timestamp, prompt)
}

func waitForCatalog(t *testing.T, ready func() bool) {
	t.Helper()
	waitForCatalogWithin(t, 5*time.Second, ready)
}

// The large conversion fixtures persist source observations as well as views.
// Their race-instrumented rebuild is not the ordinary catalog freshness test.
func waitForCatalogWithin(t *testing.T, budget time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for thread catalog")
}
