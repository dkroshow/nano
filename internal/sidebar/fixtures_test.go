package sidebar

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	"github.com/dkroshow/nano/internal/features/subscriptions"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/testsupport"
	"github.com/dkroshow/nano/internal/testsupport/codexserver"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func sidebarActiveFixture(t *testing.T, now time.Time) (*Controller, map[string]string) {
	t.Helper()
	s, ids, _ := sidebarActiveFixtureWithStore(t, now)
	return s, ids
}

func sidebarActiveFixtureWithStore(t *testing.T, now time.Time) (*Controller, map[string]string, *history.Store) {
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

	stars, err := OpenStars(filepath.Join(t.TempDir(), "stars.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stars.Close() })
	if err := stars.SetStarred("codex", "starred-old", true); err != nil {
		t.Fatal(err)
	}
	attachments, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	attachments.Remember(hostedstore.Attachment{Provider: "codex", NativeThreadID: "detached-old", AttachedAt: now.Format(time.RFC3339)})
	metadata, executions := newTestHostedControl(t)
	parentHosted, err := fixtureEnsureSessionWithIdentity(executions, "nht_related_parent", "codex", ownedthreads.PresentationShared, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	childHosted, err := fixtureEnsureSessionWithIdentity(executions, "nht_related_child", "codex", ownedthreads.PresentationShared, parentHosted.ID, ids["related-parent"], "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(parentHosted.ID, "sess-parent", "related-parent", "ready", ownedthreads.PresentationShared); err != nil {
		t.Fatal(err)
	}
	if err := executions.BindSession(childHosted.ID, "sess-child", "related-child", "ready", ownedthreads.PresentationShared); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureAppendOrchestrationAssignment(executions, childHosted.ID, "rel-1", &hostedstore.Assignment{
		AssignmentRole: "auditor", Focus: "Audit parent", RelatedHostedThreadID: parentHosted.ID,
		AuditTargets: []hostedstore.AuditTarget{
			{HostedThreadID: parentHosted.ID},
			{HostedThreadID: "nht_missing"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	svc := newStoredCatalog(t, store)
	attached := memorySession(t, "sess-attached", "codex", "attached-recent", "ready", "/w/att", "gpt-5.6-sol", now)
	parentLive := memorySession(t, "sess-parent", "codex", "related-parent", "ready", "/w/related-parent", "gpt-5.6-sol", now, func(cfg *sessions.Config) { cfg.HostedThreadID = parentHosted.ID })
	if response := testsupport.PostTurn(t, parentLive, context.Background(), "parent-work", "working"); response.Code != http.StatusOK {
		t.Fatalf("parent working setup: %d %s", response.Code, response.Body)
	}
	childLive := memorySession(t, "sess-child", "codex", "related-child", "ready", "/w/related-child", "gpt-5.6-sol", now, func(cfg *sessions.Config) {
		cfg.HostedThreadID = childHosted.ID
		cfg.InvokerHostedThreadID = parentHosted.ID
	})
	if response := testsupport.PostTurn(t, childLive, context.Background(), "child-work", "working"); response.Code != http.StatusOK {
		t.Fatalf("child working setup: %d %s", response.Code, response.Body)
	}
	registry := sessions.NewRegistry()
	for _, sess := range []*sessions.Session{attached, parentLive, childLive} {
		registry.RegisterSession(sess)
	}
	s := New(Sources{Catalog: svc, Registry: &registry, Attachments: attachments, Stars: stars, Metadata: metadata, Executions: executions, RuntimeEvents: NewRuntimeHub(), Mail: newTestAgentMessageStore(t)})

	if err := s.materializeSidebar(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	return s, ids, store
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

func getSidebarActive(t *testing.T, s *Controller, now time.Time) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sidebar/threads?updatedFrom="+now.Add(-30*24*time.Hour).Format(time.RFC3339), nil)
	s.HandleSidebarThreads(rec, req)
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

func strPtr(s string) *string { return &s }
func newStoredCatalog(t *testing.T, store *history.Store) *threadindex.Service {
	t.Helper()
	s, err := threadindex.New(context.Background(), store, threadindex.Options{Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: t.TempDir()}}, NamesPath: filepath.Join(t.TempDir(), "names.json"), DisableWatcher: true, NativeIDProvisional: sessions.NativeThreadIDProvisional, ClaudeProjectSlug: setup.ClaudeProjectSlug})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func newTestHostedControl(t *testing.T) (*hostedstore.MetadataStore, *hostedstore.ExecutionStore) {
	t.Helper()
	db, _, err := testsupport.TurnDatabase(t, filepath.Join(t.TempDir(), "queued.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := hostedstore.NewMetadataStore(db, db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := hostedstore.NewExecutionStore(meta, "sidebar-test", subscriptions.IsFeatureAction)
	if err != nil {
		t.Fatal(err)
	}
	return meta, store
}
func newTestAgentMessageStore(t *testing.T) *featuremail.Store {
	t.Helper()
	s, _, err := featuremail.OpenStore(filepath.Join(t.TempDir(), "mail.sqlite"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func fixtureEnsureSessionWithIdentity(s *hostedstore.ExecutionStore, id, provider, mode, invoker, native, invokerProvider string) (ownedthreads.SessionRecord, error) {
	return s.EnsureNamedSessionWithIdentity(id, provider, mode, invoker, native, invokerProvider, "")
}
func fixtureAppendOrchestrationAssignment(s *hostedstore.ExecutionStore, id, action string, assignment *hostedstore.Assignment) (int64, error) {
	disposition := hostedstore.DispositionConfirmed
	var value any
	if assignment == nil {
		disposition = hostedstore.DispositionCleared
	} else {
		value = *assignment
	}
	event, err := s.Control().AppendEvent(hostedstore.MetadataEvent{HostedThreadID: id, Field: "orchestrationAssignment", Value: value, Disposition: disposition, Source: hostedstore.PostureSourceOperator, Freshness: hostedstore.FreshnessLive, SourceEventKey: "assignment:" + id + ":" + action, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return event.ID, err
}
func memorySession(t *testing.T, id, provider, native, status, cwd, model string, created time.Time, configure ...func(*sessions.Config)) *sessions.Session {
	t.Helper()
	cfg := sessions.Config{ID: id, Provider: provider, CWD: cwd, CreatedAt: created, HostedThreadID: "fixture-" + id}
	for _, fn := range configure {
		fn(&cfg)
	}
	queue := testsupport.MustQueue(t)
	cfg.QueuedTurns = queue
	cfg.Turns = testsupport.PromptStore(t, queue.Database())
	journal, err := prompts.NewLegacyJournal(queue.Database(), queue.Path(), "sidebar-test", cfg.Turns)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SendAttempts = journal
	cfg.Projection = sessions.NewLiveProjection(provider, cfg.HostedThreadID, false)
	sess := sessions.New(cfg)
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(native)
	t.Cleanup(sess.Close)
	if status == "exited" {
		sess.Close()
		return sess
	}
	if status != "ready" {
		return sess
	}
	if provider != "codex" {
		t.Fatalf("unsupported sidebar runtime fixture %s", provider)
	}
	fake := codexserver.NewServer(t)
	fake.SetResumeResult(map[string]any{"thread": map[string]any{"id": native}, "model": model})
	fake.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "sidebar-turn"}})
	daemon := sessions.NewCodexDaemon(fake.WSURL(), "", func() bool { return true })
	if err := sess.BeginProviderSpawn(sessions.ProviderRequest{Provider: provider, Mode: "resume", ThreadID: native}, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for sess.Summary().Status != "ready" {
		if time.Now().After(deadline) {
			t.Fatal("sidebar runtime did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	return sess
}
func (s *Controller) LookupSession(id string) *sessions.Session { return s.registry.LookupSession(id) }
func (s *Controller) RegisterSession(sess *sessions.Session)    { s.registry.RegisterSession(sess) }

func newRegistry() *sessions.Registry { r := sessions.NewRegistry(); return &r }
