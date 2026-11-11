package hosted

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// Failure observed live: 95 operations of one Codex turn each read all 290 of
// its saved user rows, once a second and on every catalog commit, and the
// server stalled. Boundary: the shared read behind an operation's saved
// answer. Callers that ask while a read runs share the one read after it; none
// takes the running read's rows, which may be older than the commit that woke
// the caller, and nothing stays behind once they are answered.
func TestOperationsOfOneTurnShareOneFreshSavedRowsRead(t *testing.T) {
	const callers = 20
	var shared sharedSavedRows
	var mu sync.Mutex
	reads := 0
	running, release := make(chan struct{}), make(chan struct{})
	read := func(context.Context) ([]history.SavedUserRow, error) {
		mu.Lock()
		reads++
		generation := reads
		mu.Unlock()
		if generation == 1 {
			close(running)
			<-release // the read already running when the others ask
		}
		return []history.SavedUserRow{{ProjectionKey: string(rune('0' + generation))}}, nil
	}
	results := make(chan string, callers+1)
	call := func() {
		rows, err := shared.read(context.Background(), "thread\x00turn", read)
		if err != nil || len(rows) != 1 {
			t.Errorf("shared read: %v, %v", rows, err)
			results <- ""
			return
		}
		results <- rows[0].ProjectionKey
	}
	waitFor := func(want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			shared.mu.Lock()
			got := 0
			if key := shared.keys["thread\x00turn"]; key != nil {
				got = key.callers
			}
			shared.mu.Unlock()
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("callers waiting = %d, want %d", got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	go call()
	<-running
	for i := 0; i < callers; i++ {
		go call()
	}
	waitFor(callers + 1)
	close(release)
	stale := 0
	for i := 0; i < callers+1; i++ {
		if <-results == "1" {
			stale++
		}
	}
	if stale != 1 || reads != 2 {
		t.Fatalf("%d callers behind a running read made %d reads and %d took the running read's rows; want 2 reads and only its own caller", callers, reads, stale)
	}
	waitFor(0)
}
