package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// warmReader is the browser's end of one warm stream.
type warmReader struct {
	t        *testing.T
	status   int
	messages chan liveTextMessage
}

// openWarm opens a warm stream for a set of threads; extra query parameters
// (lines, cols, markers) ride along.
func openWarm(t *testing.T, srv *server, set []string, extra url.Values) *warmReader {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(srv.handleWarmTranscripts))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ts.Close() })
	query := url.Values{"set": {strings.Join(set, ",")}}
	for key, values := range extra {
		query[key] = values
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/warm-transcripts?"+query.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := &warmReader{t: t, status: resp.StatusCode, messages: make(chan liveTextMessage, 64)}
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 4<<20)
		for scanner.Scan() {
			data, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if !ok {
				continue
			}
			var message liveTextMessage
			if err := json.Unmarshal([]byte(data), &message); err != nil {
				t.Error(err)
				return
			}
			reader.messages <- message
		}
	}()
	return reader
}

func (r *warmReader) next(what string) liveTextMessage {
	r.t.Helper()
	select {
	case message := <-r.messages:
		return message
	case <-time.After(20 * time.Second):
		r.t.Fatalf("timed out waiting for %s", what)
	}
	return liveTextMessage{}
}

// quiet fails if the stream sends anything for twice the warm delay.
func (r *warmReader) quiet(why string) {
	r.t.Helper()
	select {
	case message := <-r.messages:
		r.t.Fatalf("%s, but the stream sent %s for %q", why, message.Type, message.Ref)
	case <-time.After(2 * liveTextWarmDelay):
	}
}

func rowIDs(rows []transcriptItem) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func markersParam(t *testing.T, ref, key string, held []transcriptItem) url.Values {
	t.Helper()
	first, last := held[0].ProjectedSeq, held[len(held)-1].ProjectedSeq
	raw, err := json.Marshal(map[string]map[string]any{ref: {"key": key, "first": first, "last": last, "digest": warmHeldDigest(held)}})
	if err != nil {
		t.Fatal(err)
	}
	return url.Values{"markers": {string(raw)}}
}

// A warm thread rides a pane on screen's reads: it gets its tail at once and
// no second read is made while the thread is shown; once the screen pane
// leaves it reads on its own coalesced schedule; idle, it reads nothing; a
// burst of publishes costs one read; a saved row reaches it as one update.
func TestWarmStreamRidesScreenReadsAndCoalesces(t *testing.T) {
	h := newLiveTextHarnessWith(t, warmPaneQuery()) // the Enhanced pane's own page query
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	reply := lineWith(saved, fixtureReply[:strings.Index(fixtureReply, ":")])
	h.save(saved[:reply])
	h.expect("first rows", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	ref := h.sess.hostedThreadID
	h.srv.sessions = map[string]*session{h.sess.id: h.sess}

	reads := h.catalogReads.Load()
	warm := openWarm(t, h.srv, []string{ref}, nil)
	if set := warm.next("set"); set.Type != "set" || !slices.Equal(set.Refs, []string{ref}) {
		t.Fatalf("first message = %+v, want the set naming the thread", set)
	}
	tail := warm.next("tail while the thread is on screen")
	if tail.Type != "tail" || tail.Ref != ref || tail.Live != nil || tail.Saved != nil {
		t.Fatalf("got %+v, want a rows-only tail for %s", tail, ref)
	}
	if got, want := rowIDs(tail.Rows), rowIDs(h.page(warmPaneQuery())); !slices.Equal(got, want) {
		t.Fatalf("tail rows = %v, want the saved page %v", got, want)
	}
	if tail.Page == nil || tail.Page.StartSeq != tail.Rows[0].ProjectedSeq {
		t.Fatalf("tail page = %+v, want startSeq at the window's first row %d", tail.Page, tail.Rows[0].ProjectedSeq)
	}
	if got := h.catalogReads.Load() - reads; got != 1 {
		t.Fatalf("joining beside a pane on screen took %d reads, want 1 shared read", got)
	}
	h.barrier()

	h.sess.liveText.unsubscribe(h.pane)
	reads = h.catalogReads.Load()
	warm.quiet("the thread is idle")
	for range 5 {
		h.sess.liveText.applied()
	}
	warm.quiet("nothing changed")
	if got := h.catalogReads.Load() - reads; got != 1 {
		t.Fatalf("a burst of publishes took %d reads, want 1", got)
	}

	h.save(saved[reply:])
	update := warm.next("the newly saved reply")
	if update.Type != "update" || update.Ref != ref || len(update.Rows) == 0 || update.Page == nil {
		t.Fatalf("got %+v, want an update with the new rows and the window's page", update)
	}
	for _, row := range update.Rows {
		if !slices.Contains(rowIDs(h.page(warmPaneQuery())), row.ID) {
			t.Fatalf("update row %s is not on the saved page", row.ID)
		}
	}
}

// The warm pane reads the page the Enhanced pane on screen reads, or the
// loop would make a second read for every save of a shown thread.
func TestWarmPaneSharesTheScreenPanesPage(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/live?limit=40&view=enhanced&hide=reasoning,toolCall,toolResult,subagent,system,error,unclassified", nil)
	screen, err := parseTranscriptListQuery(req)
	if err != nil {
		t.Fatal(err)
	}
	if pageQueryKey(screen) != pageQueryKey(warmPaneQuery()) {
		t.Fatalf("warm page key %q, screen pane page key %q", pageQueryKey(warmPaneQuery()), pageQueryKey(screen))
	}
}

// A page that still holds a thread's rows sends their projection key and
// sequence range. At the current projection it gets an update with only the
// rows it lacks, never a tail; a stale key gets a tail.
func TestWarmStreamMarkerGetsOnlyTheMissingRows(t *testing.T) {
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	reply := lineWith(saved, fixtureReply[:strings.Index(fixtureReply, ":")])
	h.save(saved[:reply])
	h.expect("first rows", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	ref := h.sess.hostedThreadID
	h.srv.sessions = map[string]*session{h.sess.id: h.sess}

	first := openWarm(t, h.srv, []string{ref}, nil)
	first.next("set")
	tail := first.next("tail")
	key, held := tail.Key, tail.Rows
	newest := held[len(held)-1].ProjectedSeq

	current := openWarm(t, h.srv, []string{ref}, markersParam(t, ref, key, held))
	current.next("set")
	if got := current.next("join at the current projection"); got.Type != "update" || len(got.Rows) != 0 || got.Key != key || got.Page == nil {
		t.Fatalf("got %+v, want an empty update at key %s", got, key)
	}

	h.save(saved[reply:])
	grown := current.next("the saved reply")
	if grown.Type != "update" || len(grown.Rows) == 0 {
		t.Fatalf("got %+v, want an update with the reply", grown)
	}
	page := h.page(warmPaneQuery())
	behind := openWarm(t, h.srv, []string{ref}, markersParam(t, ref, key, held))
	behind.next("set")
	catchUp := behind.next("join holding an older range")
	if catchUp.Type != "update" || len(catchUp.Rows) != len(page)-len(held) {
		t.Fatalf("got %s with %d rows, want an update with the %d rows after the marker", catchUp.Type, len(catchUp.Rows), len(page)-len(held))
	}
	for _, row := range catchUp.Rows {
		if row.ProjectedSeq <= newest {
			t.Fatalf("catch-up row %s seq %d is inside the held range", row.ID, row.ProjectedSeq)
		}
	}

	stale := openWarm(t, h.srv, []string{ref}, markersParam(t, ref, "ntp_from_before_a_rebuild", held))
	stale.next("set")
	if got := stale.next("join with a stale key"); got.Type != "tail" || len(got.Rows) != len(page) {
		t.Fatalf("got %s with %d rows, want a full tail of %d", got.Type, len(got.Rows), len(page))
	}
	// Held rows that changed while the page was away: the same key and range,
	// a different digest. An update would leave the old text on screen for
	// good, since the pane's digests would already match the saved rows. The
	// change here keeps the text's length, and a second one touches only the
	// final-answer flag: the digest covers content, not size.
	for _, change := range []func(*transcriptItem){
		func(row *transcriptItem) { row.Text = strings.Repeat("z", len(row.Text)) },
		func(row *transcriptItem) { row.IsFinalAnswer = !row.IsFinalAnswer },
	} {
		changed := append([]transcriptItem(nil), held...)
		change(&changed[0])
		edited := openWarm(t, h.srv, []string{ref}, markersParam(t, ref, key, changed))
		edited.next("set")
		if got := edited.next("join holding changed rows"); got.Type != "tail" {
			t.Fatalf("got %s, want a tail that replaces the changed held rows", got.Type)
		}
	}
}

// failOneRead fails the next transcript read, then reads as before.
type failOneRead struct {
	*countingLiveTextCatalog
	failed bool
}

func (c *failOneRead) LoadTranscripts(ctx context.Context, id string, queries []transcriptListQuery, matches []history.TranscriptMatch) ([]transcriptResponse, map[string]string, error) {
	if !c.failed {
		c.failed = true
		return nil, nil, fmt.Errorf("synthetic read failure")
	}
	return c.countingLiveTextCatalog.LoadTranscripts(ctx, id, queries, matches)
}

// A thread whose first read fails gets one error and leaves. The stream tries
// it again when its wait runs out, on its own timer: with no sidebar event in
// between, the thread would otherwise stay cold.
func TestWarmStreamRetriesAFailedThreadWhenItsWaitRunsOut(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.expect("rows", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	h.sess.liveText.unsubscribe(h.pane)
	h.srv.catalog = &failOneRead{countingLiveTextCatalog: h.srv.catalog.(*countingLiveTextCatalog)}
	h.srv.sessions = map[string]*session{h.sess.id: h.sess}

	warm := openWarm(t, h.srv, []string{h.sess.hostedThreadID}, nil)
	warm.next("set")
	if got := warm.next("the failed read"); got.Type != "error" {
		t.Fatalf("got %+v, want the read error", got)
	}
	started := time.Now()
	for {
		got := warm.next("the retry's tail")
		if got.Type == "tail" {
			if time.Since(started) > warmRetryWait+3*time.Second {
				t.Fatalf("the retry took %s", time.Since(started))
			}
			return
		}
	}
}

// Threads whose projections are still indexing send nothing. Four of them
// joining at once must not hold the stream's slots for good: an indexed thread
// behind them joins once the wait runs out.
func TestWarmStreamIndexingThreadsDoNotBlockTheRest(t *testing.T) {
	h := newLiveTextHarness(t)
	h.srv.sessions = map[string]*session{}
	refs := []string{}
	for i := range warmJoining {
		pending := newLiveTextHarness(t)
		pending.sess.hostedThreadID = fmt.Sprintf("nht_indexing_%d", i)
		pending.sess.id = fmt.Sprintf("indexing_%d", i)
		h.srv.sessions[pending.sess.id] = pending.sess
		refs = append(refs, pending.sess.hostedThreadID)
	}
	h.srv.sessions[h.sess.id] = h.sess
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.expect("indexed rows", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	refs = append(refs, h.sess.hostedThreadID)

	warm := openWarm(t, h.srv, refs, nil)
	if set := warm.next("set"); !slices.Equal(set.Refs, refs) {
		t.Fatalf("set = %v, want every thread carried, pending ones included", set.Refs)
	}
	started := time.Now()
	got := warm.next("the indexed thread's tail")
	if got.Type != "tail" || got.Ref != h.sess.hostedThreadID {
		t.Fatalf("got %s for %s, want the indexed thread's tail", got.Type, got.Ref)
	}
	if waited := time.Since(started); waited > warmJoinWait+3*time.Second {
		t.Fatalf("the indexed thread waited %s behind four indexing threads", waited)
	}
}

// A refused thread is dropped by the browser, marker and all. When the
// refusal clears, the thread joins with a tail, never with an update against
// rows the browser no longer holds.
func TestWarmStreamRetriesARefusedThreadWithATail(t *testing.T) {
	h := newLiveTextHarness(t)
	saved := fixtureLines(t, "claude-turn.saved.jsonl")
	h.save(saved)
	h.expect("rows", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	ref := h.sess.hostedThreadID
	h.srv.sessions = map[string]*session{h.sess.id: h.sess}
	h.srv.sidebar = newSidebarMaterializedState() // the stream retries on its events
	earlier := openWarm(t, h.srv, []string{ref}, nil)
	earlier.next("set")
	tail := earlier.next("tail")
	key, held := tail.Key, tail.Rows

	h.srv.sessions = map[string]*session{} // the session is gone for now
	warm := openWarm(t, h.srv, []string{ref}, markersParam(t, ref, key, held))
	if got := warm.next("refusal"); got.Type != "error" {
		t.Fatalf("got %+v, want an error", got)
	}
	warm.next("set")
	h.srv.sessions = map[string]*session{h.sess.id: h.sess} // it is back
	h.srv.sidebar.events.notify(sidebarDelta{Revision: 1})
	got := warm.next("retry")
	if got.Type == "set" { // the set names the thread again before its rows
		got = warm.next("retry rows")
	}
	if got.Type != "tail" || got.Ref != ref || len(got.Rows) != len(held) {
		t.Fatalf("got %s with %d rows, want a tail of %d: the browser dropped the thread on the error", got.Type, len(got.Rows), len(held))
	}
}

// The window: a page reporting a small window gets only the newest rows that
// fill it, with the page's startSeq at the first row sent; a page reporting
// nothing gets the default window. Rows and cols out of range are refused.
func TestWarmStreamSendsTheWindowThatFillsThePage(t *testing.T) {
	h := newLiveTextHarness(t)
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.expect("rows", func(liveTextMessage) bool { return len(h.view.rows) >= 2 })
	ref := h.sess.hostedThreadID
	h.srv.sessions = map[string]*session{h.sess.id: h.sess}
	page := h.page(warmPaneQuery())

	// A narrow window: the newest reply alone wraps past five lines.
	small := openWarm(t, h.srv, []string{ref}, url.Values{"lines": {"5"}, "cols": {"20"}})
	small.next("set")
	tail := small.next("tail")
	want := (warmFill{lines: 5, cols: 20}).count(page)
	if want >= len(page) {
		t.Fatalf("the fixture's %d rows fit a 5-line window; the test needs a page taller than the window", len(page))
	}
	if len(tail.Rows) != want || !slices.Equal(rowIDs(tail.Rows), rowIDs(page[len(page)-want:])) {
		t.Fatalf("tail rows = %v, want the newest %d of %v", rowIDs(tail.Rows), want, rowIDs(page))
	}
	if tail.Page.StartSeq != tail.Rows[0].ProjectedSeq || !tail.Page.HasMoreBefore {
		t.Fatalf("tail page = %+v, want startSeq %d and more before", tail.Page, tail.Rows[0].ProjectedSeq)
	}
	for _, turn := range tail.Turns {
		if !slices.ContainsFunc(tail.Rows, func(row transcriptItem) bool { return row.TurnID == turn.ID }) {
			t.Fatalf("turn %s is not one of the window's rows' turns", turn.ID)
		}
	}
	if report := h.srv.warm.report(); len(report) == 0 || report[len(report)-1].Lines != 5 || report[len(report)-1].Cols != 20 {
		t.Fatalf("monitor report = %+v, want the stream's 5 x 20 window", report)
	}

	whole := openWarm(t, h.srv, []string{ref}, nil)
	whole.next("set")
	if got := whole.next("tail"); len(got.Rows) != len(page) {
		t.Fatalf("default window sent %d rows of %d", len(got.Rows), len(page))
	}
	if bad := openWarm(t, h.srv, []string{ref}, url.Values{"lines": {"3"}, "cols": {"20"}}); bad.status != http.StatusBadRequest {
		t.Fatalf("lines below the minimum: status %d, want 400", bad.status)
	}
}

// A thread the server cannot warm gets one error and stays out of the set:
// no live session holds it, or the server-wide budget is spent. It is tried
// again when the sidebar changes, with no second error.
func TestWarmStreamRefusesThreadsItCannotWarm(t *testing.T) {
	h := newLiveTextHarness(t)
	h.srv.sessions = map[string]*session{h.sess.id: h.sess}
	for i := range warmBudget {
		h.srv.warm.admit(fmt.Sprintf("nht_elsewhere_%d", i))
	}
	warm := openWarm(t, h.srv, []string{"nht_not_held", h.sess.hostedThreadID}, nil)
	for _, want := range []struct{ ref, reason string }{
		{"nht_not_held", "no live session holds this thread"},
		{h.sess.hostedThreadID, "the warm budget is spent"},
	} {
		if got := warm.next("refusal"); got.Type != "error" || got.Ref != want.ref || got.Error != want.reason {
			t.Fatalf("got %+v, want error %q for %s", got, want.reason, want.ref)
		}
	}
	if set := warm.next("set"); set.Type != "set" || len(set.Refs) != 0 {
		t.Fatalf("got %+v, want an empty set", set)
	}
	if bad := openWarm(t, h.srv, []string{h.sess.hostedThreadID}, url.Values{"markers": {"not json"}}); bad.status != http.StatusBadRequest {
		t.Fatalf("malformed markers: status %d, want 400", bad.status)
	}
}

// The request: the set keeps its order without repeats and stops at the cap;
// hints from a page that has not reloaded are the set; a marker needs a key
// and a sequence range to count.
func TestParseWarmRequest(t *testing.T) {
	refs := make([]string, 0, warmSetMax+5)
	for i := range warmSetMax + 5 {
		refs = append(refs, fmt.Sprintf("nht_%03d", i))
	}
	req := httptest.NewRequest(http.MethodGet, "/warm?set="+strings.Join(append([]string{"nht_b", "nht_a", "nht_b"}, refs...), ","), nil)
	got, err := parseWarmRequest(req)
	if err != nil || len(got.set) != warmSetMax || got.set[0] != "nht_b" || got.set[1] != "nht_a" || got.set[2] != "nht_000" {
		t.Fatalf("set = %d refs starting %v (err %v), want %d in order without repeats", len(got.set), got.set[:3], err, warmSetMax)
	}
	req = httptest.NewRequest(http.MethodGet, "/warm?hints=nht_x&markers="+url.QueryEscape(`{"nht_x":{"key":"ntp_1","first":3,"last":9,"digest":"00000000deadbeef"},"nht_y":{"key":"","first":1,"last":2,"digest":"1"},"nht_z":{"key":"ntp_2","first":5,"last":4,"digest":"1"},"nht_w":{"key":"ntp_3","first":1,"last":1}}`), nil)
	got, err = parseWarmRequest(req)
	if err != nil || !slices.Equal(got.set, []string{"nht_x"}) || len(got.markers) != 1 || got.markers["nht_x"] != (warmMarker{key: "ntp_1", first: 3, last: 9, digest: "00000000deadbeef"}) {
		t.Fatalf("hints + markers = %+v (err %v), want the hint as the set and one valid marker", got, err)
	}
	if got.fill != (warmFill{lines: warmFillLinesDefault, cols: warmFillColsDefault}) {
		t.Fatalf("fill = %+v, want the default window", got.fill)
	}
}

// Panes asking for the same page share one read.
func TestLiveTextSharesIdenticalPageReads(t *testing.T) {
	h := newLiveTextHarness(t)
	h.open(transcriptListQuery{View: defaultThreadTranscriptView, Limit: 40})
	h.save(fixtureLines(t, "claude-turn.saved.jsonl"))
	h.expect("rows", func(liveTextMessage) bool { return len(h.view.rows) > 0 })
	if got := h.lastQueries.Load(); got != 1 {
		t.Fatalf("two panes on one page read %d pages, want 1", got)
	}
}

// A warm stream's backlog counts its tails: the threads joining at once,
// each at the largest page the catalog keeps inline, still fit in half of it,
// so a connect burst never ends the stream; a thread leaving takes back
// exactly the bytes it queued.
func TestWarmStreamBacklogBoundsTails(t *testing.T) {
	queue := newLiveTextQueueWithBacklog(warmStreamBacklog)
	warmPane := func(ref string) *liveTextPane {
		pane := newLiveTextPane(transcriptListQuery{})
		pane.rowsOnly, pane.warm, pane.ref, pane.liveTextQueue = true, true, ref, queue
		return pane
	}
	largest := func() liveTextMessage {
		rows := make([]transcriptItem, warmPageRows)
		for i := range rows {
			rows[i] = transcriptItem{ID: fmt.Sprintf("row-%d", i), Text: strings.Repeat("x", 8<<10)}
		}
		return liveTextMessage{Type: "tail", Rows: rows}
	}
	for i := range warmJoining {
		warmPane(fmt.Sprintf("nht_%d", i)).send(largest())
	}
	if queue.closed || queue.bytes == 0 || queue.bytes > warmStreamBacklog/2 {
		t.Fatalf("a burst of %d largest tails left the stream closed=%v with %d bytes counted", warmJoining, queue.closed, queue.bytes)
	}
	if got := queue.take(); len(got) != warmJoining || queue.bytes != 0 {
		t.Fatalf("after take: %d messages, %d bytes held, want %d and 0", len(got), queue.bytes, warmJoining)
	}
	leaving := warmPane("nht_leaving")
	leaving.fail("the saved transcript could not be read")
	leaving.close()
	if queue.bytes != 0 || len(queue.queue) != 0 {
		t.Fatalf("a thread that left still holds %d bytes in %d messages", queue.bytes, len(queue.queue))
	}
}
