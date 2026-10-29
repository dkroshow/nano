package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// List reads must cost work in proportion to the page, not the fleet.
// budgetFixture builds one server over a real history store and a real control
// store with `extra` additional live hosted sessions: half of them indexed
// outside the list window (the liveness rescue's case), half not indexed at all
// (memory rows). It always holds one live thread on the page, one detached
// attachment, one hosted session whose latest operation failed, one with extra metadata and one whose hosted record is missing (metadata unavailable).
// Durable names of every provenance, a display-name fallback, a name under
// another provider for a codex session's native id, and a named and an unnamed
// hosted detached record cover which name each row takes.
type budgetFixture struct {
	server            *server
	catalog           *countingCatalog
	now               time.Time
	hostedBy          map[string]string // native id -> hosted id
	controlStatements atomic.Int64
}

// countingCatalog counts the catalog calls a request makes. It embeds the real
// service so every optional catalog interface still resolves.
type countingCatalog struct {
	*sidebar.CatalogSource
	lists, names atomic.Int64
}

func (c *countingCatalog) ListThreads(ctx context.Context, query threadindex.ListQuery) (sidebar.CatalogPage, error) {
	c.lists.Add(1)
	return c.CatalogSource.ListThreads(ctx, query)
}

func (c *countingCatalog) ThreadNameRefFor(ctx context.Context, provider, nativeThreadID string) (*history.ThreadNameRef, bool) {
	c.names.Add(1)
	return c.CatalogSource.ThreadNameRefFor(ctx, provider, nativeThreadID)
}

func (c *countingCatalog) ThreadNameFor(ctx context.Context, provider, nativeThreadID string) (string, bool) {
	c.names.Add(1)
	return c.CatalogSource.ThreadNameFor(ctx, provider, nativeThreadID)
}

func (c *countingCatalog) calls() int64 { return c.lists.Load() + c.names.Load() }

func newBudgetFixture(t *testing.T, extra int, followChanges bool) *budgetFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	root := history.SourceRoot{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}
	if _, err := store.EnsureRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	recent, old := now.Add(-2*24*time.Hour), now.Add(-40*24*time.Hour)
	seed := func(native string, updated time.Time) {
		seedSidebarThread(t, store, root, native, "", history.ThreadMain, updated, []historyCourt{{final: true, text: "Done " + native}})
	}
	// Distinct update times keep the catalog's updated-desc order stable.
	seed("onpage-live", recent)
	seed("failed-live", recent.Add(-time.Minute))
	seed("badged-live", recent.Add(-2*time.Minute))
	seed("outside-live", old)
	seed("detached-old", old.Add(-time.Minute))
	for i := 0; i < extra; i += 2 {
		seed(fmt.Sprintf("budget-indexed-%03d", i), old.Add(-time.Duration(2+i)*time.Minute))
	}
	if _, err := store.SyncThreadNames(context.Background(), []history.ThreadNameRecord{
		{Provider: history.ProviderCodex, NativeThreadID: "onpage-live", Name: "Onpage durable", Provenance: history.ThreadNameManual},
		{Provider: history.ProviderCodex, NativeThreadID: "outside-live", Name: "Outside creation", Provenance: history.ThreadNameCreation},
		{Provider: history.ProviderCodex, NativeThreadID: "badged-live", Name: "Badged import", Provenance: history.ThreadNameHistoricalImport},
		{Provider: history.ProviderClaude, NativeThreadID: "failed-live", Name: "Claude copy", Provenance: history.ThreadNameManual},
		{Provider: history.ProviderCodex, NativeThreadID: "detached-hosted", Name: "Detached durable", Provenance: history.ThreadNameManual},
	}); err != nil {
		t.Fatal(err)
	}

	stars, err := sidebar.OpenStars(filepath.Join(t.TempDir(), "stars.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stars.Close() })
	attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "detached-old", AttachedAt: now.Format(time.RFC3339)})
	metadata, _ := newTestHostedControl(t)
	f := &budgetFixture{now: now, hostedBy: map[string]string{}}
	// Instrument the adapter's supplied database from construction, so sessions
	// and the app read the same counted pools without mutating private handles.
	var controlPath string
	if err := metadata.CanonicalDatabase().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&controlPath); err != nil {
		t.Fatal(err)
	}
	controlURL := url.URL{Scheme: "file", Path: controlPath}
	query := controlURL.Query()
	query.Set("mode", "rw")
	query.Set("_pragma", "busy_timeout(5000)")
	controlURL.RawQuery = query.Encode()
	counted := sql.OpenDB(countingConnector{driver: metadata.ReadPool().Driver(), dsn: controlURL.String(), count: &f.controlStatements})
	counted.SetMaxOpenConns(1)
	metadata, err = hostedstore.NewMetadataStore(counted, counted)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	executions, err := newControlAgentExecutionStore(metadata, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	sessions := map[string]*session{}
	var nameOwner *server
	live := func(native, cwd, model string, hosted bool, created time.Time) *session {
		sessID := "sess-" + native
		hostedID := "nht_missing_record"
		if hosted {
			hostedID = "nht_" + native
			record, err := fixtureEnsureSessionWithIdentity(executions, hostedID, "codex", presentationShared, "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := executions.BindSession(record.ID, sessID, native, "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			for _, fact := range [][2]string{{"cwd", cwd}, {postureFieldModel, model}} {
				field, value := fact[0], fact[1]
				if _, err := metadata.AppendEvent(metadataEvent{HostedThreadID: record.ID, Field: field, Value: value, Disposition: metadataDispositionRequested, Source: postureSourceCreation, Freshness: metadataFreshnessLive, SourceEventKey: record.ID + "-" + field}); err != nil {
					t.Fatal(err)
				}
			}
			f.hostedBy[native] = record.ID
		}
		sess := memorySession(t, sessID, "codex", native, "ready", "/transport/"+native, "transport-model", created, func(cfg *sessionConfig) {
			cfg.HostedThreadID = hostedID
			cfg.Metadata = readFixtureMetadata{metadata}
			cfg.AgentExecutions = &quietRuntimeExecutions{executions}
			cfg.MetadataName = func(provider, native string) (hostedstore.MetadataName, bool) {
				if nameOwner == nil {
					return hostedstore.MetadataName{}, false
				}
				return nameOwner.durableThreadNameRef(provider, native)
			}
		})
		sessions[sessID] = sess
		return sess
	}
	created := now.Add(-time.Hour)
	live("onpage-live", "/canon/onpage", "canon-model-a", true, created).SetDisplayName("Onpage display")
	live("outside-live", "/canon/outside", "canon-model-b", true, created.Add(time.Second))
	live("unindexed-live", "/canon/unindexed", "canon-model-c", true, created.Add(2*time.Second))
	live("failed-live", "/canon/failed", "canon-model-d", true, created.Add(3*time.Second)).SetDisplayName("Failed display")
	live("badged-live", "/canon/badged", "canon-model-e", true, created.Add(4*time.Second))
	live("missing-record-live", "", "", false, created.Add(5*time.Second))
	for i := 0; i < extra; i++ {
		native := fmt.Sprintf("budget-indexed-%03d", i)
		if i%2 == 1 {
			native = fmt.Sprintf("budget-unindexed-%03d", i)
		}
		live(native, "/canon/"+native, "canon-model-x", true, created.Add(time.Duration(10+i)*time.Second))
	}
	// Hosted detached records: a live session was made for each, then dropped.
	for i, native := range []string{"detached-hosted", "detached-unnamed"} {
		sess := live(native, "/canon/"+native, "canon-model-z", true, created)
		delete(sessions, sess.ID())
		attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: native, HostedThreadID: sess.HostedThreadID(),
			AttachedAt: now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339)})
	}

	turn, _, err := fixtureBeginTurn(executions, f.hostedBy["failed-live"], "action-failed", "prompt-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.MarkTurnDelivery(turn.ID, "codex:failed", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := executions.MarkTurnTerminal("codex", "failed-live", "codex:failed", agentTurnFailed,
		&agentExecutionError{Source: "provider", Code: "error", Message: "failed"}, "", ""); err != nil {
		t.Fatal(err)
	}

	f.catalog = &countingCatalog{CatalogSource: newStoredCatalog(t, store)}
	s := testServerWithSessions(&server{
		catalog: f.catalog, Registry: newSessionRegistry(),
		attachments: attachments, threadStars: stars,
		controlMetadata: metadata, agentExecutions: executions,
		runtimeEvents: sidebar.NewRuntimeHub(), mailEvents: featuremail.NewHub(), mail: newTestAgentMessageStore(t),
	}, sessions)
	nameOwner = s
	s.sidebar = sidebar.New(sidebar.Sources{Catalog: f.catalog.Service, Registry: &s.Registry, Executions: s.agentExecutions, Metadata: s.controlMetadata, MetadataError: s.controlMetadataErr, Attachments: s.attachments, Boot: &s.bootAttachments, Stars: s.threadStars, Mail: s.mail, MailError: s.mailErr, RuntimeEvents: s.runtimeEvents})
	if followChanges {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if err := s.sidebarOwner().StartSidebarMaterializer(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	f.server = s
	return f
}

// countControlStatements counts all statements through the fixture's shared
// adapter. Its single physical connection rejects writes for the read boundary.
func (f *budgetFixture) countControlStatements(t *testing.T, fn func()) int64 {
	t.Helper()
	database := f.server.controlMetadata.CanonicalDatabase()
	if _, err := database.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := database.Exec(`PRAGMA query_only=OFF`); err != nil {
			t.Error(err)
		}
	}()
	f.controlStatements.Store(0)
	fn()
	return f.controlStatements.Load()
}

// countingConnector opens connections that count every prepared statement
// and, when rows is set, every row a query returns.
type countingConnector struct {
	driver      driver.Driver
	dsn         string
	count, rows *atomic.Int64
}

func (c countingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, count: c.count, rows: c.rows}, nil
}

func (c countingConnector) Driver() driver.Driver { return c.driver }

// countingConn exposes only Prepare, so database/sql prepares every query and
// exec through it and each statement is counted once.
type countingConn struct {
	driver.Conn
	count, rows *atomic.Int64
}

func (c countingConn) Prepare(query string) (driver.Stmt, error) {
	c.count.Add(1)
	stmt, err := c.Conn.Prepare(query)
	if err != nil || c.rows == nil {
		return stmt, err
	}
	return countingStmt{Stmt: stmt, rows: c.rows}, nil
}

type countingStmt struct {
	driver.Stmt
	rows *atomic.Int64
}

func (s countingStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.Stmt.Query(args) //nolint:staticcheck // the wrapper hides the context form
	if err != nil {
		return nil, err
	}
	return countingRows{Rows: rows, count: s.rows}, nil
}

type countingRows struct {
	driver.Rows
	count *atomic.Int64
}

func (r countingRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.count.Add(1)
	}
	return err
}

func (f *budgetFixture) get(t *testing.T, handler http.HandlerFunc, target string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func (f *budgetFixture) threadsURL() string {
	return "/api/v1/threads?limit=50&updatedFrom=" + url.QueryEscape(f.now.Add(-30*24*time.Hour).Format(time.RFC3339))
}

func (f *budgetFixture) sidebarURL() string {
	return "/api/v1/sidebar/threads?updatedFrom=" + url.QueryEscape(f.now.Add(-30*24*time.Hour).Format(time.RFC3339))
}

// TestRuntimeEventReadsNoControlStoreRows catches fleet-scale work per runtime
// event: rebuilding a full summary, four control-store statements, for every
// live session on every bind, unbind and turn flip. Boundary: the runtime delta
// that notifyRuntime and notifyRuntimeRefresh publish, with 40 extra live
// hosted sessions. The asynchronous sidebar consumer is outside this publisher
// measurement: notifyRuntimeRefresh deliberately asks that consumer to read.
func TestRuntimeEventReadsNoControlStoreRows(t *testing.T) {
	f := newBudgetFixture(t, 40, false)
	// Prime the initial snapshot before measuring the two publisher calls.
	f.server.notifyRuntime()
	statements := f.countControlStatements(t, func() {
		f.server.attachmentCoordinator().NotifyRuntime()
		f.server.attachmentCoordinator().NotifyRuntimeRefresh()
	})
	if statements != 0 {
		t.Fatalf("two runtime events read %d control-store statements for %d live sessions; want 0", statements, len(f.server.SnapshotSessions()))
	}
}

// TestThreadListStatementsDoNotGrowWithLiveSessions catches per-session work
// in GET /api/v1/threads: a full summary (four control-store statements) per
// session, an index probe and a rescue query per live or detached identity,
// and per-row lineage reads. Boundary: the same list call against 4 and
// against 40 extra live sessions; catalog calls and control-store statements
// must not change.
func TestThreadListStatementsDoNotGrowWithLiveSessions(t *testing.T) {
	measure := func(extra int) (catalogCalls, statements int64, rows int) {
		f := newBudgetFixture(t, extra, true)
		before := f.catalog.calls()
		var body map[string]any
		statements = f.countControlStatements(t, func() { body = f.get(t, f.server.catalogViews().HandleThreads, f.threadsURL()) })
		items, _ := body["items"].([]any)
		return f.catalog.calls() - before, statements, len(items)
	}
	smallCalls, smallStatements, smallRows := measure(4)
	largeCalls, largeStatements, largeRows := measure(40)
	if largeCalls != smallCalls {
		t.Errorf("catalog calls grew with live sessions: %d rows -> %d calls, %d rows -> %d calls", smallRows, smallCalls, largeRows, largeCalls)
	}
	if largeStatements != smallStatements {
		t.Errorf("control-store statements grew with live sessions: %d rows -> %d, %d rows -> %d", smallRows, smallStatements, largeRows, largeStatements)
	}
}

// TestSessionsListStatementsDoNotGrowWithSessions catches per-session reads
// in GET /api/sessions, which every open tab calls after runtime events: four
// metadata statements and one durable-name query per session. Boundary: the
// same call against 4 and against 40 extra live hosted sessions;
// control-store statements and catalog reads (names included) must not
// change.
func TestSessionsListStatementsDoNotGrowWithSessions(t *testing.T) {
	measure := func(extra int) (statements, catalogCalls int64) {
		f := newBudgetFixture(t, extra, true)
		before := f.catalog.calls()
		statements = f.countControlStatements(t, func() { f.get(t, f.server.handleSessionsList, "/api/sessions") })
		return statements, f.catalog.calls() - before
	}
	smallStatements, smallCalls := measure(4)
	largeStatements, largeCalls := measure(40)
	if largeStatements != smallStatements {
		t.Errorf("/api/sessions control-store statements grew with sessions: %d (10 sessions) -> %d (46 sessions)", smallStatements, largeStatements)
	}
	if largeCalls != smallCalls {
		t.Errorf("/api/sessions catalog reads grew with sessions: %d (10 sessions) -> %d (46 sessions)", smallCalls, largeCalls)
	}
}

// TestListResponsesShowEachRowsFacts catches a list call that drops a row or
// shows a wrong fact for one: the canonical cwd and model, the durable name
// and its provenance (or the display-name fallback), attention, badges, a
// missing hosted record, memory and rescued rows, detached rows and sidebar
// placement. Boundary: GET /api/sessions, /api/v1/threads and
// /api/v1/sidebar/threads on the budget fixture, compared row by row with
// testdata/list_responses.golden (NANO_UPDATE_FIXTURES=1 rewrites it).
func TestListResponsesShowEachRowsFacts(t *testing.T) {
	f := newBudgetFixture(t, 4, true)
	var lines []string
	add := func(kind string, rows any, paths ...string) {
		list, _ := rows.([]any)
		for _, row := range list {
			lines = append(lines, kind+" "+rowFacts(row.(map[string]any), paths...))
		}
	}
	sessions := f.get(t, f.server.handleSessionsList, "/api/sessions")
	add("session", sessions["sessions"], "id", "threadId", "status", "cwd", "model", "needsAttention",
		"metadataUnavailable", "metadata.revision")
	add("detached", sessions["detached"], "provider", "nativeThreadId", "hostedThreadId", "metadataUnavailable",
		"metadata.revision")
	threads := f.get(t, f.server.catalogViews().HandleThreads, f.threadsURL())
	add("thread", threads["items"], "provider", "nativeId", "idClass", "kind", "name.value", "name.provenance",
		"runtime.status", "runtime.detached", "metadata.cwd.value", "metadata.model.value", "fleet.label")
	lines = append(lines, "thread nextCursor="+fmt.Sprint(threads["nextCursor"] != nil))
	sidebar := f.get(t, f.server.sidebarOwner().HandleSidebarThreads, f.sidebarURL())
	add("sidebar", sidebar["items"], "provider", "nativeId", "idClass", "name.value", "name.provenance", "runtime.status",
		"sidebar.active", "sidebar.depth", "fleet.label")
	roots, _ := sidebar["roots"].([]any)
	lines = append(lines, fmt.Sprintf("sidebar historicalCount=%v roots=%d", sidebar["historicalCount"], len(roots)))

	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "list_responses.golden")
	if os.Getenv("NANO_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("list responses changed (NANO_UPDATE_FIXTURES=1 rewrites the golden only for an intended change):\n%s", got)
	}
}

// rowFacts renders the named fields of one response row as path=value pairs;
// absent fields are left out. idClass is the id's prefix (nth, mem), since the
// rest of a catalog id changes with every store.
func rowFacts(row map[string]any, paths ...string) string {
	var parts []string
	for _, path := range paths {
		var value any = row
		if path == "idClass" {
			id, _ := row["id"].(string)
			value, _, _ = strings.Cut(id, "_")
		} else {
			for _, key := range strings.Split(path, ".") {
				fields, _ := value.(map[string]any)
				value = fields[key]
			}
		}
		if value == nil {
			continue
		}
		encoded, _ := json.Marshal(value)
		parts = append(parts, path+"="+string(encoded))
	}
	return strings.Join(parts, " ")
}

// TestAgentSessionsListSkipsExitedRecords catches GET /api/v1/hosted-threads
// building a full view (metadata, assignment, badges, availability) for every
// exited record only to drop it; the store keeps every record a thread ever
// had. Boundary: the handler over the budget fixture with 5 and with 50 extra
// exited records, one of them recorded exited while its live session is still
// attached and one recorded exited while boot is still reattaching it.
// Control-store statements must not change, and the response must hold every
// live and detached thread, the attached one included, the reattaching one as
// starting, and no other exited record.
func TestAgentSessionsListSkipsExitedRecords(t *testing.T) {
	measure := func(exited int) (int64, map[string]string) {
		f := newBudgetFixture(t, 4, true)
		executions := f.server.agentExecutions
		for i := 0; i < exited; i++ {
			id := fmt.Sprintf("nht_exited_%03d", i)
			if _, err := fixtureEnsureSessionWithIdentity(executions, id, "codex", presentationShared, "", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := executions.UpdateSession(id, "", fmt.Sprintf("exited-%03d", i), sessionStatusExited, ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := executions.UpdateSession(f.hostedBy["outside-live"], "", "outside-live", sessionStatusExited, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := fixtureEnsureSessionWithIdentity(executions, "nht_rebooting", "codex", presentationShared, "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := executions.UpdateSession("nht_rebooting", "", "rebooting", sessionStatusExited, ""); err != nil {
			t.Fatal(err)
		}
		rebooting := hostedstore.Attachment{Provider: "codex", NativeThreadID: "rebooting", HostedThreadID: "nht_rebooting"}
		f.server.bootAttachments = sessions.NewBootAttachments([]hostedstore.Attachment{rebooting})
		var body map[string]any
		statements := f.countControlStatements(t, func() { body = f.get(t, f.server.hostedViews().HandleSessionsList, "/api/v1/hosted-threads") })
		listed := map[string]string{}
		items, _ := body["items"].([]any)
		for _, item := range items {
			row := item.(map[string]any)
			listed[row["id"].(string)], _ = row["status"].(string)
		}
		return statements, listed
	}
	small, smallListed := measure(5)
	large, largeListed := measure(50)
	if large != small {
		t.Errorf("control-store statements grew with exited records: 5 -> %d, 50 -> %d", small, large)
	}
	want := map[string]string{}
	for _, native := range []string{"onpage-live", "outside-live", "unindexed-live", "failed-live", "badged-live",
		"detached-hosted", "detached-unnamed", "budget-indexed-000", "budget-unindexed-001", "budget-indexed-002", "budget-unindexed-003"} {
		want["nht_"+native] = "ready"
	}
	want["nht_rebooting"] = "starting"
	for name, listed := range map[string]map[string]string{"5 exited": smallListed, "50 exited": largeListed} {
		if !reflect.DeepEqual(listed, want) {
			t.Errorf("%s: listed %v, want %v", name, listed, want)
		}
	}
}

// TestAvailabilityReadsOnlyItsThreadsPrompts catches availability reading
// every queued prompt, text included, to answer for one thread: about 4,000
// rows for each thread the agent thread list shows. Boundary: availabilityFor
// on a live hosted thread with two queued prompts of its own and 5 or 50 for
// other threads; rows read must not grow, and the backlog stays 2.
func TestAvailabilityReadsOnlyItsThreadsPrompts(t *testing.T) {
	measure := func(others int) (rowsRead int64, queued int) {
		s, _, _, _, worker, _, _ := stage0Server(t)
		ctx := context.Background()
		var statements, rows atomic.Int64
		db := sql.OpenDB(countingConnector{driver: s.queuedTurns.Database().Driver(), dsn: s.queuedTurns.Path(), count: &statements, rows: &rows})
		t.Cleanup(func() { db.Close() })
		store, err := prompts.OpenStore(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		s.queuedTurns.BindTransactionStore(store)
		submit := func(hosted, native, action string) {
			if _, _, err := store.Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: "codex", NativeThreadID: native,
				ActionID: action, ProviderText: "prompt " + action, TextHash: action, RequestHash: action}); err != nil {
				t.Fatal(err)
			}
		}
		submit(worker.ID, "native-worker", "mine-1")
		submit(worker.ID, "native-worker", "mine-2")
		for i := 0; i < others; i++ {
			submit(fmt.Sprintf("nht_other_%03d", i), fmt.Sprintf("native-other-%03d", i), fmt.Sprintf("other-%03d", i))
		}
		rows.Store(0)
		queued = s.hostedViews().AvailabilityFor(worker.ID, time.Now().UTC()).Facts.QueuedPrompts
		return rows.Load(), queued
	}
	smallRows, smallQueued := measure(5)
	largeRows, largeQueued := measure(50)
	if smallQueued != 2 || largeQueued != 2 {
		t.Fatalf("queued prompts = %d and %d, want 2", smallQueued, largeQueued)
	}
	if largeRows != smallRows {
		t.Errorf("rows read grew with other threads' prompts: 5 others -> %d, 50 others -> %d", smallRows, largeRows)
	}
}

// TestThreadListPagesKeepLimitAndLoseNoThread catches a limit that does not
// bound the response (live and supervisor rows were added on top of it, so
// limit=50 returned 331 rows) and a limit that hides threads: paging the window
// must still show every live thread, and a worker's supervisors must share its
// page. The one exception to the limit: a page's first row whose complete
// supervisor chain does not fit comes with that whole chain and nothing else.
// Boundary: GET /api/v1/threads paged at limits 1 to 3 over the budget
// fixture (live rows inside and outside the window, memory rows, a detached
// row) and the lineage fixture (supervisors outside the window, two levels
// deep), against one limit-250 call; and an exact threadId search at limit 1.
func TestThreadListPagesKeepLimitAndLoseNoThread(t *testing.T) {
	supervisors := map[string][]string{
		"codex/w-related": {"codex/p-related"}, "codex/w-invoker": {"codex/p-invoker"},
		"codex/w-native": {"codex/p-native", "codex/g-native"}, "codex/w-dup": {"codex/p-native", "codex/g-native"},
		"codex/w-covered": {"codex/onpage-parent"},
	}
	identities := func(items []any) []string {
		var out []string
		for _, item := range items {
			row := item.(map[string]any)
			out = append(out, row["provider"].(string)+"/"+row["nativeId"].(string))
		}
		return out
	}
	pageAll := func(f *budgetFixture, base string, limit int) map[string]bool {
		union, cursor := map[string]bool{}, ""
		for pages := 0; pages < 100; pages++ {
			target := base + "&limit=" + strconv.Itoa(limit)
			if cursor != "" {
				target += "&cursor=" + url.QueryEscape(cursor)
			}
			body := f.get(t, f.server.catalogViews().HandleThreads, target)
			items, _ := body["items"].([]any)
			page := identities(items)
			onPage := map[string]bool{}
			for _, key := range page {
				onPage[key], union[key] = true, true
			}
			if len(page) > limit && !reflect.DeepEqual(page, append([]string{page[0]}, supervisors[page[0]]...)) {
				t.Fatalf("%s returned %d rows %v; only a first row with its whole supervisor chain may exceed the limit", target, len(page), page)
			}
			for worker, parents := range supervisors {
				for _, parent := range parents {
					if onPage[worker] && !onPage[parent] {
						t.Errorf("%s holds %s without its supervisor %s: %v", target, worker, parent, page)
					}
				}
			}
			next, _ := body["nextCursor"].(string)
			if next == "" {
				return union
			}
			if next == cursor {
				t.Fatalf("%s did not advance its cursor", target)
			}
			cursor = next
		}
		t.Fatalf("paging %s did not end", base)
		return nil
	}
	whole := func(f *budgetFixture, base string) map[string]bool {
		items, _ := f.get(t, f.server.catalogViews().HandleThreads, base+"&limit=250")["items"].([]any)
		union := map[string]bool{}
		for _, key := range identities(items) {
			union[key] = true
		}
		return union
	}

	budget := newBudgetFixture(t, 4, true)
	window := "/api/v1/threads?updatedFrom=" + url.QueryEscape(budget.now.Add(-30*24*time.Hour).Format(time.RFC3339))
	for _, limit := range []int{1, 3} {
		if paged := pageAll(budget, window, limit); !reflect.DeepEqual(paged, whole(budget, window)) {
			t.Errorf("paging at limit %d showed %v, one call shows %v", limit, paged, whole(budget, window))
		}
	}
	if paged := pageAll(budget, "/api/v1/threads?threadId=outside-live", 1); !paged["codex/outside-live"] {
		t.Errorf("threadId search at limit 1 showed %v", paged)
	}

	lineage := newLineageFixture(t, 0).budget
	tree := "/api/v1/threads?includeSubagents=true&updatedFrom=" + url.QueryEscape(lineage.now.Add(-2*24*time.Hour).Format(time.RFC3339))
	for _, limit := range []int{1, 2, 3} {
		if paged := pageAll(lineage, tree, limit); !reflect.DeepEqual(paged, whole(lineage, tree)) {
			t.Errorf("paging the lineage window at limit %d showed %v, one call shows %v", limit, paged, whole(lineage, tree))
		}
	}
}

// TestAvailabilityQueuedBacklogCountsOnlyTheThreadsQueuedPrompts catches a
// change to the busy/queued_backlog state that heartbeat wake gates and idle
// checks read, when availability counts a thread's queued prompts from its
// own rows. Boundary: availabilityFor on a live hosted thread: idle with none
// queued, busy with its own two queued prompts (another thread's queued
// prompt not counted), and turn_active still ahead of queued_backlog.
func TestAvailabilityQueuedBacklogCountsOnlyTheThreadsQueuedPrompts(t *testing.T) {
	s, _, sess, sender, worker, _, provider := stage0Server(t)
	ctx, now := context.Background(), time.Now().UTC()
	if v := s.hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityAvailable || v.Facts.QueuedPrompts != 0 {
		t.Fatalf("nothing queued = %+v", v)
	}
	submit := func(hosted, native, action string) {
		if _, _, err := sess.PromptStore().Submit(ctx, prompts.Submission{HostedThreadID: hosted, Provider: "codex", NativeThreadID: native,
			ActionID: action, ProviderText: "prompt " + action, TextHash: action, RequestHash: action}); err != nil {
			t.Fatal(err)
		}
	}
	if reason, err := sess.WithIdleDelivery(false, func() error {
		submit(worker.ID, "native-worker", "mine-1")
		submit(worker.ID, "native-worker", "mine-2")
		submit(sender.ID, "native-sender", "theirs-1")
		if v := s.hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityBusy || v.Facts.QueuedPrompts != 2 ||
			len(v.Reasons) != 1 || v.Reasons[0] != "queued_backlog" {
			t.Fatalf("own backlog = %+v", v)
		}
		provider.EmitThreadNotification(sess.ThreadID(), map[string]any{"method": "turn/started", "params": map[string]any{"threadId": sess.ThreadID(), "turn": map[string]any{"id": "t1"}}})
		waitForClaude(t, "availability working turn", func() bool { return sess.Summary().TurnActive })
		return nil
	}); err != nil || reason != "" {
		t.Fatalf("fixture delivery lock: %s %v", reason, err)
	}
	if v := s.hostedViews().AvailabilityFor(worker.ID, now); v.State != availabilityBusy || v.Reasons[0] != "turn_active" || v.Facts.QueuedPrompts != 2 {
		t.Fatalf("active with backlog = %+v", v)
	}
}

// TestQueuedPromptsAlwaysCarryAHostedThread catches a queued prompt stored
// without a hosted id, which a count of one thread's rows would miss.
// Boundary: prompts.Store.Submit, the one writer of new queued prompts (core
// session admission calls it); the legacy writer only imports old queues.
func TestQueuedPromptsAlwaysCarryAHostedThread(t *testing.T) {
	ctx := context.Background()
	db, _, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "queued-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := prompts.OpenStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Submit(ctx, prompts.Submission{Provider: "codex", NativeThreadID: "native-only",
		ActionID: "no-owner", ProviderText: "prompt", TextHash: "h", RequestHash: "h"}); err == nil {
		t.Fatal("a prompt without a hosted id was accepted")
	}
	if records, err := store.List(ctx, ""); err != nil || len(records) != 0 {
		t.Fatalf("stored records = %d, %v; want none", len(records), err)
	}
}

// Read-budget fixtures seed their canonical facts before attachment. Provider
// setup must not overwrite those deliberately contrasting transport values.
type readFixtureMetadata struct{ *hostedstore.MetadataStore }

func (readFixtureMetadata) AppendEvent(event metadataEvent) (metadataEvent, error) { return event, nil }
