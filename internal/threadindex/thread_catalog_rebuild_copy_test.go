//go:build rebuildproof

package threadindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// This opt-in receipt uses a separately prepared SQLite online-backup clone.
// Provider sources are opened by the ordinary read-only backfill path. The
// immutable baseline and retained old served binary are sibling proof artifacts.
func TestCopiedIndexParserRebuild(t *testing.T) {
	path := os.Getenv("NANO_REBUILD_INDEX_COPY")
	if path == "" {
		t.Skip("set NANO_REBUILD_INDEX_COPY to the owned index-proof/rebuild.sqlite clone")
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "rebuild.sqlite" {
		t.Fatal("expected an absolute owned rebuild.sqlite clone")
	}
	proof := filepath.Dir(path)
	baseline, err := os.ReadFile(filepath.Join(proof, "baseline-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	if err = json.Unmarshal(baseline, &evidence); err != nil {
		t.Fatal(err)
	}
	ceiling := uint64(20000000000)
	switch os.Getenv("NANO_REBUILD_CEILING_BYTES") {
	case "", "20000000000":
	case "21000000000":
		ceiling = 21000000000 // Explicit opt-in for the sample-task continuation ruling.
	default:
		t.Fatal("the proof ceiling must be 20000000000 or 21000000000 bytes")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("prepare the clone before running the proof", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := copiedIndexResources(path, 0, ceiling); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	store, err := history.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	evidence["migrationSeconds"] = time.Since(started).Seconds()
	reader, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	checkResources := func() {
		t.Helper()
		var logicalBytes uint64
		if err := reader.QueryRow("SELECT (SELECT page_count FROM pragma_page_count)*(SELECT page_size FROM pragma_page_size)").Scan(&logicalBytes); err != nil {
			t.Fatal(err)
		}
		bounds, err := copiedIndexResources(path, logicalBytes, ceiling)
		evidence["resourceBounds"] = bounds
		if err != nil {
			raw, _ := json.MarshalIndent(evidence, "", "  ")
			if writeErr := os.WriteFile(filepath.Join(proof, "rebuild-resource-stop.json"), raw, 0600); writeErr != nil {
				t.Fatal(err, writeErr)
			}
			t.Fatal(err)
		}
	}
	checkResources()
	var schema, initialErrors int
	if err = reader.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if err = reader.QueryRow("SELECT COUNT(*) FROM conversation_projections WHERE state='active' AND error_summary IS NOT NULL AND error_summary!=''").Scan(&initialErrors); err != nil {
		t.Fatal(err)
	}
	evidence["migratedSchema"] = schema
	evidence["baselineActiveErrors"] = initialErrors
	candidates, err := store.ListTranscriptParserBackfillCandidates(ctx, 100000)
	if err != nil {
		t.Fatal(err)
	}
	evidence["initialCandidates"] = len(candidates)
	if len(candidates) == 0 {
		t.Fatal("copy has no parser-behind sources; a full rebuild was not exercised")
	}
	versions := map[string]string{}
	for _, provider := range []history.Provider{history.ProviderClaude, history.ProviderCodex, history.ProviderGrok} {
		versions[string(provider)] = history.CurrentTranscriptParserVersion(provider)
	}
	evidence["targetVersions"] = versions
	sidebar, err := store.ListThreads(ctx, history.ListThreadsParams{Archived: "include", Limit: 1})
	if err != nil || len(sidebar.Items) == 0 {
		t.Fatal("prior sidebar is unavailable after migration", err)
	}
	old, err := store.ReadTranscript(ctx, history.TranscriptReadRequest{ThreadID: candidates[0].ThreadID, Limit: 5})
	if err != nil || old.Projection.Key == "" {
		t.Fatal("prior transcript unavailable before reprojection", err)
	}
	evidence["priorSidebarAvailable"] = true
	evidence["priorTranscriptAvailable"] = true
	service := &Service{store: store, hub: newCatalogHub(), transcriptChanges: make(map[string]*pendingTranscriptChange)}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	// Observe an existing thread while the production scheduler is rebuilding.
	// This goroutine only reads; it cannot acknowledge or operate a hosted turn.
	stopReads := make(chan struct{})
	readDone := make(chan struct{})
	var readError error
	reads := 0
	go func() {
		defer close(readDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopReads:
				return
			case <-ticker.C:
				page, readErr := store.ReadTranscript(ctx, history.TranscriptReadRequest{ThreadID: candidates[0].ThreadID, Limit: 5})
				if readErr != nil {
					readError = readErr
					return
				}
				if page.Projection.Key == "" {
					readError = errors.New("empty projection during rebuild")
					return
				}
				reads++
			}
		}
	}()
	stopObserver := func() { close(stopReads); <-readDone }
	defer func() {
		if stopReads != nil {
			stopObserver()
		}
	}()
	save := func(name string) {
		t.Helper()
		raw, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(proof, name), append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	save("rebuild-start.json")
	rebuildStarted := time.Now()
	evidence["rebuildStartedAt"] = rebuildStarted.UTC().Format(time.RFC3339Nano)
	rebuilt := 0
	for {
		checkResources()
		n, _, err := service.RunTranscriptParserBackfillBatch(ctx, 8)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt += n
		var errorsNow int
		if err = reader.QueryRow("SELECT COUNT(*) FROM conversation_projections WHERE state='active' AND error_summary IS NOT NULL AND error_summary!=''").Scan(&errorsNow); err != nil {
			t.Fatal(err)
		}
		evidence["processed"] = rebuilt
		evidence["elapsedSeconds"] = time.Since(started).Seconds()
		evidence["activeErrors"] = errorsNow
		checkResources()
		save("rebuild-progress.json")
		t.Logf("processed=%d initial=%d elapsed=%.3fs active_errors=%d", rebuilt, len(candidates), time.Since(started).Seconds(), errorsNow)
		if errorsNow != initialErrors {
			t.Fatal("rebuild introduced a source error; inspect the owned copy and progress receipt")
		}
		if n == 0 {
			break
		}
	}
	stopObserver()
	stopReads = nil
	evidence["concurrentTranscriptReads"] = reads
	if readError != nil || reads == 0 {
		t.Fatal("transcript availability during rebuild was not proven", readError)
	}
	remaining, err := store.ListTranscriptParserBackfillCandidates(ctx, 1)
	if err != nil || len(remaining) != 0 {
		t.Fatal("parser rebuild did not finish", err)
	}
	evidence["remainingCandidates"] = 0
	evidence["fullReprojectionSeconds"] = time.Since(rebuildStarted).Seconds()
	evidence["totalWallSeconds"] = time.Since(started).Seconds()
	counts := map[string]int64{}
	for _, table := range []string{"conversation_items", "conversation_turns", "conversation_observations", "conversation_item_observations", "conversation_relations", "conversation_parse_identities", "conversation_parse_diagnostics"} {
		var count int64
		if err = reader.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	evidence["rowsAfter"] = counts
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence["bytesAfter"] = info.Size()
	if _, err := copiedIndexResources(path, 0, ceiling); err != nil {
		t.Fatal(err)
	}
	evidence["diskGrowthBytes"] = info.Size() - int64(evidence["bytes"].(float64))
	evidence["status"] = "reprojection complete; old-served-binary proof remains separate"
	save("rebuild-complete.json")
}
