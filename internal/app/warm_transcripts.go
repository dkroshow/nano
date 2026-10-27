package app

// warm_transcripts.go — the warm stream. One stream per browser page carries
// the saved Enhanced rows of the threads the page shows in its sidebar, so
// selecting one paints at once; the pane's own stream then takes over. The
// page names the set in the order it renders it, and the window it draws
// into (lines x cols); each thread gets the newest rows that fill that window
// (warm_fill.go). A page that still holds a thread's rows sends a marker for
// it and gets only the rows it lacks.
// Each thread is one rows-only pane on its loop (live_text.go): it reads with
// a pane on screen, or on its own coalesced schedule, and an idle thread
// costs nothing after its tail. The panes share one queue, so the stream has
// one backlog; threads join a few at a time, in set order, so the top of the
// sidebar is ready first and a connect burst stays small.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
)

const (
	// warmSetMax is how many threads one page may ask for; the first are
	// kept. A sidebar renders fewer rows than this.
	warmSetMax = 100
	// warmBudget caps the distinct threads warm across every stream, so
	// several pages warming different threads cannot multiply the work.
	warmBudget = 150
	// warmJoining is how many threads join one stream at once: the next
	// joins when one of them has sent its first message, or has waited
	// warmJoinWait without one (a projection still indexing sends nothing).
	warmJoining  = 4
	warmJoinWait = 2 * time.Second
	// warmStreamBacklog is a warm stream's queue limit. The threads joining
	// at once, each at the largest page the catalog keeps inline (40 rows of
	// 8 KiB), fit in half of it, so a connect burst never ends the stream.
	warmStreamBacklog = 4 << 20
	// warmPageRows is the page a warm pane reads: the same page a pane on
	// screen reads (TRANSCRIPT_LIMIT in web/src/transcript_query.js), so the
	// two share one read.
	warmPageRows = 40
)

// warmPaneQuery is the page query a warm pane reads: exactly the Enhanced
// pane's, so the loop keys both to one read.
func warmPaneQuery() transcriptListQuery {
	return transcriptListQuery{View: defaultThreadTranscriptView, HiddenSpecified: true,
		HiddenCategories: append([]string(nil), enhancedHiddenCategories...), Limit: warmPageRows}
}

// warmMarker is what a page holds of one thread: a projection key, the
// sequence range of its rows, and a digest of those rows (warmHeldDigest).
type warmMarker struct {
	key         string
	first, last int64
	digest      string
}

// warmHeldDigest names a run of rows as a browser holds them: every field
// the Enhanced view draws, for each row in order, through 64-bit FNV-1a. The
// browser computes the same over the rows it holds (warm_transcripts.js
// heldDigest: strings as they are, numbers in decimal, booleans as 1 or 0,
// an absent value as nothing), so any change to a held row made while the
// page was away is seen without sending the rows.
func warmHeldDigest(rows []transcriptItem) string {
	h := fnv.New64a()
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	for _, row := range rows {
		createdAt, originalBytes := "", ""
		if row.CreatedAt != nil {
			createdAt = *row.CreatedAt
		}
		if row.OriginalBytes != 0 {
			originalBytes = fmt.Sprint(row.OriginalBytes)
		}
		fmt.Fprintf(h, "%s\n%d\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%d\n%d\n%s\n%d\n%s\n",
			row.ID, row.ProjectedSeq, row.TurnID, row.NativeItemID, row.Type, row.Category, row.Role, row.Status,
			row.Text, createdAt, row.Phase, flag(row.IsFinalAnswer), flag(row.Sidechain), row.ContentRef,
			flag(row.ContentTruncated), originalBytes)
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// warmRequest is what a page asks for on its stream URL.
type warmRequest struct {
	set     []string
	fill    warmFill
	markers map[string]warmMarker
}

var errWarmMarkers = errors.New("markers must be a JSON object of thread ref to {key, first, last, digest}")

// warmRetryWait is how long a thread that failed (a read error, a page the
// stream could not take) stays out before the stream tries it again: long
// enough that a thread failing every time costs one read in that time.
const warmRetryWait = 5 * time.Second

func parseWarmRequest(r *http.Request) (warmRequest, error) {
	q := r.URL.Query()
	raw := q.Get("set")
	if raw == "" {
		raw = q.Get("hints") // a page from before the set, until it reloads
	}
	var set []string
	for _, ref := range strings.Split(raw, ",") {
		if ref = strings.TrimSpace(ref); ref != "" && !slices.Contains(set, ref) {
			set = append(set, ref)
		}
	}
	if len(set) > warmSetMax {
		set = set[:warmSetMax]
	}
	fill, err := parseWarmFill(q.Get("lines"), q.Get("cols"))
	if err != nil {
		return warmRequest{}, err
	}
	markers := make(map[string]warmMarker)
	if raw := q.Get("markers"); raw != "" {
		var wire map[string]struct {
			Key    string `json:"key"`
			First  int64  `json:"first"`
			Last   int64  `json:"last"`
			Digest string `json:"digest"`
		}
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			return warmRequest{}, errWarmMarkers
		}
		for ref, m := range wire {
			if m.Key != "" && m.First > 0 && m.Last >= m.First && m.Digest != "" {
				markers[ref] = warmMarker{key: m.Key, first: m.First, last: m.Last, digest: m.Digest}
			}
		}
	}
	return warmRequest{set: set, fill: fill, markers: markers}, nil
}

// warmTranscripts holds every open warm stream and the threads they warm.
type warmTranscripts struct {
	mu      sync.Mutex
	threads map[string]int // hosted thread id: warm streams holding it
	streams map[*warmStream]bool
}

// warmStream is one page's warm stream and its counters.
type warmStream struct {
	queue    *liveTextQueue
	openedAt time.Time
	fill     warmFill
	refs     atomic.Int64
	reads    atomic.Int64
	bytes    atomic.Int64
	// Tails sent: the connect burst, then one per join, rebuild or gap.
	tails     atomic.Int64
	tailBytes atomic.Int64
}

// warmStreamReport is one warm stream on GET /api/v1/monitor.
type warmStreamReport struct {
	OpenedAt  time.Time `json:"openedAt"`
	Lines     int       `json:"lines"`
	Cols      int       `json:"cols"`
	Refs      int64     `json:"refs"`
	Reads     int64     `json:"reads"`
	Bytes     int64     `json:"bytes"`
	Tails     int64     `json:"tails"`
	TailBytes int64     `json:"tailBytes"`
}

// admit counts a stream's hold on a thread. A thread already warm costs no
// budget; a new one is refused once the budget is spent.
func (w *warmTranscripts) admit(ref string) bool {
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

func (w *warmTranscripts) release(ref string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.threads[ref]--; w.threads[ref] <= 0 {
		delete(w.threads, ref)
	}
}

func (w *warmTranscripts) open(fill warmFill) *warmStream {
	stream := &warmStream{queue: newLiveTextQueueWithBacklog(warmStreamBacklog), openedAt: time.Now().UTC(), fill: fill}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.streams == nil {
		w.streams = make(map[*warmStream]bool)
	}
	w.streams[stream] = true
	return stream
}

func (w *warmTranscripts) close(stream *warmStream) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.streams, stream)
}

func (w *warmTranscripts) report() []warmStreamReport {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]warmStreamReport, 0, len(w.streams))
	for stream := range w.streams {
		out = append(out, warmStreamReport{OpenedAt: stream.openedAt, Lines: stream.fill.lines, Cols: stream.fill.cols,
			Refs: stream.refs.Load(), Reads: stream.reads.Load(), Bytes: stream.bytes.Load(),
			Tails: stream.tails.Load(), TailBytes: stream.tailBytes.Load()})
	}
	slices.SortFunc(out, func(a, b warmStreamReport) int { return a.OpenedAt.Compare(b.OpenedAt) })
	return out
}

// handleWarmTranscripts streams the set's saved rows. Messages are the
// file-only pane messages (tail, update, error) tagged with ref, and set,
// which names the threads carried: the browser drops any other. A thread
// joining with a marker at the current projection gets an update with the
// rows it lacks; any other joining thread gets a tail; one that cannot be
// warmed gets one error, and is tried again when the sidebar changes.
func (s *server) handleWarmTranscripts(w http.ResponseWriter, r *http.Request) {
	request, err := parseWarmRequest(r)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpapi.WriteHTTPError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var sidebarEvents <-chan sidebarDelta
	if s.sidebar != nil {
		ch := s.sidebar.events.subscribe()
		defer s.sidebar.events.unsubscribe(ch)
		sidebarEvents = ch
	}
	stream := s.warm.open(request.fill)
	defer s.warm.close(stream)
	type member struct {
		sess     *session
		pane     *liveTextPane
		joining  bool // its first message is not written yet
		joinedAt time.Time
	}
	members := make(map[string]member)
	leave := func(ref string, m member) {
		m.sess.liveText.unsubscribe(m.pane)
		s.warm.release(ref)
		delete(members, ref)
	}
	defer func() {
		for ref, m := range members {
			leave(ref, m)
		}
	}()
	told := make(map[string]bool)          // threads this stream has sent an error for
	failedAt := make(map[string]time.Time) // threads whose pane failed, and when
	var sent []string                      // the set the browser last received

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	write := func(raw []byte) bool {
		n, err := w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
		stream.bytes.Add(int64(n))
		if bytes.HasPrefix(raw, []byte(`{"type":"tail"`)) {
			stream.tails.Add(1)
			stream.tailBytes.Add(int64(n))
		}
		return err == nil
	}
	writeMessage := func(message liveTextMessage) bool {
		raw, err := json.Marshal(message)
		return err == nil && write(raw)
	}
	// update moves the stream toward the set: a member whose session was
	// replaced rejoins on its new loop, a failed one leaves and is tried
	// again later, a few threads join at a time in set order, and the rest
	// stay carried until a slot frees. It reports how long until something
	// it is waiting on (a joining thread's slot, a failed thread's retry)
	// needs it again, zero for nothing, and whether the writes succeeded.
	update := func() (wake time.Duration, ok bool) {
		now := time.Now()
		soonest := func(d time.Duration) {
			if d > 0 && (wake == 0 || d < wake) {
				wake = d
			}
		}
		for ref, m := range members {
			switch {
			case m.pane.isClosed():
				// Its own error went out when it failed; the browser dropped
				// what it held, so a retry starts from nothing.
				failedAt[ref], told[ref] = now, true
				delete(request.markers, ref)
				leave(ref, m)
			case s.sessionForHostedThread(ref) != m.sess:
				leave(ref, m)
			}
		}
		joining := 0
		for _, m := range members {
			if m.joining && now.Sub(m.joinedAt) < warmJoinWait {
				joining++
				soonest(warmJoinWait - now.Sub(m.joinedAt))
			}
		}
		carried := make([]string, 0, len(request.set))
		for _, ref := range request.set {
			if _, ok := members[ref]; ok || joining >= warmJoining {
				carried = append(carried, ref)
				continue
			}
			if failed, ok := failedAt[ref]; ok && now.Sub(failed) < warmRetryWait {
				soonest(warmRetryWait - now.Sub(failed))
				continue
			}
			sess := s.sessionForHostedThread(ref)
			reason := ""
			switch {
			case sess == nil || sess.liveText == nil:
				reason = "no live session holds this thread"
			case !s.warm.admit(ref):
				reason = "the warm budget is spent"
			}
			if reason != "" {
				// The browser drops the thread on the error, so a later
				// retry sends a tail, not an update against its marker.
				delete(request.markers, ref)
				if !told[ref] {
					told[ref] = true
					if !writeMessage(liveTextMessage{Type: "error", Ref: ref, Error: reason}) {
						return 0, false
					}
				}
				continue
			}
			pane := newLiveTextPane(warmPaneQuery())
			pane.rowsOnly, pane.warm, pane.ref, pane.reads, pane.fill = true, true, ref, &stream.reads, request.fill
			if marker, ok := request.markers[ref]; ok {
				pane.marker = &marker
			}
			pane.liveTextQueue = stream.queue
			members[ref] = member{sess: sess, pane: pane, joining: true, joinedAt: now}
			joining++
			soonest(warmJoinWait)
			sess.liveText.subscribe(pane)
			carried = append(carried, ref)
		}
		stream.refs.Store(int64(len(members)))
		if sent != nil && slices.Equal(carried, sent) {
			return wake, true
		}
		sent = carried
		return wake, writeMessage(liveTextMessage{Type: "set", Refs: carried})
	}
	// joined notes a thread's first message written, which frees its slot.
	joined := func(raw []byte) bool {
		var head struct {
			Ref string `json:"ref"`
		}
		if json.Unmarshal(raw, &head) != nil {
			return false
		}
		m, ok := members[head.Ref]
		if !ok || !m.joining {
			return false
		}
		m.joining = false
		members[head.Ref] = m
		return true
	}
	// wake brings the stream back to update when a wait runs out: a joining
	// thread's slot, or a failed thread's retry.
	var wake <-chan time.Time
	settle := func() bool {
		in, ok := update()
		if in > 0 {
			wake = time.After(in)
		} else {
			wake = nil
		}
		flusher.Flush()
		return ok
	}
	if !settle() {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stream.queue.done:
			return // the backlog overflowed; the browser reconnects with its markers
		case <-stream.queue.ready:
			// A first message frees a joining slot; an error means a pane
			// failed and leaves on the next update.
			resettle := false
			for _, raw := range stream.queue.take() {
				if !write(raw) {
					return
				}
				resettle = joined(raw) || bytes.HasPrefix(raw, []byte(`{"type":"error"`)) || resettle
			}
			flusher.Flush()
			if resettle && !settle() {
				return
			}
		case <-wake:
			if !settle() {
				return
			}
		case _, ok := <-sidebarEvents:
			if !ok {
				sidebarEvents = nil
				continue
			}
			if !settle() {
				return
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
