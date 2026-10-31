package sidebar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func TestSidebarMaterializedGETDoesNotReadSources(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	// Losing the source after materialization must not change cached reads.
	s.catalog = nil
	getSidebarActive(t, s, now)
	getSidebarActive(t, s, now)
	wider := httptest.NewRecorder()
	widerRequest := httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads?updatedFrom="+now.Add(-60*24*time.Hour).Format(time.RFC3339), nil)
	s.HandleSidebarThreads(wider, widerRequest)
	if wider.Code != http.StatusOK {
		t.Fatalf("cached custom window status=%d", wider.Code)
	}

	rows := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	hostedID, _ := rows["related-child"]["hostedThreadId"].(string)
	events := s.Subscribe()
	defer s.Unsubscribe(events)
	s.ApplyMailDelta(featuremail.Delta{Counts: map[string]featuremail.Counts{
		hostedID: {Total: 2, Unread: 1, Seen: 1},
	}})
	select {
	case delta := <-events:
		if len(delta.Rows) != 1 || delta.Rows[0].HostedThreadID != hostedID {
			t.Fatalf("sidebar mail delta = %+v", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("materialized sidebar did not publish its row delta")
	}
	after := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	mail := after["related-child"]["mail"].(map[string]any)
	if mail["unread"] != float64(1) {
		t.Fatalf("cached mail patch = %v", mail)
	}

	events = s.Subscribe()
	defer s.Unsubscribe(events)
	sess := s.LookupSession("sess-attached")
	if response := testsupport.PostTurn(t, sess.Delivery(), context.Background(), "sidebar-working", "working"); response.Code != http.StatusOK {
		t.Fatalf("sidebar working setup: %d %s", response.Code, response.Body)
	}
	s.applyRuntimeDelta(s.runtimeDeltaSnapshot())
	select {
	case delta := <-events:
		found := false
		for _, row := range delta.Rows {
			found = found || row.NativeID == "attached-recent" && row.Fleet != nil && row.Fleet.Label == fleetWorking
		}
		if !found {
			t.Fatalf("sidebar runtime delta = %+v", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("materialized sidebar did not publish its runtime row delta")
	}

	events = s.Subscribe()
	defer s.Unsubscribe(events)
	s.mu.RLock()
	var completed Thread
	for _, row := range s.all {
		if row.NativeID == "attached-recent" {
			completed = row
			break
		}
	}
	version := s.version
	s.mu.RUnlock()
	completed.Runtime = nil
	s.applyCatalogRows([]Thread{completed}, version+1)
	select {
	case delta := <-events:
		if len(delta.RemovedIDs) != 1 || delta.RemovedIDs[0] != completed.ID {
			t.Fatalf("sidebar removal delta = %+v", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("materialized sidebar did not publish its removal delta")
	}
}

// A star or a transcript that looks busy kept threads Nano does not hold in the
// list as gray rows. A thread is active in Nano or it is history.
func TestSidebarActiveShowsOnlyThreadsActiveInNano(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	body := getSidebarActive(t, s, now)
	byNative := sidebarItemsByNative(t, body)
	if item := byNative["attached-recent"]; !sidebarFlag(item, "active") {
		t.Fatalf("attached row = %s", rawJSON(item))
	}
	for _, native := range []string{"idle-recent", "working-recent", "abandoned-recent", "detached-old", "starred-old", "pane-old", "parent-old", "working-child"} {
		if item, ok := byNative[native]; ok {
			t.Fatalf("thread Nano does not hold stayed in the sidebar: %s", rawJSON(item))
		}
	}
	if s.threadStars == nil || !s.threadStars.IsStarred("codex", "starred-old") {
		t.Fatal("the star itself must survive")
	}
	if int(body["historicalCount"].(float64)) < 1 {
		t.Fatalf("historicalCount = %v", body["historicalCount"])
	}
	// Keep the retired field literal so a reference scan cannot be bypassed by
	// constructing the old response key in pieces.
	removedPaneOverlayKey := "paneRuntime"
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		if _, ok := item[removedPaneOverlayKey]; ok {
			t.Fatalf("removed pane overlay field present: %s", rawJSON(item))
		}
	}
}

// A closed thread with an open sub-agent held a gray row, so closing it did not
// clear the sidebar. It goes to history even when starred, and the sub-agent
// takes its place.
func TestSidebarClosedThreadGoesToHistoryAndItsOpenChildMovesUp(t *testing.T) {
	thread := func(id, invoker string) Thread {
		return Thread{Thread: threadindex.Thread{ID: id, Provider: "codex", NativeID: id}, HostedThreadID: "hosted-" + id, InvokerHostedThreadID: invoker}
	}
	open := func(id, invoker string) Thread {
		row := thread(id, invoker)
		row.HostedStatus, row.Runtime = "ready", &RuntimeInfo{Attached: true, Status: "ready"}
		return row
	}
	closed := func(id, invoker string) Thread {
		row := thread(id, invoker)
		row.HostedStatus, row.Starred = sessions.StatusExited, true
		return row
	}
	result, nodes := projectSidebar([]Thread{
		open("supervisor", ""),
		closed("closed-owner", "hosted-supervisor"),
		open("grandchild", "hosted-closed-owner"),
		closed("closed-top", ""),
		open("child", "hosted-closed-top"),
	}, sidebarBaseQuery(), 1)

	parents := map[string]string{}
	for _, row := range result.Items {
		parents[row.ID] = ""
		if row.Sidebar.ParentID != nil {
			parents[row.ID] = *row.Sidebar.ParentID
		}
	}
	want := map[string]string{"supervisor": "", "grandchild": "supervisor", "child": ""}
	if !reflect.DeepEqual(parents, want) {
		t.Fatalf("sidebar parents = %v, want %v", parents, want)
	}
	if roots := []string{"supervisor", "child"}; !reflect.DeepEqual(result.Roots, roots) {
		t.Fatalf("roots = %v, want %v", result.Roots, roots)
	}
	if result.HistoricalCount != 2 {
		t.Fatalf("historicalCount = %d, want both closed threads", result.HistoricalCount)
	}
	page, err := pageSidebarHistory(nodes, "supervisor", nil, 10, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "closed-owner" {
		t.Fatalf("supervisor history = %+v, want the closed owner", page.Items)
	}
}

func TestSidebarKeepsRestingThreadsActive(t *testing.T) {
	root := Thread{Thread: threadindex.Thread{ID: "root", Provider: "codex", NativeID: "root-native"}, HostedThreadID: "hosted-root", Runtime: &RuntimeInfo{Detached: true, Status: "resting"}}
	child := Thread{Thread: threadindex.Thread{ID: "child", Provider: "codex", NativeID: "child-native"}, HostedThreadID: "hosted-child", InvokerHostedThreadID: root.HostedThreadID, Runtime: &RuntimeInfo{Detached: true, Status: "resting"}}
	detached := Thread{Thread: threadindex.Thread{ID: "detached", Provider: "codex", NativeID: "detached-native"}, HostedThreadID: "hosted-detached", Runtime: &RuntimeInfo{Detached: true}}
	s := &Controller{registry: newRegistry()}
	nodes, _ := s.assembleSidebarGraph(context.Background(), history.SidebarGraph{}, []Thread{root, child, detached}, SidebarRuntimeSnapshot{})
	included := map[string]bool{}
	for _, node := range nodes {
		included[node.thread.ID] = node.included
	}
	if !included["root"] || !included["child"] || included["detached"] {
		t.Fatalf("included = %v; want the resting root and child, not the detached thread", included)
	}
}

func TestSidebarActiveRelatedLineageBeatsInvokerAndNative(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, ids := sidebarActiveFixture(t, now)
	body := getSidebarActive(t, s, now)
	byNative := sidebarItemsByNative(t, body)
	related := byNative["related-child"]
	if sidebarString(related["sidebar"].(map[string]any), "parentId") != ids["related-parent"] {
		t.Fatalf("related parent not preferred: %s", rawJSON(related))
	}
}

func TestSidebarStandaloneSuppressesInvokerUntilAssignmentMovesThread(t *testing.T) {
	parent := Thread{Thread: threadindex.Thread{ID: "parent", Provider: "codex", NativeID: "parent-native"}, HostedThreadID: "hosted-parent"}
	child := Thread{Thread: threadindex.Thread{ID: "child", Provider: "grok", NativeID: "child-native"}, HostedThreadID: "hosted-child", InvokerHostedThreadID: parent.HostedThreadID, InvokerNativeThreadID: parent.NativeID, InvokerProvider: parent.Provider, SidebarPlacement: ownedthreads.SidebarPlacementStandalone}
	index := indexSidebarThreads([]Thread{parent, child})
	if index.byID[child.ID].parentID != "" {
		t.Fatalf("standalone child parent = %q", index.byID[child.ID].parentID)
	}
	child.Assignment = &hostedstore.AssignmentView{AssignmentRole: "worker", RelatedHostedThreadID: parent.HostedThreadID}
	index = indexSidebarThreads([]Thread{parent, child})
	if index.byID[child.ID].parentID != parent.ID {
		t.Fatalf("assigned standalone child parent = %q", index.byID[child.ID].parentID)
	}
}

func TestSidebarActiveBreaksCyclesAndMissingParents(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, ids := sidebarActiveFixture(t, now)
	body := getSidebarActive(t, s, now)
	byID := map[string]map[string]any{}
	for _, item := range body["items"].([]any) {
		row := item.(map[string]any)
		byID[row["id"].(string)] = row
	}
	a := byID[ids["cycle-a"]]
	b := byID[ids["cycle-b"]]
	if a == nil || b == nil {
		t.Fatalf("cycle rows missing: %s", rawJSON(body))
	}
	aParent := a["sidebar"].(map[string]any)["parentId"]
	bParent := b["sidebar"].(map[string]any)["parentId"]
	if (aParent != nil) == (bParent != nil) {
		t.Fatalf("cycle should break exactly one parent: a=%v b=%v", aParent, bParent)
	}
	broken := a
	if aParent != nil {
		broken = b
	}
	if broken["sidebar"].(map[string]any)["lineage"] != "cycle_broken" {
		t.Fatalf("cycle lineage = %s", rawJSON(broken))
	}
	expectedBrokenID := ids["cycle-a"]
	if ids["cycle-b"] < expectedBrokenID {
		expectedBrokenID = ids["cycle-b"]
	}
	if broken["id"] != expectedBrokenID {
		t.Fatalf("cycle broke %v, want lexical minimum %s", broken["id"], expectedBrokenID)
	}
	orphan := byID[ids["orphan-child"]]
	if orphan["sidebar"].(map[string]any)["parentId"] != nil {
		t.Fatalf("missing parent fabricated: %s", rawJSON(orphan))
	}
	seen := map[string]int{}
	for _, item := range body["items"].([]any) {
		seen[item.(map[string]any)["id"].(string)]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("duplicate canonical id %s count %d", id, count)
		}
	}
}

// The active-sidebar pipeline assembles its own graph and never passes through
// listCatalogPage, so it needs its own mail overlay pass: hosted rows
// with durable mail must carry counts for the compact rail's badges.
func TestSidebarActiveCarriesMailCounts(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	before := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	child, _ := before["related-child"]
	hostedID, _ := child["hostedThreadId"].(string)
	if hostedID == "" {
		t.Fatalf("related-child lost its hosted identity: %s", rawJSON(child))
	}
	if child["mail"] != nil {
		t.Fatalf("mail overlay present before any mail: %s", rawJSON(child))
	}
	create := featuremail.Create{ClientMessageID: "sb-1", SenderKey: "operator", SenderKind: "operator",
		Origin: "operator", SenderName: "Operator", RecipientHostedID: hostedID,
		Subject: "Badge", Body: "Badge fuel", CreatedAt: now}
	create.RequestHash = featuremail.RequestHashWithFeedback(hostedID, "Badge", "Badge fuel", nil, false, "")
	if _, _, err := s.mail.CreateMessage(create); err != nil {
		t.Fatal(err)
	}
	counts, err := s.mail.CountsFor([]string{hostedID})
	if err != nil {
		t.Fatal(err)
	}
	s.ApplyMailDelta(featuremail.Delta{Counts: counts})
	after := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	mail, _ := after["related-child"]["mail"].(map[string]any)
	if mail == nil || mail["total"] != float64(1) || mail["unread"] != float64(1) {
		t.Fatalf("sidebar row missing mail counts: %s", rawJSON(after["related-child"]))
	}
}
