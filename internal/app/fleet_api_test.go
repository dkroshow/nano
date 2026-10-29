package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func TestHostedFleetUsesLifecycleAndMatchesPane(t *testing.T) {
	now := time.Date(2026, 9, 12, 7, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		work lifecycle.Work
		want string
	}{
		{lifecycle.Idle, catalog.FleetIdle},
		{lifecycle.Accepted, catalog.FleetWorking},
		{lifecycle.Working, catalog.FleetWorking},
		{lifecycle.WaitingOnYou, catalog.FleetNeedsYou},
	} {
		t.Run(string(tc.work), func(t *testing.T) {
			metadata, executions := newTestHostedControl(t)
			record, err := fixtureEnsureSessionWithIdentity(executions, "nht_lifecycle_"+string(tc.work), "codex", presentationShared, "", "", "")
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
				fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_hosted", Provider: "codex", NativeID: "native-hosted", Kind: "main"}}}}},
				// Deliberately contradict idle and waiting lifecycle states. Hosted
				// rows must never take their label from provider transcript prose.
				courts: map[string]catalog.FleetCourtMessage{"nth_hosted": {
					ItemType: "user_message", Head: "continue", Tail: "continue", LastActivity: now,
				}},
			}
			s := testServerWithSessions(&server{
				catalog: fake, Registry: newSessionRegistry(),
				controlMetadata: metadata, agentExecutions: executions,
			}, map[string]*session{sess.ID(): sess})
			wireFleetTestController(t, s, fake)
			// The feed and catalog share this composed row's lifecycle label;
			// the pane below independently observes the same session state.
			rows := []sidebar.Thread{{Thread: fake.page.Items[0].Thread}}
			s.sidebarOwner().ComposeCatalogThreads(context.Background(), rows, s.sidebarOwner().SidebarRuntimeSnapshot(), false)
			if rows[0].Fleet == nil || rows[0].Fleet.Label != tc.want {
				t.Fatalf("composed feed row = %+v, want %s", rows[0].Fleet, tc.want)
			}

			wireFleetTestController(t, s, fake)
			items := listCatalog(t, s, "/api/v1/threads")
			if len(items) != 1 {
				t.Fatalf("items = %d", len(items))
			}
			fleet := fleetRow(t, items[0])
			if fleet == nil || fleet.Label != tc.want {
				t.Fatalf("fleet = %#v, want %q", fleet, tc.want)
			}
			if pane := liveStatusForTest(t,fixture.session).Runtime.ThreadStatus; pane != tc.want {
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
		record, err := fixtureEnsureSessionWithIdentity(executions, "nht_lifecycle_detached", "codex", presentationShared, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := executions.BindSession(record.ID, "", "native-hosted", sessionStatusExited, presentationShared); err != nil {
			t.Fatal(err)
		}
		fake := &fakeFleetCatalog{
			fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_hosted", Provider: "codex", NativeID: "native-hosted", Kind: "main"}}}}},
			courts: map[string]catalog.FleetCourtMessage{"nth_hosted": {
				ItemType: "user_message", Head: "continue", Tail: "continue", LastActivity: now,
			}},
		}
		s := &server{
			catalog: fake, Registry: newSessionRegistry(),
			controlMetadata: metadata, agentExecutions: executions,
		}
		wireFleetTestController(t, s, fake)
		items := listCatalog(t, s, "/api/v1/threads")
		if len(items) != 1 {
			t.Fatalf("items = %d", len(items))
		}
		fleet := fleetRow(t, items[0])
		if fleet == nil || fleet.Label != catalog.FleetIdle || fleet.Watched {
			t.Fatalf("detached hosted fleet = %#v, want lifecycle idle and not watched", fleet)
		}
	})
}

// fakeFleetCatalog is a catalog that can answer the classifier's court read, so
// a request produces real fleet labels rather than absent ones.
type fakeFleetCatalog struct {
	fakeThreadCatalog
	courts   map[string]catalog.FleetCourtMessage
	courtErr error
}

func fleetRow(t *testing.T, item map[string]json.RawMessage) *catalog.FleetState {
	t.Helper()
	raw, ok := item["fleet"]
	if !ok {
		return nil
	}
	var state catalog.FleetState
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
		fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []sidebar.Thread{
			{Thread: threadindex.Thread{ID: "nth_working", Provider: "codex", NativeID: "native-working", Kind: "main", UpdatedAt: updated(4)}},
			{Thread: threadindex.Thread{ID: "nth_needs", Provider: "codex", NativeID: "native-needs", Kind: "main", UpdatedAt: updated(3)}},
			{Thread: threadindex.Thread{ID: "nth_old", Provider: "codex", NativeID: "native-old", Kind: "main", UpdatedAt: updated(2)}},
			{Thread: threadindex.Thread{ID: "nth_quiet", Provider: "codex", NativeID: "native-quiet", Kind: "main", UpdatedAt: updated(1)}},
		}}},
		courts: map[string]catalog.FleetCourtMessage{
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

	wireFleetTestController(t, s, fake)
	items := listCatalog(t, s, "/api/v1/threads")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got,
		[]string{fake.page.Items[0].ID, fake.page.Items[1].ID, fake.page.Items[2].ID, fake.page.Items[3].ID, "mem_new", "mem_busy"}) {
		t.Fatalf("row ids = %v", got)
	}
	// nth_old's prompt has gone unanswered for a year: the agent owes, but no
	// turn is running, so it is not working.
	want := []string{catalog.FleetWorking, catalog.FleetNeedsYou, catalog.FleetIdle, catalog.FleetIdle, catalog.FleetIdle, catalog.FleetWorking}
	for index, label := range want {
		state := fleetRow(t, items[index])
		if state == nil || state.Label != label {
			t.Fatalf("row %d fleet = %#v, want %s", index, state, label)
		}
		if state.BallInCourt == "" {
			t.Fatalf("row %d carried a label with no explanation: %#v", index, state)
		}
	}
	// SQL scaling is covered by the list-read budget tests. Here the real
	// catalog supplies every saved court and memory rows retain live evidence.
	// A live turn on a live child is direct evidence, so the memory row is
	// working with the agent owing the move.
	if state := fleetRow(t, items[5]); state.Court != catalog.CourtAgentOwes {
		t.Fatalf("busy memory row = %#v", state)
	}
}

// TestCatalogFleetStateDegradesWithoutLosingMemoryRows: classification is
// best-effort. A failing court read costs the durable labels, never the page —
// and never the memory rows, whose evidence is not in SQLite at all.
func TestCatalogFleetStateDegradesWithoutLosingMemoryRows(t *testing.T) {
	fake := &fakeFleetCatalog{
		fakeThreadCatalog: fakeThreadCatalog{page: catalogPage{Items: []sidebar.Thread{
			{Thread: threadindex.Thread{ID: "nth_one", Provider: "codex", NativeID: "native-one", Kind: "main", UpdatedAt: strPtr("2026-08-01T12:00:00Z")}},
		}}},
		courtErr: errors.New("sqlite unavailable"),
	}
	base := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	s := testServerWithSessions(&server{catalog: fake, Registry: newSessionRegistry()}, map[string]*session{
		"new": memorySession(t, "new", "codex", "", "starting", "/w/new", "", base),
	})
	wireFleetTestController(t, s, fake)
	items := listCatalog(t, s, "/api/v1/threads")
	if len(items) != 2 || fleetRow(t, items[0]) != nil {
		t.Fatalf("failed court read must leave the durable label absent: %v", items[0])
	}
	if state := fleetRow(t, items[1]); state == nil || state.Label != catalog.FleetIdle {
		t.Fatalf("memory row lost its label: %#v", state)
	}

	// A catalog with no court surface at all behaves the same way.
	plain := &server{catalog: &fakeThreadCatalog{page: catalogPage{Items: []sidebar.Thread{
		{Thread: threadindex.Thread{ID: "nth_one", Provider: "codex", NativeID: "native-one", Kind: "main"}},
	}}}}
	if state := fleetRow(t, listCatalog(t, plain, "/api/v1/threads")[0]); state != nil {
		t.Fatalf("catalog without a court surface produced %#v", state)
	}
}

// TestFleetChangesUnavailableWithoutWatcher: the endpoint reports the feed is
// unavailable rather than answering "nothing changed" from a feed that is not
// running.
func TestFleetChangesUnavailableWithoutWatcher(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&server{}).sidebarOwner().HandleFleetChanges(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/fleet/changes", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
	assertCatalogErrorCode(t, recorder, "fleet_unavailable")

	s := &server{}
	recorder = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/fleet/changes", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", recorder.Code)
	}
}

// wireFleetTestController keeps the HTTP page fixture, while saved-file court
// reads go through a concrete index over real projected messages.
func wireFleetTestController(t *testing.T, s *server, fixture *fakeFleetCatalog) {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "fleet.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	for i, row := range fixture.page.Items {
		court := fixture.courts[row.ID]
		updated := time.Now()
		if row.UpdatedAt != nil {
			updated, _ = time.Parse(time.RFC3339Nano, *row.UpdatedAt)
		}
		items := []historyCourt{{user: court.ItemType == "user_message", final: court.ItemType != "user_message", text: court.Tail}}
		if court.ItemType == "" {
			items = nil
		}
		if !court.LastActivity.IsZero() {
			updated = court.LastActivity
		}
		id := seedSidebarThread(t, store, root, row.NativeID, "", history.ThreadMain, updated, items)
		fixture.page.Items[i].ID = id
	}
	source := newStoredCatalog(t, store)
	s.sidebar = sidebar.New(sidebar.Sources{Catalog: source.Service, Registry: &s.Registry, Executions: s.agentExecutions, Metadata: s.controlMetadata, Attachments: s.attachments, Boot: &s.bootAttachments, Stars: s.threadStars, RuntimeEvents: s.runtimeEvents})
	if fixture.courtErr != nil {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
