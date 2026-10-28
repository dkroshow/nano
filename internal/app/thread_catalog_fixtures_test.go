package app

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
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
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}
