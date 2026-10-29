package mail

import "sync"

// Delta is the SSE payload: the complete current counts for each changed
// recipient plus the global summary, so a subscriber can patch its state
// without a follow-up fetch. Conflation merges deltas key-by-key — counts are
// last-value-wins state, not a log.
type Delta struct {
	Counts  map[string]Counts `json:"counts"`
	Summary Counts            `json:"summary"`
}

// Hub mirrors runtimeHub: single-slot subscribers, merge-on-conflate so a
// slow subscriber still ends with every thread's latest counts.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Delta]struct{}
}

// NewHub constructs the mailbox-count change publisher.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan Delta]struct{})}
}

// Subscribe registers a consumer for mailbox-count changes.
func (h *Hub) Subscribe() chan Delta {
	ch := make(chan Delta, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe releases a mailbox-count consumer.
func (h *Hub) Unsubscribe(ch chan Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// Notify publishes current counts, merging any pending recipient states.
func (h *Hub) Notify(delta Delta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case pending := <-ch:
			merged := Delta{Counts: map[string]Counts{}, Summary: delta.Summary}
			for id, counts := range pending.Counts {
				merged.Counts[id] = counts
			}
			for id, counts := range delta.Counts {
				merged.Counts[id] = counts
			}
			delta = merged
		default:
		}
		select {
		case ch <- delta:
		default:
		}
	}
}
