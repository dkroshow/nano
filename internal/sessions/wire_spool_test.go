package sessions

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/wire"
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
	sess := sessionWithOwners(&Session{
		wire: Wire{
			events: []wire.Event{testWireEvent(1), testWireEvent(2), testWireEvent(3), testWireEvent(4)},
			seq:    4,
			subs:   map[chan wire.Event]struct{}{}},
		state: State{
			lifecycleState: testLifecycleSnapshot("ready", false)},
		identity: attachmentIdentity{id: "s1"},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/events/page?beforeSeq=4&limit=2", nil)
	rec := httptest.NewRecorder()
	sess.wire.HandleEventsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Events  []wire.Event `json:"events"`
		HasMore bool         `json:"hasMore"`
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
	sess := sessionWithOwners(&Session{
		wire: Wire{subs: map[chan wire.Event]struct{}{}},
		state: State{
			lifecycleState: testLifecycleSnapshot("ready", false)}})
	for i := 0; i < wireBufferCap+5; i++ {
		sess.wire.recordWire(protocol.Send, []byte(`{"jsonrpc":"2.0","method":"test"}`))
	}
	if got := len(sess.wire.events); got != wireBufferCap {
		t.Fatalf("len(events) = %d, want %d", got, wireBufferCap)
	}
	if got := sess.wire.events[0].Seq; got != 6 {
		t.Fatalf("first seq = %d, want 6", got)
	}
	if got := sess.wire.events[len(sess.wire.events)-1].Seq; got != wireBufferCap+5 {
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

func testWireEvent(seq int64) wire.Event {
	return wire.Event{
		Seq: seq,
		TS:  "2026-07-07T00:00:00Z",
		Dir: "recv",
		Msg: json.RawMessage(`{"jsonrpc":"2.0","method":"test"}`),
	}
}

func assertSeqs(t *testing.T, events []wire.Event, want []int64) {
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

// A configured spool path must never erase another application's files.
func TestPrepareWireSpoolDirPreservesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "notes.jsonl")
	if err := os.WriteFile(foreign, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "000bc899.jsonl")
	if err := os.WriteFile(legacy, []byte("keep this too"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWireSpoolDir(dir); err == nil {
		t.Fatal("accepted a non-empty, unmarked directory")
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "keep me" {
		t.Fatalf("foreign file changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(legacy); err != nil || string(got) != "keep this too" {
		t.Fatalf("mixed directory was partially cleaned: %q, %v", got, err)
	}
}

// The first upgrade must recognize the unmarked files older Nano builds wrote.
func TestPrepareWireSpoolDirUpgradesLegacyFiles(t *testing.T) {
	for _, directoryEntry := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%v", directoryEntry), func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"000bc899.jsonl", "1234abcd.jsonl", "ffffffff.jsonl"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if directoryEntry {
				if err := os.Mkdir(filepath.Join(dir, "01234567.jsonl"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := PrepareWireSpoolDir(dir)
			if directoryEntry {
				if err == nil {
					t.Fatal("accepted a legacy-named directory")
				}
				if _, err := os.Stat(filepath.Join(dir, "000bc899.jsonl")); err != nil {
					t.Fatalf("refused directory was partially cleaned: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("upgrade refused: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".nano-wire-spool" {
				t.Fatalf("upgrade contents = %v, %v; want only marker", entries, err)
			}
		})
	}
}

// Restart cleanup owns only marked Nano spool files, never the directory.
func TestPrepareWireSpoolDirRemovesOnlyStaleSpools(t *testing.T) {
	dir := t.TempDir()
	if _, err := PrepareWireSpoolDir(dir); err != nil {
		t.Fatal(err)
	}
	spool, err := newWireSpool(dir, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.close(false); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "notes.jsonl")
	if err := os.WriteFile(foreign, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareWireSpoolDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spool.path); !os.IsNotExist(err) {
		t.Fatalf("stale spool remains: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file removed: %v", err)
	}
}
