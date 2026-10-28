package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/threadindex"

	"github.com/dkroshow/nano/internal/lifecycle"
)

// A restart used to leave only a log line with its total time, so nobody could
// tell which step or which thread was slow. Each boot must write one record
// with every thread's steps, its sidebar place and its failure reason, and
// serve it through the API. Boundary: the real boot loop and Codex handshake
// against a fake app server, the written restarts.jsonl line, and
// GET /api/v1/restarts.
func TestRestartRecordTracksEveryThreadOfABoot(t *testing.T) {
	daemon := newFakeAppServer(t)
	s, store := bootReattachServer(t, "", daemon.WSURL())
	path := filepath.Join(t.TempDir(), restartsFileName)
	s.restart = newRestartTracker(path, "test-stamp", "boot-test", time.Now())
	for _, native := range []string{"starred", "plain", "forgotten"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.remember(rec)
	}
	seedBootAttachments(t, s)
	// The boot loop queues only threads with open work.
	for _, hosted := range []string{"nht_starred", "nht_plain", "nht_forgotten"} {
		if _, _, err := s.agentExecutions.beginTurn(hosted, "nano:test:open-turn", "hash"); err != nil {
			t.Fatal(err)
		}
	}
	s.prepareBootReattach()
	now := time.Now().UTC()
	updated := now.Format(time.RFC3339Nano)
	threads := []catalogThread{{Thread: threadindex.Thread{ID: "nth_starred", Provider: "codex", NativeID: "starred", UpdatedAt: &updated}, HostedThreadID: "nht_starred", Starred: true}}
	s.attachRuntimeOverlayBatch(context.Background(), threads, s.sidebarRuntimeSnapshot())
	query := sidebarDefaultQuery(now)
	active, nodes := projectSidebar(threads, query, 1)
	s.sidebar = newSidebarMaterializedState()
	s.sidebar.replace(threads, query, active, nodes, 1)
	s.restart.markHTTPOpen()
	s.attachments.forget("codex", "forgotten")

	s.reattachOnBoot()

	var record restartRecord
	waitForClaude(t, "the written restart record", func() bool {
		data, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(data, &record) == nil
	})
	if !record.Complete || record.ThreadsTotal != 3 || record.Live != 2 || record.Failed != 1 || record.Pending != 0 || record.Visible != 1 {
		t.Fatalf("restart summary = %+v", record)
	}
	if record.HTTPOpenMs == nil || record.BootLoopMs == nil || record.VisibleLiveMs == nil || record.AllSettledMs == nil {
		t.Fatalf("restart times missing: %+v", record)
	}
	// A failed thread must not let the restart claim every thread went live.
	if record.AllLiveMs != nil {
		t.Fatalf("all_live_ms = %d with a failed thread", *record.AllLiveMs)
	}
	byNative := map[string]restartThreadRecord{}
	for _, th := range record.Threads {
		byNative[th.NativeThreadID] = th
	}
	starred := byNative["starred"]
	if !starred.Visible || starred.Tier != "starred" || *record.VisibleLiveMs != *starred.LiveMs {
		t.Fatalf("starred thread = %+v, visible live %d", starred, *record.VisibleLiveMs)
	}
	for _, native := range []string{"starred", "plain"} {
		th := byNative[native]
		for name, at := range map[string]*int64{"picked": th.PickedMs, "lock": th.LockMs, "registered": th.RegisteredMs, "attached": th.AttachedMs, "live": th.LiveMs} {
			if at == nil {
				t.Fatalf("%s thread has no %s time: %+v", native, name, th)
			}
		}
		if th.FailedMs != nil || th.Reason != "" {
			t.Fatalf("%s thread failed: %+v", native, th)
		}
	}
	if plain := byNative["plain"]; plain.Visible || plain.Tier != "unseen" {
		t.Fatalf("thread outside the sidebar = %+v", plain)
	}
	if forgotten := byNative["forgotten"]; forgotten.FailedMs == nil || forgotten.Reason != "no_longer_stored" || forgotten.LiveMs != nil {
		t.Fatalf("forgotten thread = %+v", forgotten)
	}
	if codex := record.ByProvider["codex"]; codex.Threads != 3 || codex.Live != 2 || codex.Failed != 1 || codex.Ms["reconnect"].N != 2 {
		t.Fatalf("codex totals = %+v", codex)
	}

	for query, wantThreads := range map[string]int{"": 0, "?threads=1": 3} {
		rec := httptest.NewRecorder()
		s.handleRestarts(rec, httptest.NewRequest(http.MethodGet, "/api/v1/restarts"+query, nil))
		var body struct{ Restarts []restartRecord }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Restarts) != 1 {
			t.Fatalf("GET restarts%s = %d %s", query, rec.Code, rec.Body.String())
		}
		if got := body.Restarts[0]; got.RestartID != record.RestartID || len(got.Threads) != wantThreads {
			t.Fatalf("GET restarts%s = %s, %d threads", query, got.RestartID, len(got.Threads))
		}
	}
	if strings.Count(strings.TrimSpace(string(mustRead(t, path))), "\n") != 0 {
		t.Fatal("one boot wrote more than one restart line")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A thread whose session exits before it is ever live used to stay pending
// until the 15-minute cap, and one that went live and then failed counted as
// live. The last of live and failed decides. Boundary: the session's
// lifecycle transition, where live and failure are recorded.
func TestRestartRecordTakesLiveAndFailureFromTheLifecycle(t *testing.T) {
	for _, tc := range []struct {
		events       []lifecycle.Event
		state        string
		live, failed int
	}{
		{[]lifecycle.Event{lifecycle.EventReadyIdle}, "live", 1, 0},
		{[]lifecycle.Event{lifecycle.EventRuntimeExited}, "failed", 0, 1},
		{[]lifecycle.Event{lifecycle.EventReadyIdle, lifecycle.EventRuntimeExited}, "failed", 0, 1},
	} {
		tracker := newRestartTracker(filepath.Join(t.TempDir(), restartsFileName), "test-stamp", "boot-test", time.Now())
		tracker.addThreads([]attachmentRecord{{Provider: "claude", NativeThreadID: "thread"}})
		thread := tracker.thread(attachmentKey{Provider: "claude", NativeThreadID: "thread"})
		for _, event := range tc.events {
			if event == lifecycle.EventReadyIdle {
				thread.at(restartLive)
			} else {
				thread.fail("exited")
			}
		}
		restarts, err := tracker.recent(1, true)
		if err != nil {
			t.Fatal(err)
		}
		record, th := restarts[0], restarts[0].Threads[0]
		if th.State != tc.state || record.Live != tc.live || record.Failed != tc.failed || (tc.failed == 1) != (th.Reason == "exited") {
			t.Fatalf("%v: thread = %+v, live %d failed %d", tc.events, th, record.Live, record.Failed)
		}
	}
}

// A record whose line cannot be appended used to vanish from the API, which
// then served only older restarts. Boundary: the tracker's write and the
// API's reader.
func TestRestartRecordStaysReadableWhenItsLineCannotBeWritten(t *testing.T) {
	tracker := newRestartTracker(filepath.Join(t.TempDir(), "missing-dir", restartsFileName), "test-stamp", "boot-test", time.Now())
	tracker.markBootLoopDone()
	restarts, err := tracker.recent(1, false)
	if err != nil || len(restarts) != 1 || restarts[0].RestartID != "boot-test" || !restarts[0].Complete {
		t.Fatalf("restarts after a failed write = %+v, %v", restarts, err)
	}
}

// The API used to call a restart complete once every thread was live, while
// a mark it waits for was still due: a Claude respawn goes live before its
// attached mark. Boundary: the tracker's marks and the API's reader, before
// and after the line is written.
func TestRestartRecordIsCompleteOnlyOnceWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), restartsFileName)
	tracker := newRestartTracker(path, "test-stamp", "boot-test", time.Now())
	tracker.addThreads([]attachmentRecord{{Provider: "claude", NativeThreadID: "thread"}})
	th := tracker.thread(attachmentKey{Provider: "claude", NativeThreadID: "thread"})
	th.at(restartRegistered)
	th.at(restartLive)
	tracker.markBootLoopDone()
	if restarts, _ := tracker.recent(5, false); len(restarts) != 1 || restarts[0].Complete {
		t.Fatalf("restarts while the attached mark is due = %+v", restarts)
	}
	th.at(restartAttached)
	restarts, err := tracker.recent(5, true)
	if err != nil || len(restarts) != 1 || !restarts[0].Complete || restarts[0].Threads[0].AttachedMs == nil {
		t.Fatalf("restarts once written = %+v, %v", restarts, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("restart line not written: %v", err)
	}
}

// Failure caught: a resting thread picked for an on-demand attach still
// counted as settled, so the record could be written complete before that
// attach went live or failed. Boundary: the restart tracker.
func TestRestartRecordWaitsForAnOnDemandAttach(t *testing.T) {
	tracker := newRestartTracker(filepath.Join(t.TempDir(), restartsFileName), "test-stamp", "boot-test", time.Now())
	rec := attachmentRecord{Provider: "codex", NativeThreadID: "resting", HostedThreadID: "nht_resting"}
	tracker.addThreads([]attachmentRecord{rec})
	tracker.rest([]attachmentRecord{rec})
	tracker.thread(rec.Key()).at(restartPicked)
	tracker.thread(rec.Key()).at(restartRegistered)
	tracker.markBootLoopDone()
	tracker.mu.Lock()
	record := tracker.recordLocked(true)
	tracker.mu.Unlock()
	if record.Complete || record.Pending != 1 || record.Resting != 0 {
		t.Fatalf("record during an on-demand attach = complete %v, pending %d, resting %d; want pending, not complete", record.Complete, record.Pending, record.Resting)
	}
	tracker.thread(rec.Key()).at(restartLive)
	tracker.mu.Lock()
	record = tracker.recordLocked(true)
	tracker.mu.Unlock()
	if record.Live != 1 || record.Pending != 0 {
		t.Fatalf("record after the attach = live %d, pending %d", record.Live, record.Pending)
	}
}

// Failure caught: with only resting threads, the record was written the
// moment the boot loop finished, before any browser asked for the threads it
// shows, so their attaches were never recorded. Boundary: the restart tracker.
func TestRestartRecordWaitsForBrowsersWhileThreadsRest(t *testing.T) {
	tracker := newRestartTracker(filepath.Join(t.TempDir(), restartsFileName), "test-stamp", "boot-test", time.Now())
	shown := attachmentRecord{Provider: "codex", NativeThreadID: "shown", HostedThreadID: "nht_shown"}
	tracker.addThreads([]attachmentRecord{shown})
	tracker.rest([]attachmentRecord{shown})
	tracker.markBootLoopDone()
	tracker.thread(shown.Key()).at(restartPicked) // a browser shows it after the loop
	tracker.thread(shown.Key()).at(restartLive)
	tracker.mu.Lock()
	written, record := tracker.written, tracker.recordLocked(true)
	tracker.mu.Unlock()
	if written || record.Live != 1 || record.Resting != 0 {
		t.Fatalf("record = written %v, live %d, resting %d; want unwritten with the browser's attach live", written, record.Live, record.Resting)
	}
}

// Failure caught: a timer armed while threads rested wrote the record after a
// later mark changed what it had to wait for: before a browser's attach had
// its attached mark, or while a second browser attach was still pending, so
// the record froze and later marks were lost. Boundary: the restart tracker.
func TestRestartRecordTimerReweighsLaterMarks(t *testing.T) {
	tracker := newRestartTracker(filepath.Join(t.TempDir(), restartsFileName), "test-stamp", "boot-test", time.Now())
	first := attachmentRecord{Provider: "codex", NativeThreadID: "first", HostedThreadID: "nht_first"}
	second := attachmentRecord{Provider: "codex", NativeThreadID: "second", HostedThreadID: "nht_second"}
	tracker.addThreads([]attachmentRecord{first, second})
	tracker.rest([]attachmentRecord{first, second})
	tracker.markBootLoopDone()
	// fire makes the quiet pass and fires the armed timer now.
	fire := func() bool {
		t.Helper()
		tracker.mu.Lock()
		tracker.lastMark = time.Now().Add(-2 * restartRestQuiet)
		if tracker.wake != nil {
			tracker.wake.Reset(0)
		}
		tracker.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		return tracker.written
	}
	tracker.thread(first.Key()).at(restartPicked)
	tracker.thread(first.Key()).at(restartLive) // its attached mark is on its way
	if fire() {
		t.Fatal("written before the first attach's attached mark, within its grace")
	}
	tracker.thread(second.Key()).at(restartPicked) // a browser shows the second
	tracker.thread(first.Key()).at(restartAttached)
	if fire() {
		t.Fatal("written while the second attach is pending")
	}
	tracker.thread(second.Key()).at(restartLive)
	tracker.thread(second.Key()).at(restartAttached)
	fire()
	tracker.mu.Lock()
	written, record := tracker.written, tracker.recordLocked(true)
	tracker.mu.Unlock()
	if !written || record.Live != 2 || record.Pending != 0 {
		t.Fatalf("record = written %v, live %d, pending %d; want both browser attaches live", written, record.Live, record.Pending)
	}
}
