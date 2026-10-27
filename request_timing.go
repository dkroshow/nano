package main

import (
	"cmp"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Nano times every API call it serves, by the route's operation name, and
// GET /api/v1/monitor serves closed minutes to external monitoring clients.
// A call is filed in the minute it ends, read from the
// same clock under the same lock that decides which minutes are closed, so a
// minute once served never changes. The monitor takes the minutes newer than
// its last snapshot's, so the minutes kept here cover a monitor gap of that
// length.

const requestMinutesKept = 10

type requestTimes struct {
	now     func() time.Time // nil: time.Now
	mu      sync.Mutex
	minutes map[int64]map[string]*routeCalls // by Unix minute, then operation
	streams map[string]int                   // streams open now, by operation
}

type routeCalls struct {
	took  []time.Duration
	sizes []int64 // response body bytes the handler wrote, per call
	// Per stream: time and bytes until its first flush that carried data.
	firstData      []time.Duration
	firstDataSizes []int64
	failed         map[int]int // answers of 400 and above, by status code
	streamsOpened  int
}

// requestMinute is one closed minute as /api/v1/monitor serves it.
type requestMinute struct {
	Minute time.Time              `json:"minute"`
	Routes map[string]routeMinute `json:"routes"`
}

type routeMinute struct {
	Calls       int     `json:"calls,omitempty"`
	MedianMS    float64 `json:"medianMs,omitempty"`
	MaxMS       float64 `json:"maxMs,omitempty"`
	MedianBytes int64   `json:"medianBytes,omitempty"`
	MaxBytes    int64   `json:"maxBytes,omitempty"`
	// A stream's initial content: time and bytes until it first sent data.
	FirstDataMedianMS    float64        `json:"firstDataMedianMs,omitempty"`
	FirstDataMaxMS       float64        `json:"firstDataMaxMs,omitempty"`
	FirstDataMedianBytes int64          `json:"firstDataMedianBytes,omitempty"`
	FirstDataMaxBytes    int64          `json:"firstDataMaxBytes,omitempty"`
	Failed               map[string]int `json:"failed,omitempty"`
	StreamsOpened        int            `json:"streamsOpened,omitempty"`
}

// timed wraps one route's handler. A text/event-stream answer counts as a
// stream, open until the handler returns, and stays out of call times.
func (t *requestTimes) timed(operation string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		began := t.clock()
		tw := &timedWriter{ResponseWriter: w, times: t, operation: operation, began: began}
		defer func() {
			// A panic ends the call too (net/http drops the connection): count
			// it as a 500, close its stream, and let it continue.
			p := recover()
			if p != nil && tw.status == 0 {
				tw.status = http.StatusInternalServerError
			}
			t.record(operation, began, tw)
			if p != nil {
				panic(p)
			}
		}()
		next(tw, r)
	}
}

func (t *requestTimes) clock() time.Time {
	if t.now == nil {
		return time.Now()
	}
	return t.now()
}

func (t *requestTimes) calls(operation string, at time.Time) *routeCalls {
	minute := at.Unix() / 60
	if t.minutes == nil {
		t.minutes, t.streams = map[int64]map[string]*routeCalls{}, map[string]int{}
	}
	for m := range t.minutes {
		if m < minute-requestMinutesKept {
			delete(t.minutes, m)
		}
	}
	routes := t.minutes[minute]
	if routes == nil {
		routes = map[string]*routeCalls{}
		t.minutes[minute] = routes
	}
	calls := routes[operation]
	if calls == nil {
		calls = &routeCalls{}
		routes[operation] = calls
	}
	return calls
}

func (t *requestTimes) streamOpened(operation string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls(operation, t.clock()).streamsOpened++
	t.streams[operation]++
}

// recordFirstData files a stream's time and bytes to its first data.
func (t *requestTimes) recordFirstData(operation string, began time.Time, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at := t.clock()
	calls := t.calls(operation, at)
	calls.firstData = append(calls.firstData, at.Sub(began))
	calls.firstDataSizes = append(calls.firstDataSizes, bytes)
}

func (t *requestTimes) record(operation string, began time.Time, w *timedWriter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ended := t.clock()
	if w.stream {
		if t.streams[operation]--; t.streams[operation] == 0 {
			delete(t.streams, operation)
		}
		return
	}
	calls := t.calls(operation, ended)
	calls.took = append(calls.took, ended.Sub(began))
	calls.sizes = append(calls.sizes, w.bytes)
	status := cmp.Or(w.status, http.StatusOK)
	// A 304 answers a revalidation from the browser's cache; only a client
	// or server error is a failure.
	if status >= 400 {
		if calls.failed == nil {
			calls.failed = map[int]int{}
		}
		calls.failed[status]++
	}
}

// report returns the closed minutes kept, oldest first, and the streams open.
// A median is the upper middle call of an even count.
func (t *requestTimes) report() ([]requestMinute, map[string]int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.clock().Unix() / 60
	minutes := []requestMinute{}
	for m, routes := range t.minutes {
		if m >= current || m < current-requestMinutesKept {
			continue
		}
		out := requestMinute{Minute: time.Unix(m*60, 0).UTC(), Routes: map[string]routeMinute{}}
		for operation, calls := range routes {
			r := routeMinute{Calls: len(calls.took), StreamsOpened: calls.streamsOpened}
			if len(calls.took) > 0 {
				took := slices.Clone(calls.took)
				slices.Sort(took)
				r.MedianMS, r.MaxMS = milliseconds(took[len(took)/2]), milliseconds(took[len(took)-1])
				sizes := slices.Clone(calls.sizes)
				slices.Sort(sizes)
				r.MedianBytes, r.MaxBytes = sizes[len(sizes)/2], sizes[len(sizes)-1]
			}
			if len(calls.firstData) > 0 {
				took, sizes := slices.Clone(calls.firstData), slices.Clone(calls.firstDataSizes)
				slices.Sort(took)
				slices.Sort(sizes)
				r.FirstDataMedianMS, r.FirstDataMaxMS = milliseconds(took[len(took)/2]), milliseconds(took[len(took)-1])
				r.FirstDataMedianBytes, r.FirstDataMaxBytes = sizes[len(sizes)/2], sizes[len(sizes)-1]
			}
			for status, n := range calls.failed {
				if r.Failed == nil {
					r.Failed = map[string]int{}
				}
				r.Failed[strconv.Itoa(status)] = n
			}
			out.Routes[operation] = r
		}
		minutes = append(minutes, out)
	}
	slices.SortFunc(minutes, func(a, b requestMinute) int { return a.Minute.Compare(b.Minute) })
	streams := make(map[string]int, len(t.streams))
	for operation, n := range t.streams {
		streams[operation] = n
	}
	return minutes, streams
}

func milliseconds(d time.Duration) float64 {
	return math.Round(float64(d.Microseconds())/100) / 10
}

// timedWriter notes the status a handler sends. It forwards Flush, which the
// stream handlers require, and Unwrap, for http.ResponseController.
type timedWriter struct {
	http.ResponseWriter
	times     *requestTimes
	operation string
	status    int // 0 until the header is sent
	stream    bool
	bytes     int64 // body bytes the handler wrote, before any compression
	began     time.Time
	// For a stream: whether it has written data (SSE comment lines such as
	// ": keepalive" are not data), and whether its first data has been
	// recorded, at the flush that sent it.
	unflushedData, firstDataSent bool
}

func (w *timedWriter) sent(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if status == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.stream = true
		w.times.streamOpened(w.operation)
	}
}

func (w *timedWriter) WriteHeader(status int) {
	w.sent(status)
	w.ResponseWriter.WriteHeader(status)
}

func (w *timedWriter) Write(b []byte) (int, error) {
	w.sent(http.StatusOK)
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	if w.stream && n > 0 && b[0] != ':' {
		w.unflushedData = true
	}
	return n, err
}

func (w *timedWriter) Flush() {
	w.sent(http.StatusOK)
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	if w.unflushedData && !w.firstDataSent {
		w.firstDataSent = true
		w.times.recordFirstData(w.operation, w.began, w.bytes)
	}
}

func (w *timedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
