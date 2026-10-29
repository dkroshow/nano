package sessions

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/threadindex"
)

// The request: the set keeps its order without repeats and stops at the cap;
// hints from a page that has not reloaded are the set; a marker needs a key
// and a sequence range to count.
func TestParseWarmRequest(t *testing.T) {
	refs := make([]string, 0, warmSetMax+5)
	for i := range warmSetMax + 5 {
		refs = append(refs, fmt.Sprintf("nht_%03d", i))
	}
	req := httptest.NewRequest(http.MethodGet, "/warm?set="+strings.Join(append([]string{"nht_b", "nht_a", "nht_b"}, refs...), ","), nil)
	got, err := ParseWarmRequest(req)
	if err != nil || len(got.Set) != warmSetMax || got.Set[0] != "nht_b" || got.Set[1] != "nht_a" || got.Set[2] != "nht_000" {
		t.Fatalf("set = %d refs starting %v (err %v), want %d in order without repeats", len(got.Set), got.Set[:3], err, warmSetMax)
	}
	req = httptest.NewRequest(http.MethodGet, "/warm?hints=nht_x&markers="+url.QueryEscape(`{"nht_x":{"key":"ntp_1","first":3,"last":9,"digest":"00000000deadbeef"},"nht_y":{"key":"","first":1,"last":2,"digest":"1"},"nht_z":{"key":"ntp_2","first":5,"last":4,"digest":"1"},"nht_w":{"key":"ntp_3","first":1,"last":1}}`), nil)
	got, err = ParseWarmRequest(req)
	if err != nil || !slices.Equal(got.Set, []string{"nht_x"}) || len(got.Markers) != 1 || got.Markers["nht_x"] != (WarmMarker{Key: "ntp_1", First: 3, Last: 9, Digest: "00000000deadbeef"}) {
		t.Fatalf("hints + markers = %+v (err %v), want the hint as the set and one valid marker", got, err)
	}
	if got.Fill != (WarmFill{lines: warmFillLinesDefault, cols: warmFillColsDefault}) {
		t.Fatalf("fill = %+v, want the default window", got.Fill)
	}
}

// A warm stream's backlog counts its tails: the threads joining at once,
// each at the largest page the catalog keeps inline, still fit in half of it,
// so a connect burst never ends the stream; a thread leaving takes back
// exactly the bytes it queued.
func TestWarmStreamBacklogBoundsTails(t *testing.T) {
	queue := newLiveTextQueueWithBacklog(warmStreamBacklog)
	warmPane := func(ref string) *liveTextPane {
		pane := newLiveTextPane(threadindex.TranscriptQuery{})
		pane.rowsOnly, pane.warm, pane.ref, pane.liveTextQueue = true, true, ref, queue
		return pane
	}
	largest := func() LiveTextMessage {
		rows := make([]threadindex.TranscriptItem, warmPageRows)
		for i := range rows {
			rows[i] = threadindex.TranscriptItem{ID: fmt.Sprintf("row-%d", i), Text: strings.Repeat("x", 8<<10)}
		}
		return LiveTextMessage{Type: "tail", Rows: rows}
	}
	for i := range WarmJoining {
		warmPane(fmt.Sprintf("nht_%d", i)).send(largest())
	}
	if queue.closed || queue.bytes == 0 || queue.bytes > warmStreamBacklog/2 {
		t.Fatalf("a burst of %d largest tails left the stream closed=%v with %d bytes counted", WarmJoining, queue.closed, queue.bytes)
	}
	if got := queue.take(); len(got) != WarmJoining || queue.bytes != 0 {
		t.Fatalf("after take: %d messages, %d bytes held, want %d and 0", len(got), queue.bytes, WarmJoining)
	}
	leaving := warmPane("nht_leaving")
	leaving.fail("the saved transcript could not be read")
	leaving.close()
	if queue.bytes != 0 || len(queue.queue) != 0 {
		t.Fatalf("a thread that left still holds %d bytes in %d messages", queue.bytes, len(queue.queue))
	}
}
