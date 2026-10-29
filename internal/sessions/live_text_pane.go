package sessions

import (
	"crypto/sha256"
	"encoding/json"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dkroshow/nano/internal/threadindex"
)

// liveTextPane is one open pane: one profile of one thread on one stream.
type liveTextPane struct {
	query threadindex.TranscriptQuery
	// rowsOnly marks a stream of rows, never live text: an agent's, or a warm
	// stream's.
	rowsOnly bool
	// warm marks a warm-stream pane, a thread whose saved rows the browser
	// keeps ready (warm_transcripts.go). It rides a pane on screen's reads,
	// otherwise reads on its own coalesced schedule, and tags its messages
	// with ref. reads counts its stream's page reads.
	warm  bool
	ref   string
	reads *atomic.Int64
	// A warm pane's window: the fill rule its rows are chosen by, and the
	// rows the browser already holds, used by its first read and then
	// dropped (warm_transcripts.go).
	fill   WarmFill
	marker *WarmMarker

	// The stream this pane writes to; a warm stream's panes share one.
	*liveTextQueue
	closed bool // guarded by the queue's mutex

	// Owned by the loop goroutine.
	key    string
	rows   map[string]liveRowState
	maxSeq int64
	sent   liveSent
}

// liveTextQueue is one stream's unsent messages and its one backlog budget.
type liveTextQueue struct {
	mu     sync.Mutex
	queue  []liveTextQueued
	bytes  int
	limit  int // counted bytes that end the stream
	closed bool
	ready  chan struct{}
	done   chan struct{}
}

type liveTextQueued struct {
	pane *liveTextPane
	raw  []byte
	// counted marks bytes the backlog budget holds: everything but a pane
	// stream's tail.
	counted bool
}

// liveSent is what a pane last received of the live-text region.
type liveSent struct {
	shape string
	texts map[string]string
}

type liveRowState struct {
	seq    int64
	digest [sha256.Size]byte
}

func newLiveTextQueue() *liveTextQueue {
	return newLiveTextQueueWithBacklog(liveTextPaneBacklog)
}

func newLiveTextQueueWithBacklog(limit int) *liveTextQueue {
	return &liveTextQueue{limit: limit, ready: make(chan struct{}, 1), done: make(chan struct{})}
}

func newLiveTextPane(query threadindex.TranscriptQuery) *liveTextPane {
	query.Before, query.BeforeNativeTurn, query.BeforeTurn, query.TurnLimit = "", "", 0, 0
	return &liveTextPane{query: query, liveTextQueue: newLiveTextQueue()}
}

func (p *liveTextPane) send(message LiveTextMessage) {
	message.Ref = p.ref
	if p.query.View == threadindex.DefaultThreadTranscriptView {
		message.Rows, message.Turns = enhancedRows(message.Rows), enhancedTurns(message.Turns)
	}
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	q := p.liveTextQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.closed || q.closed {
		return
	}
	tail := message.Type == "tail"
	if tail {
		// A tail replaces everything its pane queued before it.
		p.dropQueuedLocked()
	}
	// A pane stream's tail must always land, so only the changes queued after
	// it count as backlog. A warm stream counts its tails too: they are
	// bounded, and the stream can drop them.
	q.push(liveTextQueued{pane: p, raw: raw, counted: !tail || p.warm})
}

// push queues one message, or ends the stream once the counted backlog
// would pass its budget; the browser reconnects to fresh tails.
func (q *liveTextQueue) push(m liveTextQueued) {
	if m.counted && q.bytes+len(m.raw) > q.limit {
		q.queue = nil
		q.closeLocked()
		return
	}
	if m.counted {
		q.bytes += len(m.raw)
	}
	q.queue = append(q.queue, m)
	q.signalLocked()
}

// fail ends a pane's stream with one error message, which the browser shows
// instead of reconnecting. A warm stream carries on without that thread.
func (p *liveTextPane) fail(message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && !p.liveTextQueue.closed {
		p.failLocked(message)
	}
}

func (p *liveTextPane) failLocked(message string) {
	raw, _ := json.Marshal(LiveTextMessage{Type: "error", Ref: p.ref, Error: message})
	q := p.liveTextQueue
	if p.warm {
		p.dropQueuedLocked()
		p.closed = true
		q.push(liveTextQueued{pane: p, raw: raw, counted: true})
		return
	}
	q.queue = []liveTextQueued{{pane: p, raw: raw}}
	q.closeLocked()
}

// close ends the pane: its queued messages go unsent, and a pane's own
// stream ends with it.
func (p *liveTextPane) close() {
	q := p.liveTextQueue
	q.mu.Lock()
	defer q.mu.Unlock()
	p.dropQueuedLocked()
	p.closed = true
	if !p.warm {
		q.closeLocked()
	}
}

func (p *liveTextPane) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed || p.liveTextQueue.closed
}

func (p *liveTextPane) dropQueuedLocked() {
	q := p.liveTextQueue
	q.queue = slices.DeleteFunc(q.queue, func(m liveTextQueued) bool {
		if m.pane == p && m.counted {
			q.bytes -= len(m.raw)
		}
		return m.pane == p
	})
}

func (q *liveTextQueue) take() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([][]byte, 0, len(q.queue))
	for _, m := range q.queue {
		out = append(out, m.raw)
	}
	q.queue, q.bytes = nil, 0
	return out
}

func (q *liveTextQueue) signalLocked() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *liveTextQueue) closeLocked() {
	if !q.closed {
		q.closed = true
		close(q.done)
	}
}

type LiveTextMessage struct {
	Type string `json:"type"`
	// Ref names the thread of a warm stream's message; Refs, on a warm
	// stream's set message, every thread it now carries.
	Ref        string                            `json:"ref,omitempty"`
	Refs       []string                          `json:"refs,omitempty"`
	ThreadID   string                            `json:"threadId,omitempty"`
	Key        string                            `json:"key,omitempty"`
	Rows       []threadindex.TranscriptItem      `json:"rows,omitempty"`
	Removed    []string                          `json:"removed,omitempty"`
	Turns      []threadindex.TranscriptTurn      `json:"turns,omitempty"`
	Page       *threadindex.TranscriptPageInfo   `json:"page,omitempty"`
	Projection *threadindex.TranscriptProjection `json:"projection,omitempty"`
	Live       *[]liveTextEntry                  `json:"live,omitempty"`
	// Saved names the live text this message removes because its rows arrived.
	Saved  []string        `json:"saved,omitempty"`
	ID     string          `json:"id,omitempty"`
	Append string          `json:"append,omitempty"`
	Error  string          `json:"error,omitempty"`
	Status *liveTextStatus `json:"status,omitempty"`
}

// enhancedRows and enhancedTurns are what an Enhanced pane receives: the
// fields the Enhanced view draws. The developer view's raw provider copy,
// tags, provenance and turn config stay on dev panes and the HTTP API; on
// an Enhanced stream they were over a quarter of every tail.
func enhancedRows(rows []threadindex.TranscriptItem) []threadindex.TranscriptItem {
	if rows == nil {
		return nil
	}
	out := make([]threadindex.TranscriptItem, len(rows))
	for i, row := range rows {
		row.Detail, row.FilterTags, row.Synthetic, row.ToolName, row.CallID = nil, nil, false, "", ""
		out[i] = row
	}
	return out
}

func enhancedTurns(turns []threadindex.TranscriptTurn) []threadindex.TranscriptTurn {
	if turns == nil {
		return nil
	}
	out := make([]threadindex.TranscriptTurn, len(turns))
	for i, turn := range turns {
		turn.Config, turn.NativeTurnID, turn.FinalitySource, turn.FinalityConfidence, turn.CompletionEvidence = nil, "", "", "", ""
		out[i] = turn
	}
	return out
}

// tail sends a pane its newest page and the live text, as one message the
// browser swaps in one render.
func (l *LiveTextLoop) tail(pane *liveTextPane, response livePage, saved map[string]bool) {
	// Named before the pane's record of shown text is replaced below. A tail
	// also names every prompt whose saved row is in its page: a browser that
	// was away while a send of its own was saved gets the row and that word in
	// one message, so it never shows both, nor neither.
	shownSaved := pane.shown(saved)
	inPage, err := l.promptsSavedIn(response.Items)
	if err != nil {
		log.Printf("live text saved prompts %s: %v", l.hostedID(), err)
		pane.fail("the saved transcript could not be read")
		delete(l.panes, pane)
		return
	}
	for _, id := range inPage {
		if !slices.Contains(shownSaved, id) {
			shownSaved = append(shownSaved, id)
		}
	}
	sort.Strings(shownSaved)
	live := l.view(pane)
	if pane.warm {
		response = pane.window(response)
	}
	marker := pane.marker
	pane.marker = nil
	pane.key = response.Projection.Key
	pane.rows = make(map[string]liveRowState, len(response.Items))
	pane.maxSeq = 0
	for i, item := range response.Items {
		pane.rows[item.ID] = liveRowState{seq: item.ProjectedSeq, digest: response.digests[i]}
		pane.maxSeq = max(pane.maxSeq, item.ProjectedSeq)
	}
	pane.sent = sentFor(live)
	page, projection := response.Page, response.Projection
	if marker != nil && marker.Key == pane.key {
		// The browser holds this projection's rows first..last and says what
		// they are (a digest of their ids, sequences, lengths and truncation).
		// When the window's rows in that range digest the same, it gets only
		// the window's rows outside the range, as an update, and trims to the
		// window; a row changed or removed in place while it was away makes
		// the digests differ, and it gets a tail.
		var held, rows []threadindex.TranscriptItem
		for _, item := range response.Items {
			if item.ProjectedSeq < marker.First || item.ProjectedSeq > marker.Last {
				rows = append(rows, item)
			} else {
				held = append(held, item)
			}
		}
		if warmHeldDigest(held) == marker.Digest {
			pane.send(LiveTextMessage{Type: "update", ThreadID: response.ThreadID, Key: pane.key, Rows: nonNilItems(rows),
				Turns: response.Turns, Page: &page, Projection: &projection})
			return
		}
	}
	message := LiveTextMessage{Type: "tail", ThreadID: response.ThreadID, Key: pane.key, Rows: nonNilItems(response.Items),
		Turns: response.Turns, Page: &page, Projection: &projection}
	if !pane.rowsOnly {
		message.Live, message.Saved = &live, shownSaved
	}
	pane.send(message)
}

// window is the page a warm pane sends: the newest rows that fill the
// browser's window (warm_fill.go) and the turns they are in. The page is
// shared with other panes, so nothing is changed in place.
func (p *liveTextPane) window(response livePage) livePage {
	keep := p.fill.count(response.Items)
	if keep >= len(response.Items) {
		return response
	}
	cut := len(response.Items) - keep
	response.Items, response.digests = response.Items[cut:], response.digests[cut:]
	response.Page.StartSeq, response.Page.HasMoreBefore = response.Items[0].ProjectedSeq, true
	inTurns := make(map[string]bool, keep)
	for _, item := range response.Items {
		inTurns[item.TurnID] = true
	}
	turns := make([]threadindex.TranscriptTurn, 0, len(response.Turns))
	for _, turn := range response.Turns {
		if inTurns[turn.ID] {
			turns = append(turns, turn)
		}
	}
	response.Turns = turns
	return response
}

// view is the live text a pane shows: all of it, or none on an agent stream.
func (l *LiveTextLoop) view(pane *liveTextPane) []liveTextEntry {
	if pane.rowsOnly {
		return nil
	}
	return l.visible()
}

// needsTail reports that a read replaces the pane rather than patching it: a
// rebuild, or more new rows than one page holds.
func (p *liveTextPane) needsTail(response threadindex.TranscriptResponse) bool {
	gap := len(response.Items) > 0 && response.Page.HasMoreBefore && p.maxSeq > 0 &&
		response.Items[0].ProjectedSeq > p.maxSeq
	return response.Projection.Key != p.key || gap
}

// shown names the live text this pane was showing that a read found saved,
// and every prompt a read found saved: the browser shows its own send until
// the stream lists it or names it saved, even one Nano never listed live.
func (p *liveTextPane) shown(saved map[string]bool) []string {
	var ids []string
	for id := range saved {
		if _, sent := p.sent.texts[id]; sent || strings.HasPrefix(id, "prompt:") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (l *LiveTextLoop) updatePane(pane *liveTextPane, response livePage, saved map[string]bool) {
	if pane.isClosed() {
		delete(l.panes, pane)
		return
	}
	if pane.warm {
		response = pane.window(response)
	}
	if pane.needsTail(response.TranscriptResponse) {
		l.tail(pane, response, saved)
		return
	}
	live := l.view(pane)
	var rows []threadindex.TranscriptItem
	start := int64(0)
	if len(response.Items) > 0 {
		start = response.Items[0].ProjectedSeq
	}
	present := make(map[string]bool, len(response.Items))
	for i, item := range response.Items {
		present[item.ID] = true
		digest := response.digests[i]
		if state, ok := pane.rows[item.ID]; !ok || state.digest != digest {
			rows = append(rows, item)
		}
		pane.rows[item.ID] = liveRowState{seq: item.ProjectedSeq, digest: digest}
		pane.maxSeq = max(pane.maxSeq, item.ProjectedSeq)
	}
	var removed []string
	for id, state := range pane.rows {
		if present[id] {
			continue
		}
		if state.seq >= start {
			removed = append(removed, id) // it left this pane's profile
		}
		delete(pane.rows, id)
	}
	sort.Strings(removed)
	liveChanged := !pane.rowsOnly && !pane.sent.same(live, true)
	var shownSaved []string
	if !pane.rowsOnly {
		shownSaved = pane.shown(saved)
	}
	if len(rows) == 0 && len(removed) == 0 && !liveChanged && len(shownSaved) == 0 {
		return
	}
	page := response.Page
	message := LiveTextMessage{Type: "update", Key: pane.key, Rows: rows, Removed: removed, Turns: response.Turns, Page: &page}
	if liveChanged || len(shownSaved) > 0 {
		message.Live, message.Saved = &live, shownSaved
		pane.sent = sentFor(live)
	}
	pane.send(message)
}

// sendLiveChanges streams text growth as appends, and any other live-text
// change as the whole region.
func (l *LiveTextLoop) sendLiveChanges() {
	for pane := range l.panes {
		if pane.isClosed() {
			delete(l.panes, pane)
			continue
		}
		if pane.rowsOnly {
			continue
		}
		live := l.view(pane)
		if !pane.sent.same(live, false) {
			pane.send(LiveTextMessage{Type: "live", Live: &live})
			pane.sent = sentFor(live)
			continue
		}
		for _, entry := range live {
			before := pane.sent.texts[entry.ID]
			if entry.Text == before {
				continue
			}
			if !strings.HasPrefix(entry.Text, before) {
				pane.send(LiveTextMessage{Type: "live", Live: &live})
				pane.sent = sentFor(live)
				break
			}
			pane.send(LiveTextMessage{Type: "append", ID: entry.ID, Append: entry.Text[len(before):]})
			pane.sent.texts[entry.ID] = entry.Text
		}
	}
}

func sentFor(live []liveTextEntry) liveSent {
	sent := liveSent{texts: make(map[string]string, len(live))}
	var shape strings.Builder
	for _, entry := range live {
		sent.texts[entry.ID] = entry.Text
		shape.WriteString(entry.ID + "\x00" + entry.State + "\x00" + entry.Label + "\x00")
	}
	sent.shape = shape.String()
	return sent
}

// same reports the same entries, order, states and labels; withText also compares
// every entry's text.
func (s liveSent) same(live []liveTextEntry, withText bool) bool {
	if s.texts == nil || sentFor(live).shape != s.shape {
		return false
	}
	if withText {
		for _, entry := range live {
			if s.texts[entry.ID] != entry.Text {
				return false
			}
		}
	}
	return true
}

func nonNilItems(items []threadindex.TranscriptItem) []threadindex.TranscriptItem {
	if items == nil {
		return []threadindex.TranscriptItem{}
	}
	return items
}
