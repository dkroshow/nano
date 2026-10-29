package app

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func TestSidebarDeltaRidesCatalogEventStream(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	s, _ := sidebarActiveFixture(t, now)

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

func TestSidebarActiveRejectsSnapshotRoute(t *testing.T) {
	s := &server{Registry: newSessionRegistry()}
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
	s, ids, _ := sidebarActiveFixtureWithStore(t, now)
	return s, ids
}

func sidebarActiveFixtureWithStore(t *testing.T, now time.Time) (*server, map[string]string, *history.Store) {
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

	stars, err := sidebar.OpenStars(filepath.Join(t.TempDir(), "stars.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stars.Close() })
	if err := fixtureSetStarred(t, stars, "codex", "starred-old", true); err != nil {
		t.Fatal(err)
	}
	attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "detached-old", AttachedAt: now.Format(time.RFC3339)})
	metadata, executions := newTestHostedControl(t)
	parentHosted, err := fixtureEnsureSessionWithIdentity(executions, "nht_related_parent", "codex", presentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	childHosted, err := fixtureEnsureSessionWithIdentity(executions, "nht_related_child", "codex", presentationShared, parentHosted.ID, ids["related-parent"], "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(parentHosted.ID, "sess-parent", "related-parent", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(childHosted.ID, "sess-child", "related-child", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureAppendOrchestrationAssignment(executions, childHosted.ID, "rel-1", &hostedstore.Assignment{
		AssignmentRole: "auditor", Focus: "Audit parent", RelatedHostedThreadID: parentHosted.ID,
		AuditTargets: []orchestrationAuditTarget{
			{HostedThreadID: parentHosted.ID},
			{HostedThreadID: "nht_missing"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	svc := newStoredCatalog(t, store)
	attached := memorySession(t, "sess-attached", "codex", "attached-recent", "ready", "/w/att", "gpt-5.6-sol", now)
	parentLive := memorySession(t, "sess-parent", "codex", "related-parent", "ready", "/w/related-parent", "gpt-5.6-sol", now, func(cfg *sessionConfig) { cfg.HostedThreadID = parentHosted.ID })
	if response := postTurn(t, parentLive, context.Background(), "parent-work", "working"); response.Code != http.StatusOK {
		t.Fatalf("parent working setup: %d %s", response.Code, response.Body)
	}
	childLive := memorySession(t, "sess-child", "codex", "related-child", "ready", "/w/related-child", "gpt-5.6-sol", now, func(cfg *sessionConfig) {
		cfg.HostedThreadID = childHosted.ID
		cfg.InvokerHostedThreadID = parentHosted.ID
	})
	if response := postTurn(t, childLive, context.Background(), "child-work", "working"); response.Code != http.StatusOK {
		t.Fatalf("child working setup: %d %s", response.Code, response.Body)
	}
	s := testServerWithSessions(&server{
		catalog: svc, Registry: newSessionRegistry(),
		attachments: attachments, threadStars: stars,
		controlMetadata: metadata, agentExecutions: executions,
		runtimeEvents: sidebar.NewRuntimeHub(), mailEvents: featuremail.NewHub(),
		mail: newTestAgentMessageStore(t),
	}, map[string]*session{
		"sess-attached": attached, "sess-parent": parentLive, "sess-child": childLive,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.sidebarOwner().StartSidebarMaterializer(ctx, now); err != nil {
		t.Fatal(err)
	}
	return s, ids, store
}

type historyCourt struct {
	tool  bool
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
		if item.tool {
			projected.Type = history.ItemCommand
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
	s.sidebarOwner().HandleSidebarThreads(rec, req)
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

// Catches: the sidebar showing an older hosted record for a native thread when
// the newer one's update time differs only past the trimmed zeros of the older
// one's: as text ".2374Z" sorts after the later ".237401Z". Boundary: the
// sidebar's choice among unclaimed records for one native thread.
func TestSidebarHostedRecordsPicksTheNewerRecordAcrossTrimmedFractions(t *testing.T) {
	store := newTestAgentExecutionStore(t, "sidebar-newest")
	t.Cleanup(func() { _ = store.Control().Close() })
	at := time.Date(2026, 10, 15, 12, 0, 4, 237400000, time.UTC)
	for id, stamp := range map[string]string{"nht_older": at.Format(time.RFC3339Nano), "nht_newer": at.Add(time.Microsecond).Format(time.RFC3339Nano)} {
		if _, err := fixtureEnsureSession(store, id, "codex", presentationShared); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_threads SET current_native_id='native-1' WHERE hosted_thread_id=?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Control().CanonicalDatabase().Exec(`UPDATE hosted_execution_sessions SET status=?,updated_at=? WHERE hosted_thread_id=?`, sessionStatusExited, stamp, id); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	store, err = newControlAgentExecutionStore(store.Control(), "sidebar-newest")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{agentExecutions: store}
	records, found := s.sidebarOwner().SidebarHostedRecords([]sidebar.Thread{{Thread: threadindex.Thread{Provider: "codex", NativeID: "native-1"}}})
	if !found[0] || records[0].ID != "nht_newer" {
		t.Fatalf("sidebar record = %q (found %v), want the newer nht_newer", records[0].ID, found[0])
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

// materializeBootSidebar gives boot's real controller the rows that define its
// priority order, through the catalog and star stores it reads in production.
func materializeBootSidebar(t *testing.T, s *server, threads []sidebar.Thread, now time.Time) {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "boot-sidebar.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if s.threadStars == nil {
		s.threadStars = openTestThreadStars(t)
	}
	roots := map[string]history.SourceRoot{}
	for _, row := range threads {
		root, ok := roots[row.Provider]
		if !ok {
			kind := history.RootCodexSessions
			switch row.Provider {
			case "claude":
				kind = history.RootClaudeProjects
			case "grok":
				kind = history.RootGrokSessions
			}
			root = history.SourceRoot{Provider: history.Provider(row.Provider), Kind: kind, Path: t.TempDir()}
			if _, err := store.EnsureRoot(context.Background(), root); err != nil {
				t.Fatal(err)
			}
			roots[row.Provider] = root
		}
		updated := now
		if row.UpdatedAt != nil {
			updated, _ = time.Parse(time.RFC3339Nano, *row.UpdatedAt)
		}
		seedSidebarThread(t, store, root, row.NativeID, "", history.ThreadMain, updated, []historyCourt{{final: true, text: "Finished"}})
		if row.Starred {
			if err := fixtureSetStarred(t, s.threadStars, row.Provider, row.NativeID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.catalog = newStoredCatalog(t, store)
	s.sidebar = nil
	if s.attachmentsControl != nil {
		s.attachmentsControl.Catalog = s.hostedCatalog()
		s.attachmentsControl.Runtime = s.sidebarOwner()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.sidebarOwner().StartSidebarMaterializer(ctx, now); err != nil {
		t.Fatal(err)
	}
}
