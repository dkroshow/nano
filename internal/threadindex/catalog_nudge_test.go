package threadindex

import (
	"testing"
	"time"
)

// Sessions tests cover the wire hint; this checks its index queue stays nonblocking and conflated.
func TestRecordWireNudgeDoesNotBlockWhileTheWorkerIsBusy(t *testing.T) {
	service := &Service{work: make(chan struct{}, 1), dirty: map[string]*dirtySource{}}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			service.requestPathRefresh([]string{"/sessions/rollout.jsonl"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wire recording stalled behind a busy catalog worker")
	}
	if len(service.dirtyOrder) != 1 || len(service.work) != 1 {
		t.Fatalf("queued %d sources and %d wakes, want 1 and 1 (coalesced)", len(service.dirtyOrder), len(service.work))
	}
}
