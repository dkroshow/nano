package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestWireSpoolReadBeforeOrderingAndHasMore(t *testing.T) {
	sp := newTestWireSpool(t)
	for i := int64(1); i <= 5; i++ {
		if err := sp.append(testWireEvent(i)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := sp.close(false); err != nil {
		t.Fatalf("close: %v", err)
	}

	events, hasMore, err := sp.readBefore(5, 2)
	if err != nil {
		t.Fatalf("readBefore: %v", err)
	}
	if !hasMore {
		t.Fatal("hasMore = false, want true")
	}
	assertSeqs(t, events, []int64{3, 4})
}

func TestWireSpoolCloseDeletesFile(t *testing.T) {
	sp := newTestWireSpool(t)
	path := sp.path
	if err := sp.append(testWireEvent(1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := sp.close(true); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("spool file still exists or stat failed: %v", err)
	}
}

func TestWireSpoolReadBeforeSkipsMalformedLines(t *testing.T) {
	sp := newTestWireSpool(t)
	if err := sp.append(testWireEvent(1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := sp.close(false); err != nil {
		t.Fatalf("close: %v", err)
	}
	f, err := os.OpenFile(sp.path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("{not-json}\n"); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	b, _ := json.Marshal(testWireEvent(2))
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatalf("write valid: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close append: %v", err)
	}

	events, hasMore, err := sp.readBefore(3, 10)
	if err != nil {
		t.Fatalf("readBefore: %v", err)
	}
	if hasMore {
		t.Fatal("hasMore = true, want false")
	}
	assertSeqs(t, events, []int64{1, 2})
}

func TestHandleEventsPageFallsBackToMemory(t *testing.T) {
	sess := &session{id: "s1",
		events:         []wireEvent{testWireEvent(1), testWireEvent(2), testWireEvent(3), testWireEvent(4)},
		seq:            4,
		subs:           map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/events/page?beforeSeq=4&limit=2", nil)
	rec := httptest.NewRecorder()
	sess.handleEventsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Events  []wireEvent `json:"events"`
		HasMore bool        `json:"hasMore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !res.HasMore {
		t.Fatal("hasMore = false, want true")
	}
	assertSeqs(t, res.Events, []int64{2, 3})
}

func TestRecordWireCapsMemoryBuffer(t *testing.T) {
	sess := &session{subs: map[chan wireEvent]struct{}{},
		lifecycleState: testLifecycleSnapshot("ready", false)}
	for i := 0; i < wireBufferCap+5; i++ {
		sess.recordWire(protocol.Send, []byte(`{"jsonrpc":"2.0","method":"test"}`))
	}
	if got := len(sess.events); got != wireBufferCap {
		t.Fatalf("len(events) = %d, want %d", got, wireBufferCap)
	}
	if got := sess.events[0].Seq; got != 6 {
		t.Fatalf("first seq = %d, want 6", got)
	}
	if got := sess.events[len(sess.events)-1].Seq; got != wireBufferCap+5 {
		t.Fatalf("last seq = %d, want %d", got, wireBufferCap+5)
	}
}

func newTestWireSpool(t *testing.T) *wireSpool {
	t.Helper()
	sp, err := newWireSpool(t.TempDir(), "session")
	if err != nil {
		t.Fatalf("newWireSpool: %v", err)
	}
	t.Cleanup(func() {
		_ = sp.close(true)
		_ = os.RemoveAll(filepath.Dir(sp.path))
	})
	return sp
}

func testWireEvent(seq int64) wireEvent {
	return wireEvent{
		Seq: seq,
		TS:  "2026-07-07T00:00:00Z",
		Dir: "recv",
		Msg: json.RawMessage(`{"jsonrpc":"2.0","method":"test"}`),
	}
}

func assertSeqs(t *testing.T, events []wireEvent, want []int64) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("len(events) = %d, want %d (%v)", len(events), len(want), events)
	}
	for i := range want {
		if events[i].Seq != want[i] {
			t.Fatalf("seq[%d] = %d, want %d (%v)", i, events[i].Seq, want[i], events)
		}
	}
}
