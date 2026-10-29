package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/testsupport/claudeshim"
	"github.com/dkroshow/nano/internal/threadindex"
)

// catalogRowsFixture describes stored rows, not an implementation of catalog reads.
type catalogRowsFixture struct {
	stored  *catalogStoreFixture
	page    sidebar.CatalogPage
	thread  sidebar.Thread
	listErr error
	indexed map[string]bool
	rescued map[string]sidebar.Thread
}

// Only legacy read-handler unit fixtures embed this status surface. HTTP list
// and overlay tests construct the real index with source().
func (*catalogRowsFixture) IndexStatus(context.Context) (threadindex.IndexStatus, error) {
	return threadindex.IndexStatus{}, nil
}
func (*catalogRowsFixture) RequestRefresh() bool { return true }

func TestThreadUsageEndpoints(t *testing.T) {
	fixture := newCatalogStoreFixture(t)
	fixture.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-1", Kind: "main"}})
	fixture.transcript(t, "nth_1", map[string]threadindex.TranscriptResponse{"": {ThreadID: "nth_1", Items: []threadindex.TranscriptItem{{ID: "m1", Type: "user_message", Text: "Prompt"}}}}, nil)
	if _, err := fixture.db.Exec(`UPDATE conversation_turns SET usage_contract=1,usage_input=11,usage_service_tier='priority'`); err != nil {
		t.Fatal(err)
	}
	s := &server{catalog: fixture.source}
	mux := http.NewServeMux()
	mountRoutes(mux, applicationRoutes(fixtureOwners(s)))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("thread usage status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("thread usage Cache-Control = %q", got)
	}
	var page history.ThreadUsagePage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.ThreadID != "nth_1" || len(page.Turns) != 1 || len(page.Turns[0].Messages) != 1 {
		t.Fatalf("thread usage = %#v", page)
	}
	if page.Turns[0].ServiceTier != "priority" || !bytes.Contains(rec.Body.Bytes(), []byte(`"serviceTier":"priority"`)) {
		t.Fatalf("thread usage service tier response = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/usage/threads?provider=codex", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fleet usage status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list history.ThreadUsageListPage
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].InputTokens == nil || *list.Items[0].InputTokens != 11 {
		t.Fatalf("fleet usage = %#v", list)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"serviceTier"`)) {
		t.Fatalf("fleet usage must not expose an aggregate service tier: %s", rec.Body.String())
	}
}

func TestCatalogListContractAndQuery(t *testing.T) {
	value := "high"
	evidence := "native_durable"
	fake := &catalogRowsFixture{page: sidebar.CatalogPage{
		Items:          []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-1", Kind: "subagent", ParentNativeID: "native-parent", Archived: true, Metadata: threadindex.Metadata{Effort: threadindex.Value{State: "known", Value: &value, Evidence: &evidence}}}}},
		CatalogVersion: 7,
	}}
	s := &server{catalog: (fake).source(t)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/threads?threadId=native-1&includeSubagents=true&includeGuardians=true&includeProgrammatic=true&archived=only&limit=999&cursor="+cursorAt(t, "2026-12-01T00:00:00Z", "nth_newer"), nil)
	query, err := threadindex.ParseCatalogListQuery(req)
	if err != nil {
		t.Fatal(err)
	}
	fixtureOwners(s).catalogViews().HandleThreads(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if query.ThreadID != "native-1" || !query.IncludeSubagents || !query.IncludeGuardians || !query.IncludeProgrammatic || query.Archived != "only" || query.Limit != 250 || query.Cursor == "" {
		t.Fatalf("query = %#v", query)
	}
	var body sidebar.CatalogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ParentNativeID != "native-parent" ||
		body.Items[0].Metadata.Effort.Value == nil || *body.Items[0].Metadata.Effort.Value != "high" || body.CatalogVersion != 7 {
		t.Fatalf("body = %#v", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, max-age=0, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("catalog validators = etag %q vary %q", etag, rec.Header().Get("Vary"))
	}
	conditional := httptest.NewRecorder()
	conditionalRequest := httptest.NewRequest(http.MethodGet, req.URL.String(), nil)
	conditionalRequest.Header.Set("If-None-Match", etag)
	fixtureOwners(s).catalogViews().HandleThreads(conditional, conditionalRequest)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("conditional catalog response = %d body %q", conditional.Code, conditional.Body.String())
	}
}

func TestCatalogHostedAssignmentOverlayAndUnavailable(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	invoker, _, err := fixtureBeginSession(store, "parent", "parent", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := fixtureBeginSessionWithInvoker(store, "worker", "worker", "claude", presentationShared, invoker, &hostedstore.Assignment{
		AssignmentRole: "worker", Focus: "Implement the catalog overlay",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSession(worker.ID, "", "claude-native", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if err := control.ClaimNativeID(worker.ID, "claude", "claude-native", false); err != nil {
		t.Fatal(err)
	}
	fake := &catalogRowsFixture{page: sidebar.CatalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_worker", Provider: "claude", NativeID: "claude-native", Kind: "thread", Metadata: threadindex.Metadata{}}}}}}
	s := &server{catalog: (fake).source(t), agentExecutions: store, controlMetadata: control, runtimeEvents: sidebar.NewRuntimeHub(), Registry: newSessionRegistry()}

	readCatalog := func() sidebar.Thread {
		t.Helper()
		rec := httptest.NewRecorder()
		fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("catalog status = %d %s", rec.Code, rec.Body.String())
		}
		var page sidebar.CatalogPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("catalog items = %#v", page.Items)
		}
		return page.Items[0]
	}

	assigned := readCatalog()
	hosted := fixtureOwners(s).hostedViews().SessionView(worker.ID)
	if !reflect.DeepEqual(assigned.Assignment, hosted.Assignment) || assigned.Assignment == nil || assigned.Assignment.AssignmentRole != "worker" {
		t.Fatalf("catalog assignment = %#v, hosted assignment = %#v", assigned.Assignment, hosted.Assignment)
	}
	if assigned.AssignmentUnavailable {
		t.Fatalf("assigned catalog row marked unavailable: %#v", assigned)
	}

	nudges := s.runtimeEvents.Subscribe()
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/worker/assignment", bytes.NewBufferString(`{"clientActionId":"clear-assignment","assignment":null}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear assignment = %d %s", rec.Code, rec.Body.String())
	}
	select {
	case delta := <-nudges:
		if !delta.Refresh {
			t.Fatalf("assignment update delta = %#v, want forced refresh", delta)
		}
	default:
		t.Fatal("assignment update did not nudge catalog refetch")
	}
	cleared := readCatalog()
	if cleared.Assignment != nil || cleared.AssignmentUnavailable {
		t.Fatalf("cleared catalog assignment = %#v unavailable=%v", cleared.Assignment, cleared.AssignmentUnavailable)
	}

	s.controlMetadataErr = "metadata unavailable"
	s.sidebar = nil
	unavailable := readCatalog()
	if unavailable.Assignment != nil || !unavailable.AssignmentUnavailable {
		t.Fatalf("unavailable catalog assignment = %#v unavailable=%v", unavailable.Assignment, unavailable.AssignmentUnavailable)
	}
}

func TestCatalogListRejectsInvalidQuery(t *testing.T) {
	s := &server{catalog: (&catalogRowsFixture{}).source(t)}
	for _, target := range []string{
		"/api/v1/threads?updatedFrom=not-a-time",
		"/api/v1/threads?archived=maybe",
		"/api/v1/threads?includeSubagents=sometimes",
		"/api/v1/threads?includeGuardians=sometimes",
		"/api/v1/threads?includeProgrammatic=sometimes",
		"/api/v1/threads?limit=0",
		"/api/v1/threads?threadId=" + strings.Repeat("a", 256+1),
	} {
		rec := httptest.NewRecorder()
		fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
		assertCatalogErrorCode(t, rec, "invalid_query")
	}
}

func TestCatalogDetailNotFoundAndEmptySources(t *testing.T) {
	fake := &catalogRowsFixture{}
	s := &server{catalog: (fake).source(t)}
	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	assertCatalogErrorCode(t, rec, "thread_not_found")

	fake.thread = sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "claude", NativeID: "native-1", Kind: "main"}}
	fake.stored.row(t, fake.thread)
	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nth_1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if sources, ok := body["sources"].([]any); !ok || len(sources) != 0 {
		t.Fatalf("sources = %#v", body["sources"])
	}
}

func TestCatalogStatusRefreshAndUnavailable(t *testing.T) {
	fixture := newCatalogStoreFixture(t)
	for _, c := range []struct {
		interaction string
		n           int
	}{{"interactive", 5}, {"programmatic", 8}, {"unclassified", 2}} {
		for i := 0; i < c.n; i++ {
			fixture.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: fmt.Sprintf("nth_%s_%d", c.interaction, i), Interaction: c.interaction}})
		}
	}
	fixture.version(t, 3)
	s := &server{catalog: fixture.source}
	rec := httptest.NewRecorder()
	threadindex.NewHandlers(s.catalog).HandleCatalogIndexStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/thread-index/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var statusBody threadindex.IndexStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatal(err)
	}
	if statusBody.InteractionCounts.Programmatic != 8 || statusBody.InteractionCounts.Interactive != 5 || statusBody.InteractionCounts.Unclassified != 2 {
		t.Fatalf("interaction counts = %#v", statusBody.InteractionCounts)
	}
	if statusBody.TranscriptChangeWorkers != 2 || statusBody.TranscriptChangeBacklog != 0 || statusBody.TranscriptChangeInFlight != 0 {
		t.Fatalf("transcript change status = %#v", statusBody)
	}

	rec = httptest.NewRecorder()
	threadindex.NewHandlers(s.catalog).HandleCatalogRefresh(rec, httptest.NewRequest(http.MethodPost, "/api/v1/thread-index/refresh", nil))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"accepted":true`) {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	fixtureOwners(&server{}).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status = %d", rec.Code)
	}
	assertCatalogErrorCode(t, rec, "catalog_unavailable")
}

func TestCatalogBackendErrorsDoNotLeak(t *testing.T) {
	s := &server{catalog: (&catalogRowsFixture{listErr: errors.New("private parser detail")}).source(t)}
	rec := httptest.NewRecorder()
	fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); got == "" || contains(got, "private parser detail") {
		t.Fatalf("leaked body = %q", got)
	}
}

func TestCatalogInvalidCursorReturnsBadRequest(t *testing.T) {
	s := &server{catalog: (&catalogRowsFixture{listErr: threadindex.ErrInvalidThreadCatalogCursor}).source(t)}
	rec := httptest.NewRecorder()
	fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads?cursor=bad", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	assertCatalogErrorCode(t, rec, "invalid_cursor")
}

func TestCatalogFrontendContractFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/thread_catalog/threads-page.json")
	if err != nil {
		t.Fatal(err)
	}
	var page sidebar.CatalogPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 6 || page.CatalogVersion != 12 || page.NextCursor == nil {
		t.Fatalf("fixture summary = items:%d version:%d cursor:%v", len(page.Items), page.CatalogVersion, page.NextCursor)
	}
	states := map[string]bool{}
	named, unnamed := 0, 0
	for _, item := range page.Items {
		states[item.Metadata.Priority.State] = true
		if item.Name == nil {
			unnamed++
			continue
		}
		named++
		if item.Name.Value == "" || item.Name.Provenance == "" || item.Name.Evidence != "user_assigned" {
			t.Fatalf("fixture name overlay = %#v", item.Name)
		}
	}
	if !states["known"] || !states["unknown"] || !states["unsupported"] {
		t.Fatalf("fixture priority states = %#v", states)
	}
	// Names are an overlay: present on the named row, null (never fabricated) elsewhere.
	if named != 1 || unnamed != 5 {
		t.Fatalf("fixture name coverage: named=%d unnamed=%d", named, unnamed)
	}
}

func TestCatalogFrontendDetailFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/thread_catalog/thread-detail.json")
	if err != nil {
		t.Fatal(err)
	}
	var thread sidebar.Thread
	if err := json.Unmarshal(raw, &thread); err != nil {
		t.Fatal(err)
	}
	if thread.ID == "" || thread.Provider != "claude" || thread.Sources == nil ||
		len(*thread.Sources) != 1 || len((*thread.Sources)[0].Diagnostics) != 1 {
		t.Fatalf("detail fixture = %#v", thread)
	}
	if thread.Name == nil || thread.Name.Value != "metadata boundary probe" ||
		thread.Name.Provenance != string(history.ThreadNameHistoricalImport) || thread.Name.Evidence != "user_assigned" {
		t.Fatalf("detail fixture name overlay = %#v", thread.Name)
	}
}

func TestSessionNameRenamesBeforeAndAfterNativeBind(t *testing.T) {
	catalog := newCatalogStoreFixture(t).source
	var s *server
	cfg := runtimeFixtureConfig(t, sessions.Config{ID: "session-1", Provider: "claude", HostedThreadID: "name-api", CreatedAt: time.Now(), NameThread: func(provider, native, name string) {
		func() {
			if err := catalog.AssignThreadNameIfEmpty(provider, native, name); err != nil {
				t.Fatal(err)
			}
		}()
	}})
	sess := configuredFixtureSession(t, cfg, "", "starting", "")
	s = testServerWithSessions(&server{catalog: catalog, Registry: newSessionRegistry()}, map[string]*sessions.Session{sess.ID(): sess})

	put := func(name string) sessions.Summary {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/sessions/session-1/name", strings.NewReader(`{"name":`+strconv.Quote(name)+`}`))
		applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("rename %q status = %d, body = %s", name, rec.Code, rec.Body.String())
		}
		var summary sessions.Summary
		if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
			t.Fatal(err)
		}
		return summary
	}

	if summary := put("  draft name  "); summary.Name != "draft name" {
		t.Fatalf("unbound summary name = %q", summary.Name)
	}
	if name := sess.Summary().Name; name != "draft name" {
		t.Fatalf("unbound name=%q", name)
	}

	sess.ReserveNativeID("native-1")
	if summary := put("bound name"); summary.Name != "bound name" {
		t.Fatalf("bound summary name = %q", summary.Name)
	}
	waitForCatalog(t, func() bool {
		name, _ := catalog.ThreadNameFor(context.Background(), "claude", "native-1")
		return name == "bound name"
	})
	if sess.Summary().Name != "bound name" {
		t.Fatal("bound session name lost")
	}

	// A later native bind must not replay a stale pre-bind title.
	connected := make(chan *claudeshim.Conn, 1)
	shim := newFakeShim(t, func(conn *claudeshim.Conn) {
		call, ok := conn.Next()
		if !ok {
			return
		}
		conn.Reply(call, map[string]any{"shimSessionId": "shim-name"})
		connected <- conn
		conn.AnswerRest()
	})
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "claude", ClaudeShimWS: shim.WSURL(), ClaudeBin: "/fake/claude", Cwd: t.TempDir()}, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	conn := <-connected
	waitForClaudeStatus(t, sess, "ready")
	emitClaudeInit(t, sess, conn, 1, "native-2", "")
	if name, ok := catalog.ThreadNameFor(context.Background(), "claude", "native-2"); ok {
		t.Fatalf("later bind replayed staged name: %q", name)
	}
}

// A never-prompted Claude session carries its shim handle as a provisional
// native id. A rename in that window must stage like the pre-bind case: a
// catalog write would key the name to a handle captureInit is about to retire,
// and clearing pendingName would leave applyPendingName nothing to write when
// the real id lands — the name would vanish with the first prompt. The staged
// rename must also reach the provisional record, so a Nano restart in this
// window cannot lose it.
func TestSessionNameStagesOnProvisionalIdentity(t *testing.T) {
	catalog := newCatalogStoreFixture(t).source
	store := newTestStore(t)
	store.Remember(hostedstore.Attachment{
		Provider: "claude", NativeThreadID: "cs_shim", ShimSessionID: "cs_shim",
		Cwd: "/w", AttachedAt: "2026-08-06T10:00:00Z",
	})
	var s *server
	fixture := newProviderRuntimeFixture(t, sessions.Config{ID: "session-1", Provider: "claude", CreatedAt: time.Now(), NameThread: func(provider, native, name string) {
		func() {
			if err := catalog.AssignThreadNameIfEmpty(provider, native, name); err != nil {
				t.Fatal(err)
			}
		}()
	}}, "cs_shim", "")
	sess := fixture.session
	s = testServerWithSessions(&server{catalog: catalog, attachments: store, Registry: newSessionRegistry()}, map[string]*sessions.Session{sess.ID(): sess})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/sessions/session-1/name", strings.NewReader(`{"name":"nano api fable"}`))
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("provisional rename status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if name := sess.Summary().Name; name != "nano api fable" {
		t.Fatalf("provisional rename name=%q", name)
	}
	if name, ok := catalog.ThreadNameFor(context.Background(), "claude", "cs_shim"); ok {
		t.Fatalf("provisional rename wrote %q", name)
	}
	if stored, ok := store.Get("claude", "cs_shim"); !ok || stored.PendingName != "nano api fable" {
		t.Fatalf("provisional record pendingName = %q, %v; want staged rename", stored.PendingName, ok)
	}
	emitClaudeInit(t, sess, fixture.claude, fixture.seq+1, "native-real", "")
	waitForCatalog(t, func() bool {
		name, _ := catalog.ThreadNameFor(context.Background(), "claude", "native-real")
		return name == "nano api fable"
	})
}

// memorySession builds a live session with a fixed creation time, so the
// memory rows a request produces are deterministic.
func memorySession(t *testing.T, id, provider, threadID, status, cwd, model string, createdAt time.Time, configure ...func(*sessions.Config)) *sessions.Session {
	t.Helper()
	cfg := sessions.Config{ID: id, Provider: provider, CWD: cwd, CreatedAt: createdAt}
	for _, fn := range configure {
		fn(&cfg)
	}
	if status == "ready" {
		return newProviderRuntimeFixture(t, cfg, threadID, model).session
	}
	sess := newFixtureSession(cfg, threadID)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	if status == "exited" {
		sess.Close()
	}
	return sess
}

// memoryRowServer wires a catalog holding two durable rows — one whose thread
// the scanner has indexed, one present on the page only — against five live
// sessions covering every merge decision.
func memoryRowServer(t *testing.T, corrupt ...bool) *server {
	t.Helper()
	fake := &catalogRowsFixture{
		page: sidebar.CatalogPage{Items: []sidebar.Thread{
			{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-indexed", Kind: "main", UpdatedAt: strPtr("2026-07-28T12:00:01Z")}},
			{Thread: threadindex.Thread{ID: "nth_2", Provider: "claude", NativeID: "native-onpage", Kind: "main", UpdatedAt: strPtr("2026-07-28T12:00:00Z")}},
		}},
		indexed: map[string]bool{"codex/native-indexed": true},
	}
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	metadata, executions := newTestHostedControl(t)
	source := fake.source(t)
	if len(corrupt) > 0 {
		fake.stored.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: "nth_new", Provider: "claude", NativeID: "native-new", Kind: "subagent"}})
		fake.stored.corrupt(t, "native-new")
	}
	s := &server{catalog: source, Registry: newSessionRegistry(), controlMetadata: metadata, agentExecutions: executions}
	for index, fixture := range []struct{ id, provider, native, status, cwd, model string }{
		{"aaa", "codex", "", "starting", "/w/start", ""},
		{"bbb", "claude", "native-new", "ready", "/w/new", "sonnet"},
		{"ccc", "codex", "native-indexed", "ready", "/w/idx", "gpt-5"},
		{"ddd", "claude", "native-onpage", "ready", "/w/page", "sonnet"},
		{"eee", "codex", "native-dead", "exited", "/w/dead", "gpt-5"},
	} {
		hostedID := "nht_memory_" + fixture.id
		record, err := fixtureEnsureSessionWithIdentity(executions, hostedID, fixture.provider, presentationShared, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		generation, err := metadata.BeginTransportBinding(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fixture.cwd != "" {
			if _, err := metadata.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: record.ID, Field: "cwd", Value: fixture.cwd, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
				t.Fatal(err)
			}
		}
		if fixture.model != "" {
			if _, err := metadata.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: record.ID, Field: postureFieldModel, Value: fixture.model, Disposition: metadataDispositionConfirmed, Source: postureSourceProvider, Freshness: metadataFreshnessLive, AttachmentGeneration: generation}); err != nil {
				t.Fatal(err)
			}
		}
		sess := memorySession(t, fixture.id, fixture.provider, fixture.native, fixture.status, fixture.cwd, fixture.model, base.Add(time.Duration(index)*time.Minute), func(cfg *sessions.Config) { cfg.HostedThreadID = hostedID; cfg.Metadata = metadata })
		s.RegisterSession(sess)
	}
	return s
}

func listCatalog(t *testing.T, s *server, target string) []map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	fixtureOwners(s).catalogViews().HandleThreads(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status = %d, body = %s", target, rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Items
}

func catalogRowIDs(items []map[string]json.RawMessage) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		var id string
		_ = json.Unmarshal(item["id"], &id)
		ids = append(ids, id)
	}
	return ids
}

// TestCatalogMemoryRowsMergeIntoFirstPage is the unified-catalog merge in one
// test: live sessions the scanner has not indexed appear as transient rows in
// list order (here after the newer durable ones, newest session first), while
// an indexed thread, a thread already on the page, and an exited session
// contribute nothing.
func TestCatalogMemoryRowsMergeIntoFirstPage(t *testing.T) {
	s := memoryRowServer(t)
	items := listCatalog(t, s, "/api/v1/threads")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got, []string{"nth_1", "nth_2", "mem_bbb", "mem_aaa"}) {
		t.Fatalf("row ids = %v", got)
	}

	// Durable rows never claim to be transient.
	for _, item := range items[:2] {
		if _, present := item["transient"]; present {
			t.Fatalf("durable row carried transient: %v", item)
		}
	}

	// The starting session's row, field by field: identity from the session,
	// cwd known from runtime evidence, model unknown until it is captured, and
	// an attached runtime overlay despite having no native id to match on.
	var starting sidebar.Thread
	raw, err := json.Marshal(items[3])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &starting); err != nil {
		t.Fatal(err)
	}
	if starting.Provider != "codex" || starting.NativeID != "" || starting.Kind != "thread" ||
		!starting.Transient || starting.Interaction != "unclassified" || starting.Archived {
		t.Fatalf("starting memory row = %#v", starting)
	}
	if starting.Name != nil || starting.Title != nil || starting.Preview != nil {
		t.Fatalf("unnamed starting memory row invented an overlay: %#v", starting)
	}
	if starting.CreatedAt == nil || *starting.CreatedAt != "2026-07-27T12:00:00Z" ||
		starting.UpdatedAt == nil || *starting.UpdatedAt != *starting.CreatedAt {
		t.Fatalf("starting memory row times = %v %v", starting.CreatedAt, starting.UpdatedAt)
	}
	if starting.Metadata.CWD.State != "known" || *starting.Metadata.CWD.Value != "/w/start" ||
		*starting.Metadata.CWD.Evidence != "canonical_hosted_metadata" {
		t.Fatalf("starting memory row cwd = %#v", starting.Metadata.CWD)
	}
	if starting.Metadata.Model.State != "unknown" || starting.Metadata.Model.Value != nil ||
		starting.Metadata.Effort.State != "unknown" || starting.Metadata.Origin.State != "unknown" {
		t.Fatalf("starting memory row metadata = %#v", starting.Metadata)
	}
	if starting.Runtime == nil || !starting.Runtime.Attached || starting.Runtime.Detached ||
		starting.Runtime.SessionID != "aaa" || starting.Runtime.Status != "starting" {
		t.Fatalf("starting memory row runtime = %#v", starting.Runtime)
	}

	// Wire spellings the frontend keys on.
	if got := string(items[3]["transient"]); got != "true" {
		t.Fatalf("transient JSON = %q", got)
	}
	if got := string(items[3]["runtime"]); got != `{"attached":true,"detached":false,"sessionId":"aaa","status":"starting","threadStatus":"idle","presentationMode":"shared"}` {
		t.Fatalf("memory row runtime JSON = %s", got)
	}

	// The ready-but-unindexed session keeps the model it already captured.
	var ready sidebar.Thread
	raw, err = json.Marshal(items[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.Provider != "claude" || ready.NativeID != "native-new" ||
		ready.Metadata.Model.State != "known" || *ready.Metadata.Model.Value != "sonnet" {
		t.Fatalf("ready memory row = %#v", ready)
	}
}

func TestMemoryThreadRowCarriesDialogNameBeforeDurableIndexing(t *testing.T) {
	row := sidebar.MemoryThreadRow(sessions.Summary{
		ID: "session-1", Provider: "codex", Name: "sidebar name", CreatedAt: "2026-08-04T12:00:00Z",
	})
	if row.Name == nil || row.Name.Value != "sidebar name" ||
		row.Name.Provenance != "manual" || row.Name.Evidence != threadindex.ThreadNameEvidence {
		t.Fatalf("memory row name = %#v", row.Name)
	}
}

// TestCatalogMemoryRowsQueryScope pins where memory rows do and do not belong:
// never on a page whose cursor they precede (an earlier page carried them),
// never in the archived-only class, and an exact-id lookup answers with the
// matching live thread alone.
func TestCatalogMemoryRowsQueryScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   []string
	}{
		{"page after the live rows", "/api/v1/threads?cursor=" + cursorAt(t, "2026-07-01T00:00:00Z", "nth_0"), []string{}},
		{"archived only", "/api/v1/threads?archived=only", []string{}},
		{"archived exclude still merges", "/api/v1/threads?archived=exclude", []string{"nth_1", "nth_2", "mem_bbb", "mem_aaa"}},
		{"exact id of a live unindexed thread", "/api/v1/threads?threadId=native-new", []string{"mem_bbb"}},
		{"exact id of an indexed thread", "/api/v1/threads?threadId=native-indexed", []string{"nth_1"}},
		{"exact id nobody holds", "/api/v1/threads?threadId=native-nowhere", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := catalogRowIDs(listCatalog(t, memoryRowServer(t), tc.target))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("row ids = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCatalogMemoryRowsSuppressedOnProbeFailure: an unreadable catalog must not
// answer with a row that duplicates one it cannot see.
func TestCatalogMemoryRowsSuppressedOnProbeFailure(t *testing.T) {
	s := memoryRowServer(t, true)
	got := catalogRowIDs(listCatalog(t, s, "/api/v1/threads"))
	// The identity-less starting session is unaffected: it has nothing to probe.
	if !reflect.DeepEqual(got, []string{"nth_1", "nth_2", "mem_aaa"}) {
		t.Fatalf("row ids = %v", got)
	}
}

// rescueServer wires the liveness-rescue fixture: a default-filtered page
// holding one interactive row, a live claude session whose durable row is
// programmatic-classified (the sdk-cli wrinkle — filtered off that page), and a
// detached codex attachment whose durable row is a subagent (likewise filtered).
// Both filtered threads are indexed, so no memory row can stand in for them.
func rescueServer(t *testing.T, corrupt ...bool) *server {
	t.Helper()
	fake := &catalogRowsFixture{
		page: sidebar.CatalogPage{Items: []sidebar.Thread{
			{Thread: threadindex.Thread{ID: "nth_1", Provider: "codex", NativeID: "native-visible", Kind: "main", Interaction: "interactive", UpdatedAt: strPtr("2026-07-28T12:00:02Z")}},
		}},
		indexed: map[string]bool{"codex/native-visible": true, "claude/native-prog": true},
		rescued: map[string]sidebar.Thread{
			"native-prog": {Thread: threadindex.Thread{ID: "nth_2", Provider: "claude", NativeID: "native-prog", Kind: "main", Interaction: "programmatic", UpdatedAt: strPtr("2026-07-28T12:00:01Z")}},
			"native-sub":  {Thread: threadindex.Thread{ID: "nth_3", Provider: "codex", NativeID: "native-sub", Kind: "subagent", Interaction: "interactive", UpdatedAt: strPtr("2026-07-28T12:00:00Z")}},
		},
	}
	store, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "native-sub", AttachedAt: "2026-07-27T12:00:00Z"})
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	source := fake.source(t)
	if len(corrupt) > 0 {
		fake.stored.corrupt(t, "native-prog")
	}
	s := testServerWithSessions(&server{catalog: source, attachments: store, Registry: newSessionRegistry()}, map[string]*sessions.Session{
		"aaa": memorySession(t, "aaa", "codex", "native-visible", "ready", "/w/vis", "gpt-5", base),
		"bbb": memorySession(t, "bbb", "claude", "native-prog", "ready", "/w/prog", "sonnet", base.Add(time.Minute)),
	})
	return s
}

func rescueRow(t *testing.T, item map[string]json.RawMessage) sidebar.Thread {
	t.Helper()
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var thread sidebar.Thread
	if err := json.Unmarshal(raw, &thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

// TestCatalogLivenessRescuesFilteredRows: liveness outranks the fetch-time
// filters. An attached thread whose durable row is programmatic and a detached
// thread whose durable row is a subagent both belong on the default page, with
// their runtime state and their true interaction intact.
func TestCatalogLivenessRescuesFilteredRows(t *testing.T) {
	s := rescueServer(t)
	items := listCatalog(t, s, "/api/v1/threads")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got, []string{"nth_1", "nth_2", "nth_3"}) {
		t.Fatalf("row ids = %v", got)
	}

	attached := rescueRow(t, items[1])
	if attached.Interaction != "programmatic" || attached.Kind != "main" {
		t.Fatalf("rescued row lost its true classification: %#v", attached)
	}
	if attached.Runtime == nil || !attached.Runtime.Attached || attached.Runtime.SessionID != "bbb" ||
		attached.Runtime.Status != "ready" {
		t.Fatalf("rescued attached row runtime = %#v", attached.Runtime)
	}

	detached := rescueRow(t, items[2])
	if detached.Kind != "subagent" {
		t.Fatalf("rescued row lost its true kind: %#v", detached)
	}
	if detached.Runtime == nil || detached.Runtime.Attached || !detached.Runtime.Detached ||
		detached.Runtime.SessionID != "" {
		t.Fatalf("rescued detached row runtime = %#v", detached.Runtime)
	}

}

// TestCatalogLivenessRescueScope pins where rescued rows do not appear: a page
// whose cursor they precede (an earlier page carried them), an exact threadId
// lookup (which states its own filters), and archived=only.
func TestCatalogLivenessRescueScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"page after the rescued rows", "/api/v1/threads?cursor=" + cursorAt(t, "2026-07-01T00:00:00Z", "nth_0")},
		{"exact id", "/api/v1/threads?threadId=native-prog"},
		{"archived only", "/api/v1/threads?archived=only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rescueServer(t)
			got := catalogRowIDs(listCatalog(t, s, tc.target))
			if !reflect.DeepEqual(got, []string{}) {
				t.Fatalf("row ids = %v, want []", got)
			}
		})
	}
}

// TestCatalogLivenessRescueDegradesOnError: a rescue is a repair, so a failing
// lookup costs the rescued rows, never the page.
func TestCatalogLivenessRescueDegradesOnError(t *testing.T) {
	s := rescueServer(t, true)
	got := catalogRowIDs(listCatalog(t, s, "/api/v1/threads"))
	if !reflect.DeepEqual(got, []string{"nth_1"}) {
		t.Fatalf("row ids = %v, want [nth_1]", got)
	}
}

func TestCatalogWorkerIncludesSupervisorOutsideWindow(t *testing.T) {
	fake := &catalogRowsFixture{
		page: sidebar.CatalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_worker", Provider: "claude", NativeID: "agent-worker", Kind: "subagent", ParentNativeID: "parent-session"}}}},
		rescued: map[string]sidebar.Thread{
			"parent-session": {Thread: threadindex.Thread{ID: "nth_parent", Provider: "claude", NativeID: "parent-session", Kind: "main"}},
		},
	}
	s := &server{catalog: (fake).source(t), Registry: newSessionRegistry()}
	items := listCatalog(t, s, "/api/v1/threads?includeSubagents=true&updatedFrom=2026-07-30T00:00:00Z")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got, []string{"nth_worker", "nth_parent"}) {
		t.Fatalf("row ids = %v, want worker plus supervisor context", got)
	}
}

func TestCatalogInvocationIncludesCrossProviderParentOutsideWindow(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	parent, _, err := fixtureBeginSession(store, "parent", "parent-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(parent.ID, "nano-parent", "native-parent", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.Session(parent.ID)
	child, _, err := fixtureBeginSessionWithInvoker(store, "child", "child-hash", "claude", presentationBackground, parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(child.ID, "nano-child", "native-child", "ready", presentationBackground); err != nil {
		t.Fatal(err)
	}
	fake := &catalogRowsFixture{
		page: sidebar.CatalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_child", Provider: "claude", NativeID: "native-child", Kind: "main"}}}},
		rescued: map[string]sidebar.Thread{
			"native-parent": {Thread: threadindex.Thread{ID: "nth_parent", Provider: "codex", NativeID: "native-parent", Kind: "main"}},
		},
	}
	s := &server{catalog: (fake).source(t), Registry: newSessionRegistry(), agentExecutions: store}
	items := listCatalog(t, s, "/api/v1/threads?updatedFrom=2026-07-30T00:00:00Z")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got, []string{"nth_child", "nth_parent"}) {
		t.Fatalf("row ids = %v, want child plus cross-provider invoker", got)
	}
}

func TestCatalogStandaloneDoesNotRescueImmutableInvoker(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := fixtureBeginSession(store, "parent", "parent", "codex", presentationShared)
	_ = store.BindSession(parent.ID, "nano-parent", "native-parent", "ready", presentationShared)
	parent, _ = store.Session(parent.ID)
	child, _, _ := fixtureBeginSessionWithInvoker(store, "child", "child", "grok", presentationShared, parent)
	_ = store.BindSession(child.ID, "nano-child", "native-child", "ready", presentationShared)
	if _, _, _, err := store.SetSidebarPlacement(child.ID, sidebarPlacementStandalone); err != nil {
		t.Fatal(err)
	}
	fake := &catalogRowsFixture{page: sidebar.CatalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_child", Provider: "grok", NativeID: "native-child", Kind: "main"}}}}, rescued: map[string]sidebar.Thread{
		"native-parent": {Thread: threadindex.Thread{ID: "nth_parent", Provider: "codex", NativeID: "native-parent", Kind: "main"}},
	}}
	s := &server{catalog: (fake).source(t), Registry: newSessionRegistry(), agentExecutions: store, controlMetadata: control}
	items := listCatalog(t, s, "/api/v1/threads?updatedFrom=2026-07-30T00:00:00Z")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got, []string{"nth_child"}) {
		t.Fatalf("row ids = %v, want standalone child only", got)
	}
}

func TestCatalogMutableRelationshipIncludesCurrentParentOutsideWindow(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	original, _, _ := fixtureBeginSession(store, "original", "original", "codex", presentationShared)
	newParent, _, _ := fixtureBeginSession(store, "new-parent", "new-parent", "claude", presentationShared)
	child, _, _ := fixtureBeginSessionWithInvoker(store, "child", "child", "codex", presentationShared, original, nil)
	if err := store.BindSession(original.ID, "nano-original", "native-original", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(newParent.ID, "nano-new", "native-new", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(child.ID, "nano-child", "native-child", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureAppendOrchestrationAssignment(store, child.ID, "move", &hostedstore.Assignment{
		AssignmentRole: "worker", Focus: "Move visually", RelatedHostedThreadID: newParent.ID,
	}); err != nil {
		t.Fatal(err)
	}
	fake := &catalogRowsFixture{
		page: sidebar.CatalogPage{Items: []sidebar.Thread{{Thread: threadindex.Thread{ID: "nth_child", Provider: "codex", NativeID: "native-child", Kind: "main"}}}},
		rescued: map[string]sidebar.Thread{
			"native-original": {Thread: threadindex.Thread{ID: "nth_original", Provider: "codex", NativeID: "native-original", Kind: "main"}},
			"native-new":      {Thread: threadindex.Thread{ID: "nth_new", Provider: "claude", NativeID: "native-new", Kind: "main"}},
		},
	}
	s := &server{catalog: (fake).source(t), Registry: newSessionRegistry(), agentExecutions: store, controlMetadata: control}
	items := listCatalog(t, s, "/api/v1/threads?updatedFrom=2026-07-30T00:00:00Z")
	if got := catalogRowIDs(items); !reflect.DeepEqual(got, []string{"nth_child", "nth_new"}) {
		t.Fatalf("row ids = %v, want child plus current visual parent", got)
	}
}

func assertCatalogErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != want {
		t.Fatalf("error code = %q, want %q", body.Error.Code, want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestThreadDetailAndTranscriptResolveReferences pins the id contract these two
// read paths share with /messages: an agent that pastes a native session id or
// an unambiguous prefix reaches the thread, and an ambiguous prefix is reported
// with its candidates rather than silently resolved.
func TestThreadDetailAndTranscriptResolveReferences(t *testing.T) {
	fixture := newCatalogStoreFixture(t)
	fixture.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: "nth_1", Provider: "claude", NativeID: "native-1", Kind: "main"}})
	fixture.row(t, sidebar.Thread{Thread: threadindex.Thread{ID: "nth_2", Provider: "claude", NativeID: "next-2", Kind: "main"}})
	s := &server{catalog: fixture.source}

	for _, reference := range []string{"native-1", "nat"} {
		rec := httptest.NewRecorder()
		applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/"+reference, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("detail %q status = %d body %q", reference, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"id":"nth_1"`) {
			t.Fatalf("resolved detail=%s", rec.Body.String())
		}

		rec = httptest.NewRecorder()
		threadindex.NewHandlers(s.catalog).HandleThreadTranscript(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/"+reference+"/transcript", nil), reference)
		if rec.Code != http.StatusOK {
			t.Fatalf("transcript %q status = %d body %q", reference, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"threadId":"nth_1"`) {
			t.Fatalf("resolved transcript=%s", rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/n", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous detail status = %d", rec.Code)
	}
	assertCatalogErrorCode(t, rec, "ambiguous_thread_id")

	rec = httptest.NewRecorder()
	applicationHandler(fixtureOwners(s)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/threads/nowhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown detail status = %d", rec.Code)
	}
	assertCatalogErrorCode(t, rec, "thread_not_found")
}

// assertOneLineageRead checks that the lineage pass read its one parent level
// in a single identity read naming exactly want.
// cursorAt encodes a list cursor at one position in the list order.
func cursorAt(t *testing.T, updated, id string) string {
	t.Helper()
	at, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := threadindex.EncodeThreadCatalogCursor(&history.ThreadCursor{UpdatedAt: &at, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return url.QueryEscape(encoded)
}
