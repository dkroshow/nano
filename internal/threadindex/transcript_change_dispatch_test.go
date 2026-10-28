package threadindex

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A parser bump can queue thousands of changes before the live callback is
// wired. The dispatcher must lose none, bound concurrency, coalesce queued
// duplicates and replay a thread changed while its callback is in flight.
func TestTranscriptChangeDispatcherBoundsParserRecoveryAndRetainsPendingChanges(t *testing.T) {
	const changedTranscripts = 5000
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{transcriptChanges: make(map[string]*pendingTranscriptChange)}
	service.transcriptChangedCond = sync.NewCond(&service.transcriptChangedMu)
	blocked := make(chan struct{})
	var release sync.Once
	service.transcriptChangeWorkerWg.Add(transcriptChangeWorkerCount)
	for worker := 0; worker < transcriptChangeWorkerCount; worker++ {
		go service.runTranscriptChangeWorker(ctx)
	}
	t.Cleanup(func() {
		cancel()
		release.Do(func() { close(blocked) })
		service.transcriptChangedMu.Lock()
		service.transcriptChangeStopping = true
		service.transcriptChangedCond.Broadcast()
		service.transcriptChangedMu.Unlock()
		service.transcriptChangeWorkerWg.Wait()
	})
	change := func(index int) TranscriptChange {
		return TranscriptChange{ThreadID: fmt.Sprintf("nth_%d", index), Provider: "codex", NativeThreadID: fmt.Sprintf("native-%d", index)}
	}
	for index := 0; index < changedTranscripts; index++ {
		service.notifyTranscriptChanged(change(index))
	}
	if backlog, inFlight := service.transcriptChangeStatus(); backlog != changedTranscripts || inFlight != 0 {
		t.Fatalf("before callback: backlog=%d in-flight=%d", backlog, inFlight)
	}
	started := make(chan struct{}, 2*transcriptChangeWorkerCount)
	var active, maxActive atomic.Int32
	var callsMu sync.Mutex
	calls := make(map[string]int)
	service.SetTranscriptChanged(func(ctx context.Context, change TranscriptChange) {
		current := active.Add(1)
		for current > maxActive.Load() && !maxActive.CompareAndSwap(maxActive.Load(), current) {
		}
		defer active.Add(-1)
		callsMu.Lock()
		calls[change.ThreadID]++
		callsMu.Unlock()
		if change.ThreadID == "nth_0" || change.ThreadID == "nth_1" {
			started <- struct{}{}
			select {
			case <-blocked:
			case <-ctx.Done():
			}
		}
	})
	for worker := 0; worker < transcriptChangeWorkerCount; worker++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("transcript workers did not start")
		}
	}
	service.notifyTranscriptChanged(change(0))
	service.notifyTranscriptChanged(change(changedTranscripts - 1))
	service.notifyTranscriptChanged(change(changedTranscripts - 1))
	if backlog, inFlight := service.transcriptChangeStatus(); backlog != changedTranscripts || inFlight != transcriptChangeWorkerCount {
		t.Fatalf("blocked callbacks: backlog=%d in-flight=%d", backlog, inFlight)
	}
	release.Do(func() { close(blocked) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		backlog, inFlight := service.transcriptChangeStatus()
		if backlog == 0 && inFlight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript recovery did not drain: backlog=%d in-flight=%d", backlog, inFlight)
		}
		time.Sleep(time.Millisecond)
	}
	if maxActive.Load() > transcriptChangeWorkerCount {
		t.Fatalf("callback concurrency=%d, workers=%d", maxActive.Load(), transcriptChangeWorkerCount)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if len(calls) != changedTranscripts {
		t.Fatalf("callbacks reached %d threads, want %d", len(calls), changedTranscripts)
	}
	for index := 0; index < changedTranscripts; index++ {
		want := 1
		if index == 0 {
			want = 2
		}
		if got := calls[change(index).ThreadID]; got != want {
			t.Fatalf("thread %d: callbacks=%d, want %d", index, got, want)
		}
	}
}
