package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

const transcriptChangeWorkerCount = 2

func queuedCatalogFixture(t *testing.T, provider string, nativeIDs []string) (*threadCatalogService, []string) {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, len(nativeIDs))
	kind := history.RootCodexSessions
	for i, native := range nativeIDs {
		var contents string
		if provider == "claude" {
			kind = history.RootClaudeProjects
			paths[i] = filepath.Join(root, "-workspace", native+".jsonl")
			contents = claudeCatalogUserRow(native, "2026-10-01T00:00:00Z", "saved prompt")
		} else {
			paths[i] = filepath.Join(root, "rollout-"+native+".jsonl")
			contents = fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"saved prompt\"}}\n", native)
		}
		if err := os.MkdirAll(filepath.Dir(paths[i]), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths[i], []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{Roots: []history.SourceRoot{{Provider: history.Provider(provider), Kind: kind, Path: root}}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	service.RequestRefresh()
	waitForCatalogWithin(t, 2*time.Minute, func() bool {
		status, err := service.IndexStatus(context.Background())
		return err == nil && status.TranscriptChangeBacklog == len(nativeIDs)
	})
	return service, paths
}
func appendCallbackFixture(t *testing.T, service *threadCatalogService, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\",\"message\":\"new saved answer\"}}\n")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	service.FileChanged(path)
}
