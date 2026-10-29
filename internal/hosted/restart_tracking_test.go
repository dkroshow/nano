package hosted

import (
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/lifecycle"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
		tracker := NewRestartTracker(filepath.Join(t.TempDir(), RestartsFileName), "test-stamp", "boot-test", time.Now())
		tracker.AddThreads([]hostedstore.Attachment{{Provider: "claude", NativeThreadID: "thread"}})
		thread := tracker.Thread(hostedstore.AttachmentKey{Provider: "claude", NativeThreadID: "thread"})
		for _, event := range tc.events {
			if event == lifecycle.EventReadyIdle {
				thread.At(restartLive)
			} else {
				thread.Fail("exited")
			}
		}
		restarts, err := tracker.Recent(1, true)
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
	tracker := NewRestartTracker(filepath.Join(t.TempDir(), "missing-dir", RestartsFileName), "test-stamp", "boot-test", time.Now())
	tracker.MarkBootLoopDone()
	restarts, err := tracker.Recent(1, false)
	if err != nil || len(restarts) != 1 || restarts[0].RestartID != "boot-test" || !restarts[0].Complete {
		t.Fatalf("restarts after a failed write = %+v, %v", restarts, err)
	}
}

// The API used to call a restart complete once every thread was live, while
// a mark it waits for was still due: a Claude respawn goes live before its
// attached mark. Boundary: the tracker's marks and the API's reader, before
// and after the line is written.
func TestRestartRecordIsCompleteOnlyOnceWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), RestartsFileName)
	tracker := NewRestartTracker(path, "test-stamp", "boot-test", time.Now())
	tracker.AddThreads([]hostedstore.Attachment{{Provider: "claude", NativeThreadID: "thread"}})
	th := tracker.Thread(hostedstore.AttachmentKey{Provider: "claude", NativeThreadID: "thread"})
	th.At(restartRegistered)
	th.At(restartLive)
	tracker.MarkBootLoopDone()
	if restarts, _ := tracker.Recent(5, false); len(restarts) != 1 || restarts[0].Complete {
		t.Fatalf("restarts while the attached mark is due = %+v", restarts)
	}
	th.At(restartAttached)
	restarts, err := tracker.Recent(5, true)
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
	tracker := NewRestartTracker(filepath.Join(t.TempDir(), RestartsFileName), "test-stamp", "boot-test", time.Now())
	rec := hostedstore.Attachment{Provider: "codex", NativeThreadID: "resting", HostedThreadID: "nht_resting"}
	tracker.AddThreads([]hostedstore.Attachment{rec})
	tracker.Rest([]hostedstore.Attachment{rec})
	tracker.Thread(rec.Key()).At(restartPicked)
	tracker.Thread(rec.Key()).At(restartRegistered)
	tracker.MarkBootLoopDone()
	tracker.mu.Lock()
	record := tracker.recordLocked(true)
	tracker.mu.Unlock()
	if record.Complete || record.Pending != 1 || record.Resting != 0 {
		t.Fatalf("record during an on-demand attach = complete %v, pending %d, resting %d; want pending, not complete", record.Complete, record.Pending, record.Resting)
	}
	tracker.Thread(rec.Key()).At(restartLive)
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
	tracker := NewRestartTracker(filepath.Join(t.TempDir(), RestartsFileName), "test-stamp", "boot-test", time.Now())
	shown := hostedstore.Attachment{Provider: "codex", NativeThreadID: "shown", HostedThreadID: "nht_shown"}
	tracker.AddThreads([]hostedstore.Attachment{shown})
	tracker.Rest([]hostedstore.Attachment{shown})
	tracker.MarkBootLoopDone()
	tracker.Thread(shown.Key()).At(restartPicked) // a browser shows it after the loop
	tracker.Thread(shown.Key()).At(restartLive)
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
	tracker := NewRestartTracker(filepath.Join(t.TempDir(), RestartsFileName), "test-stamp", "boot-test", time.Now())
	first := hostedstore.Attachment{Provider: "codex", NativeThreadID: "first", HostedThreadID: "nht_first"}
	second := hostedstore.Attachment{Provider: "codex", NativeThreadID: "second", HostedThreadID: "nht_second"}
	tracker.AddThreads([]hostedstore.Attachment{first, second})
	tracker.Rest([]hostedstore.Attachment{first, second})
	tracker.MarkBootLoopDone()
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
	tracker.Thread(first.Key()).At(restartPicked)
	tracker.Thread(first.Key()).At(restartLive) // its attached mark is on its way
	if fire() {
		t.Fatal("written before the first attach's attached mark, within its grace")
	}
	tracker.Thread(second.Key()).At(restartPicked) // a browser shows the second
	tracker.Thread(first.Key()).At(restartAttached)
	if fire() {
		t.Fatal("written while the second attach is pending")
	}
	tracker.Thread(second.Key()).At(restartLive)
	tracker.Thread(second.Key()).At(restartAttached)
	fire()
	tracker.mu.Lock()
	written, record := tracker.written, tracker.recordLocked(true)
	tracker.mu.Unlock()
	if !written || record.Live != 2 || record.Pending != 0 {
		t.Fatalf("record = written %v, live %d, pending %d; want both browser attaches live", written, record.Live, record.Pending)
	}
}
