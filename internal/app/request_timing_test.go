package app

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/observability"
)

type monitorRequests struct {
	Requests    []observability.RequestMinute `json:"requests"`
	StreamsOpen map[string]int                `json:"streamsOpen"`
}

func readMonitorRequests(t *testing.T, url string) monitorRequests {
	t.Helper()
	resp, err := http.Get(url + "/api/v1/monitor")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body monitorRequests
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// timingServer serves the real route table, plus any extra timed routes, with
// request timing on a clock the test sets.
func timingServer(t *testing.T, at time.Time, extra func(s *server, mux *http.ServeMux)) (*server, *atomic.Value, string) {
	t.Helper()
	clock := &atomic.Value{}
	clock.Store(at)
	dir := t.TempDir()
	s := &server{sessionRegistry: sessionRegistry{sessions: map[string]*session{}}, hostMonitor: newHostMonitor(dir, filepath.Join(dir, "deploys.jsonl"))}
	s.requests = observability.NewRequestTimes(func() time.Time { return clock.Load().(time.Time) })
	mux := http.NewServeMux()
	mux.Handle("/", applicationHandler(s))
	if extra != nil {
		extra(s, mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, clock, srv.URL
}

// Catches: API calls missing from the monitor's view because a route was
// mounted untimed, failures not counted by status, a 304 cache revalidation
// counted as a failure, response bytes lost across several writes, or a
// minute served while still open, which the monitor would then record short.
// Boundary: the real route table, plus one timed 304 route, read back through
// GET /api/v1/monitor.
func TestMonitorServesEachRoutesCallsOnceTheirMinuteCloses(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 30, 0, time.UTC)
	_, clock, url := timingServer(t, at, func(s *server, mux *http.ServeMux) {
		mux.HandleFunc("GET /cached", s.requests.Timed("test.cached", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotModified)
		}))
		mux.HandleFunc("GET /sized", s.requests.Timed("test.sized", func(w http.ResponseWriter, r *http.Request) {
			n, _ := strconv.Atoi(r.URL.Query().Get("n"))
			_, _ = w.Write([]byte(strings.Repeat("x", n/2)))
			_, _ = w.Write([]byte(strings.Repeat("x", n-n/2)))
		}))
	})
	for _, n := range []string{"300", "100", "200"} {
		resp, err := http.Get(url + "/sized?n=" + n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if resp, err := http.Get(url + "/cached"); err != nil || resp.StatusCode != http.StatusNotModified {
		t.Fatalf("cached route = %v, %v", resp, err)
	} else {
		resp.Body.Close()
	}
	for range 2 {
		resp, err := http.Post(url+"/api/sessions/missing/interrupt", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("interrupt of a missing session = %d", resp.StatusCode)
		}
	}

	if open := readMonitorRequests(t, url); len(open.Requests) != 0 {
		t.Fatalf("served the open minute: %+v", open.Requests)
	}
	clock.Store(at.Add(time.Minute))
	closed := readMonitorRequests(t, url)
	if len(closed.Requests) != 1 {
		t.Fatalf("minutes = %+v; want the one closed minute", closed.Requests)
	}
	routes := closed.Requests[0].Routes
	if r := routes["sessions.turn.interrupt"]; r.Calls != 2 || !reflect.DeepEqual(r.Failed, map[string]int{"404": 2}) {
		t.Fatalf("interrupt = %+v; want 2 calls, both 404", r)
	}
	if r := routes["monitor.read"]; r.Calls != 1 || r.Failed != nil {
		t.Fatalf("monitor reads = %+v; want the first read, answered 200", r)
	}
	if r := routes["test.cached"]; r.Calls != 1 || r.Failed != nil || r.MaxBytes != 0 {
		t.Fatalf("cached = %+v; want one call, no failure and no body", r)
	}
	if r := routes["test.sized"]; r.Calls != 3 || r.MedianBytes != 200 || r.MaxBytes != 300 {
		t.Fatalf("sized = %+v; want 3 calls, median 200 and max 300 body bytes over several writes", r)
	}
}

// Catches: a slow call filed in the minute it started, after the monitor has
// already saved that minute, so the call is never recorded. Boundary: a timed
// handler served next to the real route table, read through
// GET /api/v1/monitor.
func TestMonitorFilesASlowCallInTheMinuteItEnds(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 30, 0, time.UTC)
	started, release := make(chan struct{}), make(chan struct{})
	_, clock, url := timingServer(t, at, func(s *server, mux *http.ServeMux) {
		mux.HandleFunc("GET /slow", s.requests.Timed("test.slow", func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
		}))
	})
	done := make(chan error, 1)
	go func() {
		resp, err := http.Get(url + "/slow")
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-started
	clock.Store(at.Add(40 * time.Second)) // 12:01:10: the call's start minute has closed
	served := readMonitorRequests(t, url)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if again := readMonitorRequests(t, url); !reflect.DeepEqual(again.Requests, served.Requests) {
		t.Fatalf("a served minute changed: %+v, then %+v", served.Requests, again.Requests)
	}
	clock.Store(at.Add(100 * time.Second)) // 12:02:10
	latest := readMonitorRequests(t, url).Requests
	if r := latest[len(latest)-1]; !r.Minute.Equal(at.Add(30*time.Second)) || r.Routes["test.slow"].Calls != 1 || r.Routes["test.slow"].MaxMS != 40000 {
		t.Fatalf("minutes = %+v; want the 40-second call in 12:01, the minute it ended", latest)
	}
}

// Catches: a live stream counted as one endless call, its open count never
// falling (here after the handler aborts with a panic, as net/http allows),
// the timing wrapper hiding http.Flusher so no event reaches the browser, or a
// stream's first data (a thread's initial tail) timed from a keepalive
// comment instead of the data.
// Boundary: a served stream behind the same wrapper the route table applies,
// read back through GET /api/v1/monitor.
func TestMonitorCountsStreamsApartFromCalls(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 30, 0, time.UTC)
	sendData, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	_, clock, url := timingServer(t, at, func(s *server, mux *http.ServeMux) {
		stream := s.requests.Timed("test.stream", func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flusher", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
			<-sendData
			_, _ = w.Write([]byte("data: hello\n\n"))
			flusher.Flush()
			_, _ = w.Write([]byte("data: later\n\n")) // not first data
			flusher.Flush()
			<-release
			panic(http.ErrAbortHandler)
		})
		mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
			defer close(finished) // after the wrapper has recorded the stream's end
			stream(w, r)
		})
	})

	resp, err := http.Get(url + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bufio.NewReader(resp.Body)
	if line, err := body.ReadString('\n'); err != nil || strings.TrimSpace(line) != ": keepalive" {
		t.Fatalf("first stream line %q, err %v; want the keepalive", line, err)
	}
	clock.Store(at.Add(2 * time.Second))
	close(sendData)
	_, _ = body.ReadString('\n') // the keepalive's blank line
	line, err := body.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "data: hello" {
		t.Fatalf("first stream line %q, err %v; want the flushed event", line, err)
	}
	if open := readMonitorRequests(t, url); open.StreamsOpen["test.stream"] != 1 {
		t.Fatalf("streams open = %v; want test.stream 1", open.StreamsOpen)
	}

	close(release)
	<-finished
	clock.Store(at.Add(time.Minute))
	closed := readMonitorRequests(t, url)
	if _, still := closed.StreamsOpen["test.stream"]; still {
		t.Fatalf("streams open after it ended = %v", closed.StreamsOpen)
	}
	if r := closed.Requests[0].Routes["test.stream"]; r.Calls != 0 || r.StreamsOpened != 1 || r.FirstDataMaxMS != 2000 || r.FirstDataMaxBytes != int64(len(": keepalive\n\ndata: hello\n\n")) {
		t.Fatalf("test.stream = %+v; want no calls, one stream opened, and first data after 2 s and 26 bytes", r)
	}
}

// Catches: a handler that panics before answering recorded as a success, so
// the failure never shows in the monitor's counts. net/http drops the
// connection, which the caller sees as a failed call. Boundary: a timed
// handler served next to the real route table, read through
// GET /api/v1/monitor.
func TestMonitorCountsAPanickedCallAsA500(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 30, 0, time.UTC)
	_, clock, url := timingServer(t, at, func(s *server, mux *http.ServeMux) {
		mux.HandleFunc("GET /panics", s.requests.Timed("test.panics", func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}))
	})
	if resp, err := http.Get(url + "/panics"); err == nil {
		resp.Body.Close()
		t.Fatalf("a panicking handler answered %d", resp.StatusCode)
	}
	clock.Store(at.Add(time.Minute))
	if r := readMonitorRequests(t, url).Requests[0].Routes["test.panics"]; r.Calls != 1 || !reflect.DeepEqual(r.Failed, map[string]int{"500": 1}) {
		t.Fatalf("test.panics = %+v; want one call, failed 500", r)
	}
}
