package sidebar

import (
	"slices"
	"sync"

	"github.com/dkroshow/nano/internal/sessions"
)

// warmBudget caps the distinct threads warm across all browser streams.
const warmBudget = 150

// warmStreams holds every open warm stream and the threads they warm.
type warmStreams struct {
	mu      sync.Mutex
	threads map[string]int // hosted thread id: warm streams holding it
	streams map[*sessions.WarmStream]bool
}

// Admit counts a stream's hold on a thread. A thread already warm costs no
// budget; a new one is refused once the budget is spent.
func (w *warmStreams) admit(ref string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threads[ref] == 0 && len(w.threads) >= warmBudget {
		return false
	}
	if w.threads == nil {
		w.threads = make(map[string]int)
	}
	w.threads[ref]++
	return true
}

// Release removes a stream's hold on a warm thread.
func (w *warmStreams) release(ref string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threads[ref]--; w.threads[ref] <= 0 {
		delete(w.threads, ref)
	}
}

// Open creates a shared warm-stream queue with bounded backlog.
func (w *warmStreams) open(fill sessions.WarmFill) *sessions.WarmStream {
	stream := sessions.NewWarmStream(fill)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.streams == nil {
		w.streams = make(map[*sessions.WarmStream]bool)
	}
	w.streams[stream] = true
	return stream
}

// Close removes an ended stream from monitoring.
func (w *warmStreams) close(stream *sessions.WarmStream) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.streams, stream)
}

// Report snapshots the active warm streams.
func (w *warmStreams) report() []sessions.WarmStreamReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]sessions.WarmStreamReport, 0, len(w.streams))
	for stream := range w.streams {
		out = append(out, stream.Report())
	}
	slices.SortFunc(out, func(a, b sessions.WarmStreamReport) int { return a.OpenedAt.Compare(b.OpenedAt) })
	return out
}
