package catalog

import "sync"

const maxChangeThreadIDs = 32

// Change names the threads a catalog commit changed. Refresh tells a surface
// that lists a window of threads to read that window again: the commit added,
// removed or re-parented threads, or changed more than a change can name. A
// Refresh that names threads names every thread it changed, so a surface that
// can place new and moved rows itself (the materialized sidebar) repairs them;
// one that names none may have changed anything.
type Change struct {
	Version   uint64
	ThreadIDs []string
	Refresh   bool
}

// Hub fans index changes out without allowing a slow reader to block index
// maintenance. Each reader retains one conflated change.
type Hub struct {
	mu     sync.Mutex
	subs   map[chan Change]struct{}
	closed bool
}

func NewHub() *Hub { return &Hub{subs: make(map[chan Change]struct{})} }

func (h *Hub) Subscribe() chan Change {
	ch := make(chan Change, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch
	}
	h.subs[ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(ch chan Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *Hub) Notify(change Change) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		pending := Change{}
		select {
		case pending = <-ch:
		default:
		}
		merged := MergeChanges(pending, change)
		select {
		case ch <- merged:
		default:
		}
	}
}

func MergeChanges(left, right Change) Change {
	merged := Change{Version: max(left.Version, right.Version), Refresh: left.Refresh || right.Refresh}
	if left.Refresh && len(left.ThreadIDs) == 0 || right.Refresh && len(right.ThreadIDs) == 0 {
		return Change{Version: merged.Version, Refresh: true}
	}
	seen := make(map[string]struct{}, len(left.ThreadIDs)+len(right.ThreadIDs))
	for _, ids := range [][]string{left.ThreadIDs, right.ThreadIDs} {
		for _, id := range ids {
			if id == "" {
				continue
			}
			seen[id] = struct{}{}
			if len(seen) > maxChangeThreadIDs {
				return Change{Version: merged.Version, Refresh: true}
			}
		}
	}
	merged.ThreadIDs = make([]string, 0, len(seen))
	for id := range seen {
		merged.ThreadIDs = append(merged.ThreadIDs, id)
	}
	if len(merged.ThreadIDs) == 0 {
		merged.Refresh = true
	}
	return merged
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}
