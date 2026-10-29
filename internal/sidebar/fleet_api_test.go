package sidebar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threadindex"
)

// fleetWatcherFixture builds a watcher with the transition log wired to a
// server.
func fleetWatcherFixture() (*Controller, *fleetWatcher) {
	s := &Controller{fleet: newFleetWatcher()}
	return s, s.fleet
}

// courtRows are materialized rows whose saved-file courts the test controls.
func courtRows(courts map[string]fleetCourtMessage) []Thread {
	rows := make([]Thread, 0, len(courts))
	for id, court := range courts {
		rows = append(rows, Thread{Thread: threadindex.Thread{ID: id, Provider: "codex", NativeID: "native-" + id, Kind: "main"}, Court: court})
	}
	return rows
}

func fleetChanges(t *testing.T, s *Controller, since string) fleetChangesResponse {
	t.Helper()
	target := "/api/v1/fleet/changes"
	if since != "" {
		target += "?since=" + since
	}
	recorder := httptest.NewRecorder()
	s.HandleFleetChanges(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response fleetChangesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// The working bound is evaluated whenever the feed resyncs, so an abandoned
// prompt leaves working at the first resync past the bound, with no court
// change and no clock-driven work of its own.
func TestFleetChangesReportTheWorkingBoundAtTheNextResync(t *testing.T) {
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	s, watcher := fleetWatcherFixture()
	rows := courtRows(map[string]fleetCourtMessage{
		"nth_one": {ItemType: "user_message", Head: "go", Tail: "go", LastActivity: start},
	})

	// Seeding is silent: every thread is new to a fresh process.
	watcher.resync(rows, start.Add(time.Minute))
	seeded := fleetChanges(t, s, "")
	if !seeded.Resnapshot || seeded.Reason != fleetResnapshotNoBookmark || len(seeded.Transitions) != 0 {
		t.Fatalf("first call = %#v", seeded)
	}

	watcher.resync(rows, start.Add(365*24*time.Hour))
	abandoned := fleetChanges(t, s, seeded.Bookmark)
	if len(abandoned.Transitions) != 1 || abandoned.Transitions[0].From != fleetWorking || abandoned.Transitions[0].To != fleetIdle {
		t.Fatalf("resync past the bound = %#v", abandoned)
	}
}

// A row's label from its saved message is kept while the message is
// unchanged, so the feed must still see a new answer that asks a question.
func TestFleetChangesFollowANewSavedMessage(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	s, watcher := fleetWatcherFixture()
	watcher.resync(courtRows(map[string]fleetCourtMessage{
		"nth_one": {ItemType: "agent_message", Head: "Done.", Tail: "Done.", LastActivity: now},
	}), now)
	bookmark := fleetChanges(t, s, "").Bookmark

	watcher.resync(courtRows(map[string]fleetCourtMessage{
		"nth_one": {ItemType: "agent_message", Head: "Done.", Tail: "Want me to deploy?", LastActivity: now.Add(time.Minute)},
	}), now.Add(2*time.Minute))
	answered := fleetChanges(t, s, bookmark)
	if len(answered.Transitions) != 1 || answered.Transitions[0].From != fleetIdle || answered.Transitions[0].To != fleetNeedsYou {
		t.Fatalf("transitions = %#v, want idle→needs_you", answered.Transitions)
	}
}

// The feed once re-listed the catalog from SQLite after every change (6% of
// idle CPU). It now follows the materialized sidebar, so a committed catalog
// change must still reach the feed through the sidebar's own repair.
func TestFleetFeedFollowsTheMaterializedSidebar(t *testing.T) {
	s, c := startSidebarFixture(t)

	await := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("the feed never saw %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	await("the first sidebar build", func() bool {
		s.fleet.mu.Lock()
		defer s.fleet.mu.Unlock()
		return s.fleet.seeded
	})
	bookmark := fleetChanges(t, s, "").Bookmark

	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := c.store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	id := seedSidebarThread(t, c.store, root, "native-feed", "", history.ThreadMain, time.Now(),
		[]historyCourt{{user: true, text: "go"}})
	c.publish(t, catalog.Change{ThreadIDs: []string{id}})

	var latest fleetChangesResponse
	await("the new thread", func() bool {
		latest = fleetChanges(t, s, bookmark)
		return len(latest.Transitions) > 0
	})
	if len(latest.Transitions) != 1 || latest.Transitions[0].ThreadID != id ||
		latest.Transitions[0].From != "" || latest.Transitions[0].To != fleetWorking {
		t.Fatalf("transitions = %#v, want %s entering as working", latest.Transitions, id)
	}
}

// TestFleetChangeLogBookmarkContract: a bookmark this process cannot honour is
// answered with an explicit re-snapshot signal, never a silent gap.
func TestFleetChangeLogBookmarkContract(t *testing.T) {
	log := newFleetChangeLog()
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)

	fresh := log.since("")
	if !fresh.Resnapshot || fresh.Reason != fleetResnapshotNoBookmark {
		t.Fatalf("no bookmark = %#v", fresh)
	}
	// A bookmark from an empty log round-trips: nothing happened, nothing missed.
	if caught := log.since(fresh.Bookmark); caught.Resnapshot || len(caught.Transitions) != 0 {
		t.Fatalf("empty-log bookmark = %#v", caught)
	}

	for index := 0; index < 3; index++ {
		log.append(FleetTransition{ThreadID: "nth_one", From: fleetWorking, To: fleetIdle, At: at})
	}
	caught := log.since(fresh.Bookmark)
	if caught.Resnapshot || len(caught.Transitions) != 3 || caught.Transitions[0].Seq != 1 || caught.Transitions[2].Seq != 3 {
		t.Fatalf("missed polls = %#v", caught)
	}

	for _, tc := range []struct {
		name     string
		bookmark string
		reason   string
	}{
		{"another process's epoch", "deadbeef.2", fleetResnapshotUnknown},
		{"not a bookmark at all", "garbage", fleetResnapshotUnknown},
		{"a sequence this process never issued", log.epoch + ".99", fleetResnapshotUnknown},
		{"a non-numeric sequence", log.epoch + ".soon", fleetResnapshotUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := log.since(tc.bookmark)
			if !got.Resnapshot || got.Reason != tc.reason || len(got.Transitions) != 0 {
				t.Fatalf("response = %#v", got)
			}
			if got.Bookmark == "" {
				t.Fatal("a re-snapshot must still hand back a usable bookmark")
			}
		})
	}

	// A consumer that falls further behind than the ring retains is told to
	// re-snapshot rather than handed a partial history.
	for index := 0; index < fleetChangeRingSize+1; index++ {
		log.append(FleetTransition{ThreadID: "nth_two", From: fleetWorking, To: fleetIdle, At: at})
	}
	expired := log.since(caught.Bookmark)
	if !expired.Resnapshot || expired.Reason != fleetResnapshotExpired {
		t.Fatalf("expired bookmark = %#v", expired)
	}
	// The newest ring window is still served whole.
	recent := log.since(log.epoch + "." + strconv.FormatUint(log.next-2, 10))
	if recent.Resnapshot || len(recent.Transitions) != 1 {
		t.Fatalf("recent window = %#v", recent)
	}
}

// TestFleetTransitionsMirrorOntoTheCatalogStream: the SSE mirror carries the
// same payload the pull feed returns, best-effort and without replay.
func TestFleetTransitionsMirrorOntoTheCatalogStream(t *testing.T) {
	start := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	_, watcher := fleetWatcherFixture()
	watcher.resync(courtRows(map[string]fleetCourtMessage{
		"nth_one": {ItemType: "user_message", Head: "go", Tail: "go", LastActivity: start},
	}), start)
	subscriber := watcher.hub.subscribe()
	defer watcher.hub.unsubscribe(subscriber)

	watcher.publish([]FleetTransition{{ThreadID: "nth_one", From: fleetWorking, To: fleetIdle}}, start.Add(time.Second))
	select {
	case transition := <-subscriber:
		if transition.To != fleetIdle || transition.Seq != 1 {
			t.Fatalf("mirrored transition = %#v", transition)
		}
	default:
		t.Fatal("transition never reached the SSE mirror")
	}

	// A subscriber that stops reading is dropped from, never blocks, the sweep.
	for index := 0; index < fleetSubscriberBuffer+5; index++ {
		watcher.hub.notify(FleetTransition{ThreadID: "nth_flood"})
	}
	if len(subscriber) != fleetSubscriberBuffer {
		t.Fatalf("mirror buffer = %d, want a bounded %d", len(subscriber), fleetSubscriberBuffer)
	}
}
