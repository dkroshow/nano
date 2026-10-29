package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threadindex"
)

func waitForCatalog(t *testing.T, ready func() bool) {
	t.Helper()
	waitForCatalogWithin(t, 10*time.Second, ready)
}
func waitForCatalogWithin(t *testing.T, budget time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("catalog condition did not become true")
}
func writeClaudeCatalogFixture(t *testing.T, path, nativeID, timestamp, prompt string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(claudeCatalogUserRow(nativeID, timestamp, prompt)), 0600); err != nil {
		t.Fatal(err)
	}
}
func claudeCatalogUserRow(nativeID, timestamp, prompt string) string {
	return fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q,\"timestamp\":%q,\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", nativeID, timestamp, prompt)
}

func newStoredCatalog(t *testing.T, store *history.Store) *threadCatalogService {
	t.Helper()
	service, err := newThreadCatalogService(context.Background(), store, threadindex.Options{Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

func newAttachmentNameCatalog(t *testing.T, metadata *hostedstore.MetadataStore) *threadCatalogService {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, err := newThreadCatalogService(context.Background(), store, threadindex.Options{Roots: []history.SourceRoot{{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: t.TempDir()}}, NamesPath: filepath.Join(t.TempDir(), "thread-names.json"), DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	catalog.SetCreationNameSource(metadata.CreationThreadNames)
	t.Cleanup(catalog.Close)
	return catalog
}
