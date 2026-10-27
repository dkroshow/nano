package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

type blockingSupersededCollector struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (c *blockingSupersededCollector) CollectSupersededProjectionsBatch(ctx context.Context, _ int) (history.SupersededProjectionBatch, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == 1 {
		c.once.Do(func() { close(c.started) })
		select {
		case <-ctx.Done():
			return history.SupersededProjectionBatch{}, ctx.Err()
		case <-c.release:
		}
		return history.SupersededProjectionBatch{Collected: 64, Remaining: true}, nil
	}
	return history.SupersededProjectionBatch{Collected: 1}, nil
}

func TestSupersededProjectionCollectorRequiresExplicitEnable(t *testing.T) {
	t.Setenv("NANO_SUPERSEDED_COLLECTION", "")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	collector := &blockingSupersededCollector{started: make(chan struct{}), release: make(chan struct{})}
	service, err := newThreadCatalogService(context.Background(), store, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{
			Provider: history.ProviderCodex,
			Kind:     history.RootCodexSessions,
			Path:     t.TempDir(),
		}},
		DisableWatcher:                      true,
		supersededCollector:                 collector,
		EnableSupersededProjectionCollector: supersededProjectionCollectionEnabled(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	select {
	case <-service.catalogReady:
	case <-time.After(2 * time.Second):
		t.Fatal("cold catalog readiness waited for superseded collection")
	}
	select {
	case <-collector.started:
		t.Fatal("superseded collector started without explicit enablement")
	case <-time.After(50 * time.Millisecond):
	}
	service.Close()

	t.Setenv("NANO_SUPERSEDED_COLLECTION", "on")
	enabledStore, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enabledStore.Close() })
	enabledCollector := &blockingSupersededCollector{started: make(chan struct{}), release: make(chan struct{})}
	enabledService, err := newThreadCatalogService(context.Background(), enabledStore, threadCatalogServiceOptions{
		Roots: []history.SourceRoot{{
			Provider: history.ProviderCodex,
			Kind:     history.RootCodexSessions,
			Path:     t.TempDir(),
		}},
		DisableWatcher:                      true,
		supersededCollector:                 enabledCollector,
		EnableSupersededProjectionCollector: supersededProjectionCollectionEnabled(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(enabledService.Close)
	select {
	case <-enabledCollector.started:
	case <-time.After(2 * time.Second):
		t.Fatal("explicitly enabled superseded collector did not start")
	}
	close(enabledCollector.release)
}

func TestSupersededCollectorRunsBeforeCatalogReady(t *testing.T) {
	collector := &blockingSupersededCollector{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	service := &threadCatalogService{
		catalogReady:         make(chan struct{}),
		supersededCollector:  collector,
		supersededCollection: make(chan struct{}, 1),
		supersededDone:       make(chan struct{}),
		logf:                 func(string, ...any) {},
	}
	go service.runSupersededProjectionCollector(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-collector.release:
		default:
			close(collector.release)
		}
		<-service.supersededDone
	})
	select {
	case <-collector.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("superseded collector waited for catalogReady")
	}
}
