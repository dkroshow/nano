package app

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestRecordWireNudgeDoesNotBlockWhileTheWorkerIsBusy proves wire recording
// stays nonblocking when nothing drains the catalog queue (the steady state
// while a pass runs), and that repeated hints for one file coalesce.
func TestRecordWireNudgeDoesNotBlockWhileTheWorkerIsBusy(t *testing.T) {
	service := &threadCatalogService{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}}
	var nudges atomic.Int32
	f := newProviderRuntimeFixture(t, sessionConfig{ID: "nudge", Provider: "codex", CatalogRefresh: func(path string) { service.RequestPathRefresh([]string{path}); nudges.Add(1) }}, "nudge-thread", "")
	f.emit(t, []byte(`{"method":"thread/started","params":{"thread":{"id":"nudge-thread","path":"/sessions/rollout.jsonl"}}}`))
	waitForClaude(t, "known catalog path", func() bool { return nudges.Load() > 0 })
	before := nudges.Load()
	line := []byte(`{"method":"turn/completed","params":{"threadId":"nudge-thread","turn":{"id":"nudge-turn","status":"completed"}}}`)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			f.emit(t, line)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wire recording stalled behind a busy catalog worker")
	}
	waitForClaude(t, "all wire nudges", func() bool { return nudges.Load() >= before+100 })
	if len(service.dirtyOrder) != 1 || len(service.work) != 1 {
		t.Fatalf("queued %d sources and %d wakes, want 1 and 1 (coalesced)", len(service.dirtyOrder), len(service.work))
	}
}
