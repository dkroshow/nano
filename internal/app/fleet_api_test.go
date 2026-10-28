package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/lifecycle"
)

func TestHostedFleetUsesLifecycleAndMatchesPane(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		work lifecycle.Work
		want string
	}{
		{lifecycle.Idle, fleetIdle},
		{lifecycle.Accepted, fleetWorking},
		{lifecycle.Working, fleetWorking},
		{lifecycle.WaitingOnYou, fleetNeedsYou},
	} {
		t.Run(string(tc.work), func(t *testing.T) {
			metadata, executions := newTestHostedControl(t)
			record, err := executions.ensureSessionWithIdentity("nht_lifecycle_"+string(tc.work), "codex", presentationShared, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := executions.BindSession(record.ID, "session", "native-hosted", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "session", Provider: "codex", HostedThreadID: record.ID, CWD: "/work", CreatedAt: now}, "native-hosted", "model")
			sess := fixture.session
			switch tc.work {
			case lifecycle.Accepted:
				if response := postTurn(t, sess, context.Background(), "fleet-prompt", "working"); response.Code != http.StatusOK {
					t.Fatalf("accepted setup: %d %s", response.Code, response.Body)
				}
			case lifecycle.Working:
				fixture.activate(t)
			case lifecycle.WaitingOnYou:
				fixture.activate(t)
				fixture.emitAndWait(t, []byte(`{"id":77,"method":"item/commandExecution/requestApproval","params":{"threadId":"native-hosted","turnId":"fleet-turn","itemId":"fleet-command","command":"echo test"}}`))
				waitForClaude(t, "fleet waiting state", func() bool { return len(sess.PendingInteractionList()) == 1 })
			}
			sess.PublishProjectionRuntime()
			fake := &fakeFleetCatalog{
				fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []catalogThread{{
					ID: "nth_hosted", Provider: "codex", NativeID: "native-hosted", Kind: "main",
				}}}},
				// Deliberately contradict idle and waiting lifecycle states. Hosted
				// rows must never take their label from provider transcript prose.
				courts: map[string]fleetCourtMessage{"nth_hosted": {
					ItemType: "user_message", Head: "continue", Tail: "continue", LastActivity: now,
				}},
			}
			s := testServerWithSessions(&server{
				catalog: fake, Registry: newSessionRegistry(),
				controlMetadata: metadata, agentExecutions: executions,
			}, map[string]*session{sess.ID(): sess})
			s.runtime = s
			// The agents' transition feed labels the materialized row, which
			// carries the contradicting saved-file court beside the overlay.
			rows := []catalogThread{{
				ID: "nth_hosted", Provider: "codex", NativeID: "native-hosted", Kind: "main",
				court: fake.courts["nth_hosted"],
			}}
			s.attachRuntimeOverlayBatch(context.Background(), rows, s.sidebarRuntimeSnapshot())
			watcher := newFleetWatcher()
			watcher.resync(rows, now)
			if feed := watcher.observed["nth_hosted"]; feed != tc.want {
				t.Fatalf("fleet feed = %q from the saved file, want the lifecycle's %q", feed, tc.want)
			}
			items := listCatalog(t, s, "/api/v1/threads")
			if len(items) != 1 {
				t.Fatalf("items = %d", len(items))
			}
			fleet := fleetRow(t, items[0])
			if fleet == nil || fleet.Label != tc.want {
				t.Fatalf("fleet = %#v, want %q", fleet, tc.want)
			}
			if pane := projectionStateForTest(fixture.projection).Runtime.ThreadStatus; pane != tc.want {
				t.Fatalf("pane = %q, rail = %q", pane, fleet.Label)
			}
			// The pane, the session list and the runtime overlay the sidebar
			// patches from deltas carry the one value the pane shows.
			var overlay, summary map[string]any
			if err := json.Unmarshal(items[0]["runtime"], &overlay); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(sess.Summary())
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &summary); err != nil {
				t.Fatal(err)
			}
			if overlay["threadStatus"] != tc.want || summary["threadStatus"] != tc.want {
				t.Fatalf("overlay threadStatus = %v, summary threadStatus = %v, want %q", overlay["threadStatus"], summary["threadStatus"], tc.want)
			}
			var fleetFields map[string]any
			if err := json.Unmarshal(items[0]["fleet"], &fleetFields); err != nil {
				t.Fatal(err)
			}
			if _, watched := fleetFields["watched"]; watched {
				t.Fatalf("hosted fleet was marked watched: %s", items[0]["fleet"])
			}
		})
	}

	t.Run("detached", func(t *testing.T) {
		metadata, executions := newTestHostedControl(t)
		record, err := executions.ensureSessionWithIdentity("nht_lifecycle_detached", "codex", presentationShared, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := executions.BindSession(record.ID, "", "native-hosted", sessionStatusExited, presentationShared); err != nil {
			t.Fatal(err)
		}
		fake := &fakeFleetCatalog{
			fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []catalogThread{{
				ID: "nth_hosted", Provider: "codex", NativeID: "native-hosted", Kind: "main",
			}}}},
			courts: map[string]fleetCourtMessage{"nth_hosted": {
				ItemType: "user_message", Head: "continue", Tail: "continue", LastActivity: now,
			}},
		}
		s := &server{
			catalog: fake, Registry: newSessionRegistry(),
			controlMetadata: metadata, agentExecutions: executions,
		}
		s.runtime = s
		items := listCatalog(t, s, "/api/v1/threads")
		if len(items) != 1 {
			t.Fatalf("items = %d", len(items))
		}
		fleet := fleetRow(t, items[0])
		if fleet == nil || fleet.Label != fleetIdle || fleet.Watched {
			t.Fatalf("detached hosted fleet = %#v, want lifecycle idle and not watched", fleet)
		}
	})
}

// fakeFleetCatalog is a catalog that can answer the classifier's court read, so
// a request produces real fleet labels rather than absent ones.
type fakeFleetCatalog struct {
	fakeThreadCatalog
	courts   map[string]fleetCourtMessage
	courtErr error
	asked    [][]string
}

func (f *fakeFleetCatalog) FleetCourt(_ context.Context, threadIDs []string) (map[string]fleetCourtMessage, error) {
	f.asked = append(f.asked, threadIDs)
	return f.courts, f.courtErr
}

func fleetRow(t *testing.T, item map[string]json.RawMessage) *fleetState {
	t.Helper()
	raw, ok := item["fleet"]
	if !ok {
		return nil
	}
	var state fleetState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return &state
}

// TestCatalogRowsCarryFleetState: durable rows are classified from their
// projected messages, memory rows — which have no projection at all — from the
// live session that produced them.
func TestCatalogRowsCarryFleetState(t *testing.T) {
	now := time.Now()
	// Durable rows arrive newest first, ahead of the memory rows' start times.
	updated := func(minutes int) *string {
		at := time.Date(2026, 8, 1, 12, minutes, 0, 0, time.UTC).Format(time.RFC3339Nano)
		return &at
	}
	fake := &fakeFleetCatalog{
		fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []catalogThread{
			{ID: "nth_working", Provider: "codex", NativeID: "native-working", Kind: "main", UpdatedAt: updated(4)},
			{ID: "nth_needs", Provider: "codex", NativeID: "native-needs", Kind: "main", UpdatedAt: updated(3)},
			{ID: "nth_old", Provider: "codex", NativeID: "native-old", Kind: "main", UpdatedAt: updated(2)},
			{ID: "nth_quiet", Provider: "codex", NativeID: "native-quiet", Kind: "main", UpdatedAt: updated(1)},
		}}},
		courts: map[string]fleetCourtMessage{
			"nth_working": {ItemType: "user_message", Head: "go", Tail: "go", LastActivity: now.Add(-time.Minute)},
			"nth_needs":   {ItemType: "agent_message", Head: "Done.", Tail: "Want me to deploy?", LastActivity: now.Add(-time.Hour)},
			"nth_old":     {ItemType: "user_message", Head: "go", Tail: "go", LastActivity: now.Add(-365 * 24 * time.Hour)},
		},
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	busy := memorySession(t, "busy", "claude", "native-busy", "ready", "/w/busy", "sonnet", base)
	if response := postTurn(t, busy, context.Background(), "busy-prompt", "working"); response.Code != http.StatusOK {
		t.Fatalf("busy setup: %d %s", response.Code, response.Body)
	}
	s := testServerWithSessions(&server{catalog: fake, Registry: newSessionRegistry()}, map[string]*session{
		"busy": busy,
		"new":  memorySession(t, "new", "codex", "", "starting", "/w/new", "", base.Add(time.Minute)),
	})
	s.runtime = s

	items := listCatalog(t, s, "/api/v1/threads")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got,
		[]string{"nth_working", "nth_needs", "nth_old", "nth_quiet", "mem_new", "mem_busy"}) {
		t.Fatalf("row ids = %v", got)
	}
	// nth_old's prompt has gone unanswered for a year: the agent owes, but no
	// turn is running, so it is not working.
	want := []string{fleetWorking, fleetNeedsYou, fleetIdle, fleetIdle, fleetIdle, fleetWorking}
	for index, label := range want {
		state := fleetRow(t, items[index])
		if state == nil || state.Label != label {
			t.Fatalf("row %d fleet = %#v, want %s", index, state, label)
		}
		if state.BallInCourt == "" {
			t.Fatalf("row %d carried a label with no explanation: %#v", index, state)
		}
	}
	// The classifier is asked about durable ids only: a memory row has no
	// durable row to read.
	if len(fake.asked) != 1 || !reflect.DeepEqual(fake.asked[0],
		[]string{"nth_working", "nth_needs", "nth_old", "nth_quiet"}) {
		t.Fatalf("court query = %#v", fake.asked)
	}
	// A live turn on a live child is direct evidence, so the memory row is
	// working with the agent owing the move.
	if state := fleetRow(t, items[5]); state.Court != fleetCourtAgentOwes {
		t.Fatalf("busy memory row = %#v", state)
	}
}

// TestCatalogFleetStateDegradesWithoutLosingMemoryRows: classification is
// best-effort. A failing court read costs the durable labels, never the page —
// and never the memory rows, whose evidence is not in SQLite at all.
func TestCatalogFleetStateDegradesWithoutLosingMemoryRows(t *testing.T) {
	fake := &fakeFleetCatalog{
		fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []catalogThread{
			{ID: "nth_one", Provider: "codex", NativeID: "native-one", Kind: "main", UpdatedAt: strPtr("2026-08-01T12:00:00Z")},
		}}},
		courtErr: errors.New("sqlite unavailable"),
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	s := testServerWithSessions(&server{catalog: fake, Registry: newSessionRegistry()}, map[string]*session{
		"new": memorySession(t, "new", "codex", "", "starting", "/w/new", "", base),
	})
	s.runtime = s
	items := listCatalog(t, s, "/api/v1/threads")
	if len(items) != 2 || fleetRow(t, items[0]) != nil {
		t.Fatalf("failed court read must leave the durable label absent: %v", items[0])
	}
	if state := fleetRow(t, items[1]); state == nil || state.Label != fleetIdle {
		t.Fatalf("memory row lost its label: %#v", state)
	}

	// A catalog with no court surface at all behaves the same way.
	plain := &server{catalog: &fakeThreadCatalog{page: catalogPage{Items: []catalogThread{
		{ID: "nth_one", Provider: "codex", NativeID: "native-one", Kind: "main"},
	}}}}
	if state := fleetRow(t, listCatalog(t, plain, "/api/v1/threads")[0]); state != nil {
		t.Fatalf("catalog without a court surface produced %#v", state)
	}
}

// fleetWatcherFixture builds a watcher with the transition log wired to a
// server.
func fleetWatcherFixture() (*server, *fleetWatcher) {
	s := &server{fleet: newFleetWatcher()}
	return s, s.fleet
}

// courtRows are materialized rows whose saved-file courts the test controls.
func courtRows(courts map[string]fleetCourtMessage) []catalogThread {
	rows := make([]catalogThread, 0, len(courts))
	for id, court := range courts {
		rows = append(rows, catalogThread{ID: id, Provider: "codex", NativeID: "native-" + id, Kind: "main", court: court})
	}
	return rows
}

func fleetChanges(t *testing.T, s *server, since string) fleetChangesResponse {
	t.Helper()
	target := "/api/v1/fleet/changes"
	if since != "" {
		target += "?since=" + since
	}
	recorder := httptest.NewRecorder()
	s.handleFleetChanges(recorder, httptest.NewRequest(http.MethodGet, target, nil))
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
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.fleet = startFleetWatcher(ctx, s.sidebar)
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
	c.publish(t, catalogChange{ThreadIDs: []string{id}})

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
		log.append(fleetTransition{ThreadID: "nth_one", From: fleetWorking, To: fleetIdle, At: at})
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
		log.append(fleetTransition{ThreadID: "nth_two", From: fleetWorking, To: fleetIdle, At: at})
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

// TestFleetChangesUnavailableWithoutWatcher: the endpoint reports the feed is
// unavailable rather than answering "nothing changed" from a feed that is not
// running.
func TestFleetChangesUnavailableWithoutWatcher(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&server{}).handleFleetChanges(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/fleet/changes", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	assertCatalogErrorCode(t, recorder, "fleet_unavailable")

	s, _ := fleetWatcherFixture()
	recorder = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/changes", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
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

	watcher.publish([]fleetTransition{{ThreadID: "nth_one", From: fleetWorking, To: fleetIdle}}, start.Add(time.Second))
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
		watcher.hub.notify(fleetTransition{ThreadID: "nth_flood"})
	}
	if len(subscriber) != fleetSubscriberBuffer {
		t.Fatalf("mirror buffer = %d, want a bounded %d", len(subscriber), fleetSubscriberBuffer)
	}
}
