package app

// fleet_changes.go — the fleet transition feed, so a consumer can learn every
// label change since a bookmark without polling the thread list. Every
// change to the materialized sidebar re-evaluates its rows
// with the catalog reads' one turn-state function and appends changed labels
// to a small in-memory ring. The feed reads no SQL of its own.
//
// The primary contract is bookmark pull: a caller presents its bookmark, gets
// every transition since plus a new one, and a reconnect is just another ask.
// A bookmark this process cannot honour — from an earlier process, or older
// than the ring retains — is answered with an explicit re-snapshot signal,
// never a silent gap. The same transitions mirror onto the catalog SSE stream
// for always-connected consumers; that mirror is best-effort with no replay.
//
// Everything here is in-memory and loss-tolerant on purpose: fleet state is
// never stored, and a consumer that loses the feed re-snapshots.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sidebar"
)

const (
	// fleetChangeRingSize bounds the transition log. A consumer polling at any
	// sane interval stays well inside it; one that falls further behind is told
	// to re-snapshot.
	fleetChangeRingSize = 512
	// fleetSubscriberBuffer is one SSE subscriber's slack before its mirror
	// starts dropping. The mirror is a courtesy; the pull feed is the contract.
	fleetSubscriberBuffer = 32
	// fleetEpochBytes sizes the per-process bookmark epoch.
	fleetEpochBytes = 8
)

// Re-snapshot reasons. Each says exactly why the caller's cursor could not be
// honoured, so a consumer can log the difference between "first call" and
// "you fell behind".
const (
	fleetResnapshotNoBookmark = "no_bookmark"
	fleetResnapshotUnknown    = "unknown_bookmark"
	fleetResnapshotExpired    = "bookmark_expired"
)

type fleetTransition struct {
	Seq      uint64 `json:"seq"`
	ThreadID string `json:"threadId"`
	From     string `json:"from"`
	To       string `json:"to"`
	At       string `json:"at"`
}

type fleetChangesResponse struct {
	Transitions []fleetTransition `json:"transitions"`
	Bookmark    string            `json:"bookmark"`
	// Resnapshot tells the caller its cursor bought it nothing and it must
	// re-read /api/v1/threads before trusting the feed again.
	Resnapshot bool   `json:"resnapshot"`
	Reason     string `json:"reason,omitempty"`
}

// fleetChangeLog is the ring. Sequence numbers are 1-based and monotonic within
// one process; the epoch makes a bookmark from a previous process detectably
// foreign rather than accidentally valid.
type fleetChangeLog struct {
	mu    sync.Mutex
	epoch string
	ring  []fleetTransition
	next  uint64
}

func newFleetChangeLog() *fleetChangeLog {
	epoch := make([]byte, fleetEpochBytes)
	if _, err := rand.Read(epoch); err != nil {
		// A process that cannot read randomness still needs a distinguishable
		// epoch; start time is unique enough for a cursor's purpose.
		return &fleetChangeLog{epoch: strconv.FormatInt(time.Now().UnixNano(), 36),
			ring: make([]fleetTransition, fleetChangeRingSize), next: 1}
	}
	return &fleetChangeLog{epoch: hex.EncodeToString(epoch),
		ring: make([]fleetTransition, fleetChangeRingSize), next: 1}
}

// append stamps and stores one transition, returning it with its sequence.
func (l *fleetChangeLog) append(transition fleetTransition) fleetTransition {
	l.mu.Lock()
	defer l.mu.Unlock()
	transition.Seq = l.next
	l.ring[l.slot(l.next)] = transition
	l.next++
	return transition
}

// since answers the bookmark pull: every transition after the caller's cursor
// plus the cursor to present next time.
func (l *fleetChangeLog) since(bookmark string) fleetChangesResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	head := l.next - 1
	response := fleetChangesResponse{
		Transitions: []fleetTransition{},
		Bookmark:    fmt.Sprintf("%s.%d", l.epoch, head),
	}
	bookmark = strings.TrimSpace(bookmark)
	if bookmark == "" {
		response.Resnapshot, response.Reason = true, fleetResnapshotNoBookmark
		return response
	}
	seq, ok := l.parse(bookmark)
	if !ok || seq > head {
		response.Resnapshot, response.Reason = true, fleetResnapshotUnknown
		return response
	}
	if seq+1 < l.oldest() {
		response.Resnapshot, response.Reason = true, fleetResnapshotExpired
		return response
	}
	for cursor := seq + 1; cursor <= head; cursor++ {
		response.Transitions = append(response.Transitions, l.ring[l.slot(cursor)])
	}
	return response
}

// parse splits a bookmark and rejects one minted by another process.
func (l *fleetChangeLog) parse(bookmark string) (uint64, bool) {
	epoch, raw, found := strings.Cut(bookmark, ".")
	if !found || epoch != l.epoch {
		return 0, false
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	return seq, err == nil
}

// oldest is the lowest sequence the ring still holds.
func (l *fleetChangeLog) oldest() uint64 {
	if l.next <= uint64(len(l.ring)) {
		return 1
	}
	return l.next - uint64(len(l.ring))
}

func (l *fleetChangeLog) slot(seq uint64) int { return int((seq - 1) % uint64(len(l.ring))) }

// fleetHub mirrors transitions onto the catalog SSE stream. Unlike catalogHub
// this is a log rather than last-value-wins state, so each subscriber gets a
// small buffer and a full one drops — documented as best-effort with no replay,
// because the bookmark feed is what guarantees delivery.
type fleetHub struct {
	mu   sync.Mutex
	subs map[chan fleetTransition]struct{}
}

func newFleetHub() *fleetHub {
	return &fleetHub{subs: make(map[chan fleetTransition]struct{})}
}

func (h *fleetHub) subscribe() chan fleetTransition {
	ch := make(chan fleetTransition, fleetSubscriberBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *fleetHub) unsubscribe(ch chan fleetTransition) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

func (h *fleetHub) notify(transition fleetTransition) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- transition:
		default: // slow subscriber; the pull feed still has it
		}
	}
}

type fleetWatcher struct {
	changes *fleetChangeLog
	hub     *fleetHub

	mu       sync.Mutex
	observed map[string]string
	// seeded suppresses transitions for the first sync: every thread is new to
	// a fresh process, and a burst of ""→label entries is noise, not news.
	seeded bool
	// read keeps each row's label with the saved message it came from. Only an
	// unanswered prompt's label moves with the clock, so any other unchanged
	// message is not scanned again; a re-attach burst re-labels every row on
	// every change.
	read map[string]fleetCourtRead
}

type fleetCourtRead struct {
	court fleetCourtMessage
	label string
}

func newFleetWatcher() *fleetWatcher {
	return &fleetWatcher{
		changes: newFleetChangeLog(), hub: newFleetHub(),
		observed: map[string]string{},
	}
}

// startFleetWatcher follows the materialized sidebar: every change to its rows
// re-evaluates their labels in memory.
func startFleetWatcher(ctx context.Context, sidebar *sidebarMaterializedState) *fleetWatcher {
	watcher := newFleetWatcher()
	go watcher.run(ctx, sidebar)
	return watcher
}

func (w *fleetWatcher) run(ctx context.Context, sidebar *sidebarMaterializedState) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sidebar.rowsChanged:
			if threads, ready := sidebar.rows(); ready {
				w.resync(threads, time.Now())
			}
		}
	}
}

// resync labels the materialized rows at now and publishes the labels that
// moved. Re-evaluating every row, not only the changed ones, is what lets an
// abandoned prompt leave working once it passes the bound.
func (w *fleetWatcher) resync(threads []sidebar.Thread, now time.Time) {
	w.mu.Lock()
	previous, seeded := w.observed, w.seeded
	observed := make(map[string]string, len(threads))
	read := make(map[string]fleetCourtRead, len(threads))
	var transitions []fleetTransition
	for _, thread := range threads {
		id := thread.ID
		held := nanoHolds(thread)
		var label string
		if prior, ok := w.read[id]; ok && !held && prior.court == thread.Court {
			label = prior.label
		} else {
			label = threadFleetState(thread, thread.Court, now).Label
		}
		if !held && thread.Court.ItemType != fleetUserMessageType {
			read[id] = fleetCourtRead{court: thread.Court, label: label}
		}
		observed[id] = label
		if before := previous[id]; seeded && before != label {
			transitions = append(transitions, fleetTransition{ThreadID: id, From: before, To: label})
		}
	}
	w.observed, w.seeded, w.read = observed, true, read
	w.mu.Unlock()
	w.publish(transitions, now)
}

// publish records transitions in the ring and mirrors them onto the SSE stream.
// Map iteration order is undefined, so one batch is ordered by thread id — a
// consumer's sequence numbers should not depend on Go's hash seed.
func (w *fleetWatcher) publish(transitions []fleetTransition, now time.Time) {
	sort.Slice(transitions, func(i, j int) bool { return transitions[i].ThreadID < transitions[j].ThreadID })
	stamp := now.UTC().Format(time.RFC3339)
	for _, transition := range transitions {
		transition.At = stamp
		w.hub.notify(w.changes.append(transition))
	}
}

func (s *server) handleFleetChanges(w http.ResponseWriter, r *http.Request) {
	if s.fleet == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "fleet_unavailable", "Fleet transition feed unavailable")
		return
	}
	writeCatalogJSON(w, http.StatusOK, s.fleet.changes.since(r.URL.Query().Get("since")))
}
