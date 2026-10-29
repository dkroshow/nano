package sidebar

import (
	"sort"
	"strconv"
	"sync"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/observability"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// RuntimeInfo is the volatile per-thread runtime overlay attached to catalog
// responses: `attached` when a nano session hosts the thread (with sessionId,
// status starting|ready, turnActive), `detached` when no session does but a
// durable attachment record survives — a thread nano can re-attach in one
// click. Absent field = neither. The detached shape carries no session fields,
// so they are omitempty rather than serialized as empty lies.
// RuntimeInfo is the live or detached overlay attached to a saved thread.
type RuntimeInfo struct {
	Attached   bool   `json:"attached"`
	Detached   bool   `json:"detached"`
	SessionID  string `json:"sessionId,omitempty"`
	Status     string `json:"status,omitempty"`
	TurnActive bool   `json:"turnActive,omitempty"`
	// ThreadStatus is the session's turn state, the same value the pane
	// reads. It rides every runtime delta, so the row it patches stays in step.
	ThreadStatus string `json:"threadStatus,omitempty"`
	// TurnStartedAtMs is the working indicator's anchor — immutable, ms epoch,
	// absent when there is no turn or no evidence of when it began (D2/D4/D8).
	// It travels in the window refetch the payload-free runtime SSE nudge
	// triggers; the nudge itself stays payload-free.
	TurnStartedAtMs int64 `json:"turnStartedAtMs,omitempty"`
	// CompactingSinceMs is when the provider began compacting the turn's
	// context; absent while it is not. The row shows a marker meanwhile.
	CompactingSinceMs int64 `json:"compactingSinceMs,omitempty"`
	// LastTurnEndedAt is the server wall-clock instant of the most recent
	// hosted turn completion. It is process-local and absent for detached rows.
	LastTurnEndedAt string `json:"lastTurnEndedAt,omitempty"`
	// Waiting means a parked interaction is holding the turn: the row shows
	// waiting rather than a ticking elapsed (D15 — waiting is not working).
	// Without this field the rail has no path at all from pending interactions,
	// which is why park/claim/drain now nudge the runtime stream.
	Waiting bool `json:"waiting,omitempty"`
	// NeedsAttention is durable execution evidence that a background session's
	// latest turn failed. Compact clients promote it even when the group is shut.
	NeedsAttention bool `json:"needsAttention,omitempty"`
	// PresentationMode is UI-only session metadata. Background sessions remain
	// fully hosted and inspectable; compact clients may group or suppress them.
	PresentationMode string `json:"presentationMode,omitempty"`
	// ThreadDiagnostics is the thread's bounded list of delivery and
	// session facts (thread_diagnostics.go).
	ThreadDiagnostics []observability.ThreadDiagnostic `json:"threadDiagnostics,omitempty"`
	// Error, MetadataDegraded and MetadataRevision let a browser keep its
	// session list current from runtime events alone: a spawn failure, an
	// ingestion warning, and a canonical metadata change it should re-read.
	// MetadataRevision is the latest revision Nano has published since boot,
	// absent when it has published none for the thread.
	Error            string `json:"error,omitempty"`
	MetadataDegraded bool   `json:"metadataDegraded,omitempty"`
	MetadataRevision int64  `json:"metadataRevision,omitempty"`
}

// RuntimeRow names one changed provider-native overlay.
type RuntimeRow struct {
	Provider string       `json:"provider"`
	NativeID string       `json:"nativeId"`
	Runtime  *RuntimeInfo `json:"runtime"`
}

// RuntimeDelta carries the overlay of each thread whose runtime changed since
// the last snapshot, and a nil overlay for each thread Nano no longer holds;
// threads it does not name are unchanged. The first snapshot names every
// thread. The one-slot hub merges a pending delta into the next by thread, so
// a subscriber that misses an intermediate turnActive flip still receives the
// latest truth, including removals. Refresh is reserved for identity and
// attachment changes; ordinary turn-state changes patch rows in place. Rows is
// always present on the wire: a browser reads a missing rows field as "read
// the whole list", which an unchanged snapshot must never ask for.
type RuntimeDelta struct {
	Rows []RuntimeRow `json:"rows"`
	// Refresh asks browsers to re-read the session list: something changed
	// that this delta does not carry (a metadata or star change, a boot).
	Refresh bool `json:"refresh,omitempty"`
	// A change to the live session set or to an attachment travels here
	// instead of as Refresh: the list rows of the sessions that joined or
	// changed, the ids of those that left, and likewise the detached rows
	// that joined or changed and the keys ("provider:nativeThreadId") of those
	// that left. A browser on a slow link then never re-reads every session
	// for one, and a restart's re-attach, which drains the detached list one
	// thread at a time, costs a few rows per change instead of the list.
	Sessions        []ListedSession       `json:"sessions,omitempty"`
	RemovedSessions []string              `json:"removedSessions,omitempty"`
	DetachedRows    []DetachedSessionView `json:"detachedRows,omitempty"`
	RemovedDetached []string              `json:"removedDetached,omitempty"`
	// Seq is the delta's place in this process's runtime stream (the latest
	// one merged in), so a browser can name where it left off on reconnect.
	Seq uint64 `json:"seq,omitempty"`
	// Rebuild tells the sidebar materializer the held set changed (Refresh
	// does too); SidebarCurrent says a star update already applied it.
	Rebuild        bool `json:"-"`
	SidebarCurrent bool `json:"-"`
}

// runtimeChangeLimit bounds the rows of any kind one delta carries. A larger
// change is sent as one Refresh, a single list read, rather than as a burst
// of rows.
const runtimeChangeLimit = 64

// bounded is the delta a subscriber receives. Past runtimeChangeLimit rows,
// pending merges included, it is a refresh marker with no rows: one list
// read and one sidebar Rebuild cover what the rows would have said.
func (d RuntimeDelta) bounded() RuntimeDelta {
	if len(d.Rows)+len(d.Sessions)+len(d.RemovedSessions)+len(d.DetachedRows)+len(d.RemovedDetached) <= runtimeChangeLimit {
		return d
	}
	return RuntimeDelta{Rows: []RuntimeRow{}, Refresh: true, Rebuild: true, Seq: d.Seq}
}

// RuntimeHub fans the compact runtime delta out to every open thread-index SSE
// subscriber. Like catalogHub, each subscriber holds a single-slot buffer, so
// a slow subscriber can never block session-land.
//
// Each delta takes the next seq of this process's generation, and the hub
// keeps the last runtimeReplayLimit of them. A browser that read the session
// list at a position subscribes from it and receives what it missed as one
// merged delta, so a page load reads the list once instead of again when its
// stream opens, and a short drop needs no read at all.
type RuntimeHub struct {
	mu         sync.Mutex
	subs       map[chan RuntimeDelta]struct{}
	generation string
	seq        uint64
	recent     []RuntimeDelta
}

const runtimeReplayLimit = 64

// RuntimePosition names a point in the runtime stream: the session list read
// reports it, and the browser's stream connect echoes it back.
type RuntimePosition struct {
	Generation string `json:"generation"`
	Seq        uint64 `json:"seq"`
}

// NewRuntimeHub constructs the runtime replay owner.
func NewRuntimeHub() *RuntimeHub {
	return &RuntimeHub{
		subs:       make(map[chan RuntimeDelta]struct{}),
		generation: strconv.FormatInt(time.Now().UnixNano(), 36),
	}
}

// Position snapshots the current replay point.
func (h *RuntimeHub) Position() RuntimePosition {
	h.mu.Lock()
	defer h.mu.Unlock()
	return RuntimePosition{Generation: h.generation, Seq: h.seq}
}

// Subscribe registers a consumer for future runtime changes.
func (h *RuntimeHub) Subscribe() chan RuntimeDelta {
	ch := make(chan RuntimeDelta, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// subscribeFrom subscribes a browser that last saw from. What it missed waits
// in the channel as one delta; a position this hub cannot replay (another
// process's, or older than the deltas kept) gets a refresh, which reads the
// list. A current position gets nothing.
// SubscribeFrom registers a consumer and replays changes after its saved position.
func (h *RuntimeHub) SubscribeFrom(from RuntimePosition) chan RuntimeDelta {
	ch := make(chan RuntimeDelta, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	missed := h.seq - from.Seq
	switch {
	case from.Generation != h.generation || from.Seq > h.seq || missed > uint64(len(h.recent)):
		ch <- RuntimeDelta{Rows: []RuntimeRow{}, Refresh: true, Seq: h.seq}
	case missed > 0:
		replay := h.recent[uint64(len(h.recent))-missed:]
		merged := replay[0]
		for _, delta := range replay[1:] {
			merged = mergeRuntimeDeltas(merged, delta)
		}
		ch <- merged.bounded()
	}
	return ch
}

// Unsubscribe releases a runtime change consumer.
func (h *RuntimeHub) Unsubscribe(ch chan RuntimeDelta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// Notify publishes one change and retains it for replay.
func (h *RuntimeHub) Notify(delta ...RuntimeDelta) {
	next := RuntimeDelta{}
	if len(delta) > 0 {
		next = delta[0]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	next.Seq = h.seq
	h.recent = append(h.recent, next)
	if len(h.recent) > runtimeReplayLimit {
		h.recent = h.recent[1:]
	}
	for ch := range h.subs {
		// Merge a pending delta into this one rather than drop it: a thread the
		// pending delta names and this one does not keeps its pending overlay,
		// and a thread both name takes the later one.
		merged := next
		select {
		case pending := <-ch:
			merged = mergeRuntimeDeltas(pending, next)
		default:
		}
		select {
		case ch <- merged.bounded():
		default:
		}
	}
}

// mergeSessionChanges carries both deltas' session and detached changes,
// later over earlier: an entry that left and came back is present, and one
// that came and left is gone.
func mergeSessionChanges(merged *RuntimeDelta, earlier, later RuntimeDelta) {
	rows := map[string]ListedSession{}
	removed := map[string]bool{}
	for _, delta := range []RuntimeDelta{earlier, later} {
		for _, id := range delta.RemovedSessions {
			delete(rows, id)
			removed[id] = true
		}
		for _, row := range delta.Sessions {
			rows[row.ID] = row
			delete(removed, row.ID)
		}
	}
	for _, row := range rows {
		merged.Sessions = append(merged.Sessions, row)
	}
	sort.Slice(merged.Sessions, func(i, j int) bool {
		return threadvalues.TimestampBefore(merged.Sessions[i].CreatedAt, merged.Sessions[j].CreatedAt)
	})
	for id := range removed {
		merged.RemovedSessions = append(merged.RemovedSessions, id)
	}
	sort.Strings(merged.RemovedSessions)
	detached := map[string]DetachedSessionView{}
	removedDetached := map[string]bool{}
	for _, delta := range []RuntimeDelta{earlier, later} {
		for _, key := range delta.RemovedDetached {
			delete(detached, key)
			removedDetached[key] = true
		}
		for _, row := range delta.DetachedRows {
			key := row.Provider + ":" + row.NativeThreadID
			detached[key] = row
			delete(removedDetached, key)
		}
	}
	for _, row := range detached {
		merged.DetachedRows = append(merged.DetachedRows, row)
	}
	sort.Slice(merged.DetachedRows, func(i, j int) bool {
		return merged.DetachedRows[i].Provider+":"+merged.DetachedRows[i].NativeThreadID < merged.DetachedRows[j].Provider+":"+merged.DetachedRows[j].NativeThreadID
	})
	for key := range removedDetached {
		merged.RemovedDetached = append(merged.RemovedDetached, key)
	}
	sort.Strings(merged.RemovedDetached)
}

// mergeRuntimeDeltas is the delta a subscriber reads in place of two: later's
// rows over earlier's by thread, in the sorted order the snapshot uses.
func mergeRuntimeDeltas(earlier, later RuntimeDelta) RuntimeDelta {
	byKey := make(map[hostedstore.AttachmentKey]RuntimeRow, len(earlier.Rows)+len(later.Rows))
	for _, row := range earlier.Rows {
		byKey[hostedstore.AttachmentKey{Provider: row.Provider, NativeThreadID: row.NativeID}] = row
	}
	for _, row := range later.Rows {
		byKey[hostedstore.AttachmentKey{Provider: row.Provider, NativeThreadID: row.NativeID}] = row
	}
	// A Rebuild still needed keeps that need through the merge; a later star
	// update that applied cleanly cannot mask it.
	needsRebuild := (earlier.Rebuild || earlier.Refresh) && !earlier.SidebarCurrent || (later.Rebuild || later.Refresh) && !later.SidebarCurrent
	merged := RuntimeDelta{
		Refresh: earlier.Refresh || later.Refresh,
		Rebuild: earlier.Rebuild || later.Rebuild, Seq: max(earlier.Seq, later.Seq),
	}
	merged.SidebarCurrent = (merged.Rebuild || merged.Refresh) && !needsRebuild
	mergeSessionChanges(&merged, earlier, later)
	if earlier.Rows != nil || later.Rows != nil {
		merged.Rows = make([]RuntimeRow, 0, len(byKey))
		for _, row := range byKey {
			merged.Rows = append(merged.Rows, row)
		}
		sort.Slice(merged.Rows, func(i, j int) bool {
			if merged.Rows[i].Provider != merged.Rows[j].Provider {
				return merged.Rows[i].Provider < merged.Rows[j].Provider
			}
			return merged.Rows[i].NativeID < merged.Rows[j].NativeID
		})
	}
	return merged
}
