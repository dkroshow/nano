package main

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
	s, store := bootReattachServer(t, "", daemon.wsURL())
	path := filepath.Join(t.TempDir(), restartsFileName)
	s.restart = newRestartTracker(path, "test-stamp", "boot-test", time.Now())
	for _, native := range []string{"starred", "plain", "forgotten"} {
		rec := bootCodexRecord(t)
		rec.NativeThreadID, rec.HostedThreadID = native, "nht_"+native
		store.remember(rec)
	}
	seedBootAttachments(t, s)
	s.prepareBootReattach()
	now := time.Now().UTC()
	updated := now.Format(time.RFC3339Nano)
	threads := []catalogThread{{ID: "nth_starred", Provider: "codex", NativeID: "starred", HostedThreadID: "nht_starred", Starred: true, UpdatedAt: &updated}}
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
		snapshot, err := lifecycle.New(1)
		if err != nil {
			t.Fatal(err)
		}
		sess := &session{lifecycleState: snapshot, restart: tracker.thread(attachmentKey{provider: "claude", nativeThreadID: "thread"})}
		sess.stateMu.Lock()
		for _, event := range tc.events {
			sess.transitionLifecycleLocked(event)
		}
		sess.stateMu.Unlock()
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
	th := tracker.thread(attachmentKey{provider: "claude", nativeThreadID: "thread"})
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
