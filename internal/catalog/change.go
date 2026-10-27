package catalog

import "sync"

const maxChangeThreadIDs = 32

// Change tells a surface whether named rows can be repaired or the complete
// materialized window must be replaced.
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
	if merged.Refresh {
		return merged
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
