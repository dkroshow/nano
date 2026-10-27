package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

type countingSidebarCatalog struct {
	threadCatalog
	source sidebarGraphSource
	mu     sync.Mutex
	reads  int
}

func (c *countingSidebarCatalog) ListSidebarGraph(ctx context.Context, params history.SidebarGraphParams) (history.SidebarGraph, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.source.ListSidebarGraph(ctx, params)
}

func (c *countingSidebarCatalog) LoadThreadsByIDs(ctx context.Context, ids []string) ([]history.Thread, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.source.LoadThreadsByIDs(ctx, ids)
}

func (c *countingSidebarCatalog) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

func TestSidebarMaterializedGETDoesNotReadSources(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	counting := &countingSidebarCatalog{threadCatalog: s.catalog, source: s.catalog.(sidebarGraphSource)}
	s.catalog = counting
	if err := s.materializeSidebar(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	before := counting.readCount()
	getSidebarActive(t, s, now)
	getSidebarActive(t, s, now)
	if got := counting.readCount(); got != before {
		t.Fatalf("two cached GETs performed %d source reads", got-before)
	}
	wider := httptest.NewRecorder()
	widerRequest := httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads?updatedFrom="+now.Add(-60*24*time.Hour).Format(time.RFC3339), nil)
	s.handleSidebarThreads(wider, widerRequest)
	if wider.Code != http.StatusOK || counting.readCount() != before {
		t.Fatalf("cached custom window status=%d source reads=%d", wider.Code, counting.readCount()-before)
	}

	rows := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	hostedID, _ := rows["related-child"]["hostedThreadId"].(string)
	events := s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)
	s.sidebar.applyMailDelta(mailDelta{Counts: map[string]agentMailCounts{
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
	if mail["unread"] != float64(1) || counting.readCount() != before {
		t.Fatalf("cached mail patch = %v, source reads %d", mail, counting.readCount()-before)
	}

	events = s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)
	sess := s.sessions["sess-attached"]
	sess.stateMu.Lock()
	sess.lifecycleState = testLifecycleSnapshot("ready", true)
	sess.stateMu.Unlock()
	s.sidebar.applyRuntimeDelta(s.runtimeDeltaSnapshot())
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
	if counting.readCount() != before {
		t.Fatalf("runtime patch performed %d source reads", counting.readCount()-before)
	}

	events = s.sidebar.events.subscribe()
	defer s.sidebar.events.unsubscribe(events)
	s.sidebar.mu.RLock()
	var completed catalogThread
	for _, row := range s.sidebar.all {
		if row.NativeID == "working-recent" {
			completed = row
			break
		}
	}
	version := s.sidebar.version
	s.sidebar.mu.RUnlock()
	completed.Fleet = &fleetState{ThreadID: completed.ID, Label: fleetIdle, Watched: true}
	s.sidebar.applyCatalogRows([]catalogThread{completed}, version+1)
	select {
	case delta := <-events:
		if len(delta.RemovedIDs) != 1 || delta.RemovedIDs[0] != completed.ID {
			t.Fatalf("sidebar removal delta = %+v", delta)
		}
	case <-time.After(time.Second):
		t.Fatal("materialized sidebar did not publish its removal delta")
	}
	if counting.readCount() != before {
		t.Fatalf("catalog row patch performed %d source reads", counting.readCount()-before)
	}
}

func TestSidebarDeltaRidesCatalogEventStream(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	service := s.catalog.(*threadCatalogService)
	service.hub = newCatalogHub()

	srv := httptest.NewServer(http.HandlerFunc(s.handleCatalogIndexEvents))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if event, _ := readSSEEvent(t, reader); event != "catalog" {
		t.Fatalf("first event = %q, want catalog", event)
	}
	event, data := readSSEEvent(t, reader)
	if event != "sidebar" || !strings.Contains(data, `"refresh":true`) {
		t.Fatalf("sidebar connect event = %q %q", event, data)
	}
}

func TestSidebarActiveSelectsHostedStarredAndRequiredAncestors(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, ids := sidebarActiveFixture(t, now)
	body := getSidebarActive(t, s, now)
	byNative := sidebarItemsByNative(t, body)
	if _, ok := byNative["idle-recent"]; ok {
		t.Fatalf("idle eligible row should be historical: %s", body)
	}
	if item := byNative["working-recent"]; !sidebarFlag(item, "active") || sidebarFlag(item, "structural") {
		t.Fatalf("working row = %s", rawJSON(item))
	}
	// An unanswered prompt whose file went quiet days ago is not a turn in
	// flight: it must not hold a place in the active sidebar as working.
	if item, ok := byNative["abandoned-recent"]; ok {
		t.Fatalf("abandoned prompt stayed in the active sidebar: %s", rawJSON(item))
	}
	if item := byNative["attached-recent"]; !sidebarFlag(item, "active") {
		t.Fatalf("attached row = %s", rawJSON(item))
	}
	if _, ok := byNative["detached-old"]; ok {
		t.Fatalf("unstarred detached row should be historical: %s", body)
	}
	if item := byNative["starred-old"]; !sidebarFlag(item, "active") || !item["starred"].(bool) {
		t.Fatalf("starred rescue missing: %s", rawJSON(item))
	}
	if _, ok := byNative["pane-old"]; ok {
		t.Fatalf("external pane row should be historical: %s", body)
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
	parent := byNative["parent-old"]
	if !sidebarFlag(parent, "structural") || sidebarFlag(parent, "active") {
		t.Fatalf("required ancestor should be structural: %s", rawJSON(parent))
	}
	child := byNative["working-child"]
	if sidebarString(child["sidebar"].(map[string]any), "parentId") != ids["parent-old"] {
		t.Fatalf("child parent = %s", rawJSON(child))
	}
	if int(body["historicalCount"].(float64)) < 1 {
		t.Fatalf("historicalCount = %v", body["historicalCount"])
	}
	if got := sidebarNumber(parent["sidebar"].(map[string]any), "historicalDescendantCount"); got != 1 {
		t.Fatalf("parent historical descendants = %v, want 1: %s", got, rawJSON(parent))
	}

}

func TestSidebarManualCloseOverridesWatchedFleetUntilTreeDisappears(t *testing.T) {
	watched := &fleetState{Label: fleetWorking, Watched: true}
	closed := catalogThread{
		HostedThreadID: "hosted-closed", HostedStatus: sessionStatusExited,
		Fleet: watched,
	}
	if sidebarIndependentlyActive(closed) {
		t.Fatal("an exited hosted row stayed active because its transcript looked working")
	}
	closed.Starred = true
	if !sidebarIndependentlyActive(closed) {
		t.Fatal("an explicit star should retain the closed row")
	}
	closed.Starred = false
	parent := catalogThread{ID: "parent", Provider: "codex", NativeID: "parent-native", HostedThreadID: "hosted-parent", HostedStatus: sessionStatusExited, Fleet: watched}
	child := catalogThread{ID: "child", Provider: "codex", NativeID: "child-native", HostedThreadID: "hosted-child", HostedStatus: "ready", InvokerHostedThreadID: parent.HostedThreadID, Fleet: watched}
	child.Runtime = &runtimeInfo{Attached: true, Status: "ready", TurnActive: true}
	s := &server{sessions: map[string]*session{}}
	nodes, _ := s.assembleSidebarGraph(context.Background(), history.SidebarGraph{}, []catalogThread{parent, child}, sidebarRuntimeSnapshot{})
	if !nodes[0].included || !nodes[0].structural || !nodes[1].included {
		t.Fatalf("closed parent should remain structural for active child: parent=%+v child=%+v", nodes[0], nodes[1])
	}
	child.HostedStatus = sessionStatusExited
	child.Runtime = nil
	nodes, _ = s.assembleSidebarGraph(context.Background(), history.SidebarGraph{}, []catalogThread{parent, child}, sidebarRuntimeSnapshot{})
	if nodes[0].included || nodes[1].included {
		t.Fatalf("fully closed unstarred tree stayed active: parent=%+v child=%+v", nodes[0], nodes[1])
	}
}

func TestSidebarDetachedTreeLeavesActiveWhileStarRemainsExplicit(t *testing.T) {
	root := catalogThread{
		ID: "root", Provider: "codex", NativeID: "root-native",
		HostedThreadID: "hosted-root", HostedStatus: sessionStatusExited,
	}
	child := catalogThread{
		ID: "child", Provider: "codex", NativeID: "child-native",
		HostedThreadID: "hosted-child", HostedStatus: agentSessionDetached,
		InvokerHostedThreadID: root.HostedThreadID,
		Runtime:               &runtimeInfo{Detached: true},
	}
	s := &server{sessions: map[string]*session{}}
	nodes, _ := s.assembleSidebarGraph(context.Background(), history.SidebarGraph{}, []catalogThread{root, child}, sidebarRuntimeSnapshot{})
	if nodes[0].included || nodes[1].included {
		t.Fatalf("fully detached unstarred tree stayed active: root=%+v child=%+v", nodes[0], nodes[1])
	}

	root.Starred = true
	nodes, _ = s.assembleSidebarGraph(context.Background(), history.SidebarGraph{}, []catalogThread{root, child}, sidebarRuntimeSnapshot{})
	if !nodes[0].included || !nodes[0].independent || nodes[0].structural {
		t.Fatalf("starred exited root was not explicitly pinned: root=%+v", nodes[0])
	}
	if nodes[1].included || nodes[0].activity.Active != 0 || nodes[0].historical != 1 {
		t.Fatalf("detached child should remain history under the pinned root: root=%+v child=%+v", nodes[0], nodes[1])
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
	parent := catalogThread{ID: "parent", Provider: "codex", NativeID: "parent-native", HostedThreadID: "hosted-parent"}
	child := catalogThread{
		ID: "child", Provider: "grok", NativeID: "child-native", HostedThreadID: "hosted-child",
		InvokerHostedThreadID: parent.HostedThreadID, InvokerNativeThreadID: parent.NativeID,
		InvokerProvider: parent.Provider, SidebarPlacement: sidebarPlacementStandalone,
	}
	index := indexSidebarThreads([]catalogThread{parent, child})
	if index.byID[child.ID].parentID != "" {
		t.Fatalf("standalone child parent = %q", index.byID[child.ID].parentID)
	}
	child.Assignment = &orchestrationAssignmentView{AssignmentRole: "worker", RelatedHostedThreadID: parent.HostedThreadID}
	index = indexSidebarThreads([]catalogThread{parent, child})
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

func TestSidebarHistoryPagesInactiveDescendants(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, ids := sidebarActiveFixture(t, now)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads/"+ids["parent-old"]+"/history?limit=1", nil)
	applicationHandler(s).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d body %s", rec.Code, rec.Body.String())
	}
	var page map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("history page = %s", rec.Body.String())
	}
	if sidebarFlag(items[0].(map[string]any), "active") {
		t.Fatalf("history returned an active row: %s", rec.Body.String())
	}
	itemSidebar := items[0].(map[string]any)["sidebar"].(map[string]any)
	if sidebarString(itemSidebar, "rootId") != ids["parent-old"] || sidebarNumber(itemSidebar, "depth") != 1 {
		t.Fatalf("history hierarchy is not canonical: %s", rec.Body.String())
	}
	ancestors, _ := itemSidebar["ancestorIds"].([]any)
	if len(ancestors) != 1 || ancestors[0] != ids["parent-old"] {
		t.Fatalf("history ancestors = %v", ancestors)
	}
	unknown := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads/nth_missing/history", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown parent = %d", unknown.Code)
	}
	bad := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads/"+ids["parent-old"]+"/history?cursor=not-a-cursor", nil))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor = %d", bad.Code)
	}
}

func TestSidebarActiveRejectsSnapshotRoute(t *testing.T) {
	s := &server{sessions: map[string]*session{}}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(s))
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fleet/snapshot", nil))
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"snapshot"`) {
		t.Fatalf("snapshot route still present: %s", rec.Body.String())
	}
}

func TestSidebarActiveBatchesHostedOverlays(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)
	body := getSidebarActive(t, s, now)
	byNative := sidebarItemsByNative(t, body)
	related := byNative["related-child"]
	assignment, _ := related["assignment"].(map[string]any)
	if assignment == nil || assignment["relatedHostedThreadId"] == nil {
		t.Fatalf("batched assignment missing: %s", rawJSON(related))
	}
	targets, _ := assignment["auditTargets"].([]any)
	if len(targets) != 2 {
		t.Fatalf("batched audit targets missing: %s", rawJSON(related))
	}
	known := targets[0].(map[string]any)
	missing := targets[1].(map[string]any)
	if known["unresolved"] == true || missing["unresolved"] != true {
		t.Fatalf("batched audit target resolution is wrong: %s", rawJSON(related))
	}

	parent := byNative["related-parent"]
	metadata, _ := parent["metadata"].(map[string]any)
	model, _ := metadata["model"].(map[string]any)
	if model["state"] != "unknown" {
		t.Fatalf("hosted row retained provider model without canonical metadata: %s", rawJSON(parent))
	}
}

func sidebarActiveFixture(t *testing.T, now time.Time) (*server, map[string]string) {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// A thread Nano does not hold is working only while its saved file moves.
	live := now.Add(-5 * time.Minute)
	recent := now.Add(-2 * 24 * time.Hour)
	old := now.Add(-40 * 24 * time.Hour)
	ids := map[string]string{}
	seed := func(native, parent string, kind history.ThreadKind, updated time.Time, items []historyCourt) {
		ids[native] = seedSidebarThread(t, store, root, native, parent, kind, updated, items)
	}
	seed("idle-recent", "", history.ThreadMain, recent, []historyCourt{{user: true, text: "hello"}, {final: true, text: "Done."}})
	seed("working-recent", "", history.ThreadMain, live, []historyCourt{{user: true, text: "please continue"}})
	seed("abandoned-recent", "", history.ThreadMain, recent, []historyCourt{{user: true, text: "please continue"}})
	seed("attached-recent", "", history.ThreadMain, recent, []historyCourt{{final: true, text: "Idle attached."}})
	seed("detached-old", "", history.ThreadMain, old, []historyCourt{{final: true, text: "Detached."}})
	seed("starred-old", "", history.ThreadMain, old, []historyCourt{{final: true, text: "Starred."}})
	seed("pane-old", "", history.ThreadMain, old, []historyCourt{{final: true, text: "Pane."}})
	seed("parent-old", "", history.ThreadMain, old, []historyCourt{{final: true, text: "Ancestor."}})
	seed("working-child", "parent-old", history.ThreadSubagent, live, []historyCourt{{user: true, text: "child work"}})
	seed("historical-child", "parent-old", history.ThreadSubagent, recent, []historyCourt{{final: true, text: "Done child."}})
	seed("related-parent", "", history.ThreadMain, recent, []historyCourt{{user: true, text: "parent live"}})
	seed("related-child", "wrong-native", history.ThreadMain, recent, []historyCourt{{user: true, text: "related live"}})
	seed("cycle-a", "cycle-b", history.ThreadMain, live, []historyCourt{{user: true, text: "cycle a"}})
	seed("cycle-b", "cycle-a", history.ThreadMain, live, []historyCourt{{user: true, text: "cycle b"}})
	seed("orphan-child", "missing-native", history.ThreadMain, live, []historyCourt{{user: true, text: "orphan"}})

	stars, err := openThreadStarsStore(filepath.Join(t.TempDir(), "stars.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stars.Close() })
	if err := stars.setStarred("codex", "starred-old", true); err != nil {
		t.Fatal(err)
	}
	attachments, err := newAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.remember(attachmentRecord{Provider: "codex", NativeThreadID: "detached-old", AttachedAt: now.Format(time.RFC3339)})
	metadata, executions := newTestHostedControl(t)
	parentHosted, err := executions.ensureSessionWithIdentity("nht_related_parent", "codex", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	childHosted, err := executions.ensureSessionWithIdentity("nht_related_child", "codex", presentationShared, parentHosted.ID, ids["related-parent"], "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.bindSession(parentHosted.ID, "sess-parent", "related-parent", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := executions.bindSession(childHosted.ID, "sess-child", "related-child", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := metadata.appendOrchestrationAssignment(childHosted.ID, "rel-1", &orchestrationAssignment{
		AssignmentRole: "auditor", Focus: "Audit parent", RelatedHostedThreadID: parentHosted.ID,
		AuditTargets: []orchestrationAuditTarget{
			{HostedThreadID: parentHosted.ID},
			{HostedThreadID: "nht_missing"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	svc := &threadCatalogService{store: store}
	attached := memorySession("sess-attached", "codex", "attached-recent", "ready", "/w/att", "gpt-5.6-sol", now)
	parentLive := memorySession("sess-parent", "codex", "related-parent", "ready", "/w/related-parent", "gpt-5.6-sol", now)
	parentLive.hostedThreadID = parentHosted.ID
	parentLive.lifecycleState = testLifecycleSnapshot("ready", true)
	childLive := memorySession("sess-child", "codex", "related-child", "ready", "/w/related-child", "gpt-5.6-sol", now)
	childLive.hostedThreadID = childHosted.ID
	childLive.invokerHostedThreadID = parentHosted.ID
	childLive.lifecycleState = testLifecycleSnapshot("ready", true)
	s := &server{
		catalog: svc, sessions: map[string]*session{
			"sess-attached": attached, "sess-parent": parentLive, "sess-child": childLive,
		},
		attachments: attachments, threadStars: stars,
		controlMetadata: metadata, agentExecutions: executions,
		runtimeEvents: newRuntimeHub(), mailEvents: newMailHub(),
		mail: newTestAgentMessageStore(t),
	}
	s.runtime = s
	if err := s.materializeSidebar(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	return s, ids
}

type historyCourt struct {
	user  bool
	final bool
	text  string
}

func seedSidebarThread(t *testing.T, store *history.Store, root history.SourceRoot, native, parent string, kind history.ThreadKind, updated time.Time, items []historyCourt) string {
	t.Helper()
	ctx := context.Background()
	src := testSidebarSource(root, native+".jsonl", native, kind, parent, &updated)
	applied, err := store.ApplySource(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := store.LoadSource(ctx, mustSidebarRootID(t, store, root), src.Source.Path)
	if err != nil || !ok {
		t.Fatalf("stored source = %v %v", ok, err)
	}
	turn := history.TranscriptTurn{Key: "ntr_" + native, Ordinal: 1, StartSeq: 1, EndSeq: int64(len(items)), Status: history.TurnCompleted, CompletionEvidence: history.CompletionTaskComplete}
	result := history.TranscriptProjectionResult{
		ProjectionKey: "ntp_" + native, CompleteOffset: 1, CompleteLines: len(items),
		ParserState: json.RawMessage(`{}`),
	}
	for index, item := range items {
		projected := history.TranscriptItem{
			ID: "nit_" + native + string(rune('a'+index)), ProjectedSeq: int64(index + 1),
			SourceKey: native + "-" + string(rune('a'+index)), TurnKey: turn.Key, TurnOrdinal: 1,
			Type: history.ItemUserMessage, Status: history.ItemStatusCompleted, Text: item.text,
			IsFinalAnswer: item.final, SourceLine: index + 1, SourceRowType: "test",
			SourceLineSHA256: strings.Repeat("0", 64),
		}
		if !item.user {
			projected.Type = history.ItemAgentMessage
		}
		if item.final {
			turn.FinalItemID = projected.ID
		}
		result.Items = append(result.Items, projected)
	}
	result.Turns = []history.TranscriptTurn{turn}
	if _, err := store.ApplyTranscript(ctx, history.TranscriptApplyRequest{
		Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID,
		SourceGeneration: stored.Generation, Provider: root.Provider,
		ParserVersion: "test-v1", CompletionRuleVersion: "test-rules-v1", Result: result,
	}); err != nil {
		t.Fatal(err)
	}
	return applied.ThreadID
}

func testSidebarSource(root history.SourceRoot, name, native string, kind history.ThreadKind, parent string, updated *time.Time) history.SourceProjection {
	created := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)
	indexed := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	model, evidence := "test-model", history.EvidenceNativeDurable
	cwd := "/w/" + native
	metadata := history.Metadata{
		CWD:    history.MetadataField{State: history.MetadataKnown, Value: &cwd, Evidence: &evidence},
		Model:  history.MetadataField{State: history.MetadataKnown, Value: &model, Evidence: &evidence},
		Effort: history.UnknownField(), Priority: history.UnknownField(), Permission: history.UnknownField(),
		HistoricalBranch: history.UnknownField(),
	}
	projection := history.ProjectionResult{
		Projection: history.ThreadProjection{
			Provider: root.Provider, NativeThreadID: native, IdentityEvidence: history.EvidenceNativeDurable,
			Kind: kind, ParentNativeID: parent, CreatedAt: &created, UpdatedAt: updated, Metadata: metadata,
			Title: strPtr(native),
		},
		CompleteOffset: 123, CompleteLines: 3,
	}
	return history.SourceProjection{
		Source: history.SourceCandidate{Root: root, Path: filepath.Join(root.Path, name),
			RelativePath: name, Kind: kind, Archived: root.Archived, Size: 123, ModTime: created,
			Device: 1, Inode: uint64(len(name) + int(name[0]))},
		Result: &projection, ParserVersion: "test-v1", Status: history.SourcePresent, IndexedAt: indexed,
	}
}

func mustSidebarRootID(t *testing.T, store *history.Store, root history.SourceRoot) int64 {
	t.Helper()
	roots, err := store.ListRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range roots {
		if item.Root.Provider == root.Provider && item.Root.Kind == root.Kind && item.Root.Path == root.Path {
			return item.ID
		}
	}
	t.Fatalf("root not found: %+v", root)
	return 0
}

func getSidebarActive(t *testing.T, s *server, now time.Time) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads?updatedFrom="+now.Add(-30*24*time.Hour).Format(time.RFC3339), nil)
	s.handleSidebarThreads(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sidebar active status = %d body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache control = %q", rec.Header().Get("Cache-Control"))
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func sidebarItemsByNative(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	items, _ := body["items"].([]any)
	for _, item := range items {
		row := item.(map[string]any)
		out[row["nativeId"].(string)] = row
	}
	return out
}

func sidebarFlag(row map[string]any, name string) bool {
	sidebar, _ := row["sidebar"].(map[string]any)
	flag, _ := sidebar[name].(bool)
	return flag
}

func sidebarString(sidebar map[string]any, name string) string {
	value, _ := sidebar[name].(string)
	return value
}

func sidebarNumber(sidebar map[string]any, name string) float64 {
	value, _ := sidebar[name].(float64)
	return value
}

func rawJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
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
	create := agentMessageCreate{ClientMessageID: "sb-1", SenderKey: operatorSenderKey, SenderKind: operatorSenderKind,
		Origin: operatorSenderKind, SenderName: operatorSenderName, RecipientHostedID: hostedID,
		Subject: "Badge", Body: "Badge fuel", CreatedAt: now}
	create.RequestHash = agentMessageRequestHashWithFeedback(hostedID, "Badge", "Badge fuel", nil, false, "")
	if _, _, err := s.mail.CreateMessage(create); err != nil {
		t.Fatal(err)
	}
	s.notifyMailChanged(hostedID)
	after := sidebarItemsByNative(t, getSidebarActive(t, s, now))
	mail, _ := after["related-child"]["mail"].(map[string]any)
	if mail == nil || mail["total"] != float64(1) || mail["unread"] != float64(1) {
		t.Fatalf("sidebar row missing mail counts: %s", rawJSON(after["related-child"]))
	}
}

// Catches: the sidebar showing an older hosted record for a native thread when
// the newer one's update time differs only past the trimmed zeros of the older
// one's: as text ".2374Z" sorts after the later ".237401Z". Boundary: the
// sidebar's choice among unclaimed records for one native thread.
func TestSidebarHostedRecordsPicksTheNewerRecordAcrossTrimmedFractions(t *testing.T) {
	store := newTestAgentExecutionStore(t, "sidebar-newest")
	t.Cleanup(func() { _ = store.control.Close() })
	at := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	store.sessions = map[string]agentSessionRecord{
		"nht_older": {ID: "nht_older", Provider: "codex", NativeThreadID: "native-1", Status: sessionStatusExited, UpdatedAt: at.Format(time.RFC3339Nano)},
		"nht_newer": {ID: "nht_newer", Provider: "codex", NativeThreadID: "native-1", Status: sessionStatusExited, UpdatedAt: at.Add(time.Microsecond).Format(time.RFC3339Nano)},
	}
	s := &server{agentExecutions: store}
	records, found := s.sidebarHostedRecords([]catalogThread{{Provider: "codex", NativeID: "native-1"}})
	if !found[0] || records[0].ID != "nht_newer" {
		t.Fatalf("sidebar record = %q (found %v), want the newer nht_newer", records[0].ID, found[0])
	}
}
