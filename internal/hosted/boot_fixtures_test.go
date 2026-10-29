package hosted

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/testsupport"
	shimfixture "github.com/dkroshow/nano/internal/testsupport/claudeshim"
	codexfixture "github.com/dkroshow/nano/internal/testsupport/codexserver"
	"github.com/dkroshow/nano/internal/threadindex"
	shim "github.com/dkroshow/nano/internal/wire/claude"
)

type bootTestCoordinator struct {
	*AttachmentCoordinator
	events *sidebar.RuntimeHub
}

const claudeResumeThreadID = "native-1"

func newBootCoordinator(t *testing.T, shimURL, codexURL string) (*bootTestCoordinator, *hostedstore.AttachmentStore) {
	t.Helper()
	store, err := hostedstore.NewAttachmentStore(filepath.Join(t.TempDir(), "attachments.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := newTestControlMetadata(t)
	executions, err := newControlAgentExecutionStore(metadata, "test-run")
	if err != nil {
		t.Fatal(err)
	}
	queue := testsupport.MustQueue(t)
	turns := testsupport.PromptStore(t, queue.Database())
	attempts, err := prompts.NewLegacyJournal(queue.Database(), queue.Path(), "test-run", turns)
	if err != nil {
		t.Fatal(err)
	}
	registry := sessions.NewRegistry()
	boot := sessions.BootAttachments{}
	sources := AttachmentSources{
		Defaults: config.SessionDefaults{CWD: t.TempDir(), ClaudeBin: "/default/claude", ClaudeShimWS: shimURL, AllowedTools: "Read,Grep", ClaudeShimSpools: t.TempDir()},
		SpoolDir: t.TempDir(), Registry: &registry, Boot: &boot, Attachments: store, Metadata: metadata, Executions: executions, Queue: queue, Turns: turns, SendAttempts: attempts,
		Notices: interact.NewNotices(), Stopping: &atomic.Bool{}, ProcessStarted: time.Now(), SweepSubscriptions: func(string) {},
	}
	if codexURL != "" {
		sources.Codex = sessions.NewCodexDaemon(codexURL, config.DefaultCodexSupervisorLabel, func() bool { return true })
	}
	events := sidebar.NewRuntimeHub()
	sources.Runtime = sidebar.New(sidebar.Sources{Registry: &registry, Executions: executions, Metadata: metadata, Attachments: store, Boot: &boot, RuntimeEvents: events})
	sources.CreationNames = &CreationNames{Metadata: metadata, Executions: executions, Queue: queue}
	coordinator := &bootTestCoordinator{AttachmentCoordinator: NewAttachmentCoordinator(sources), events: events}
	t.Cleanup(func() {
		for _, sess := range registry.SnapshotSessions() {
			sess.Teardown().Close()
		}
	})
	return coordinator, store
}

func bootCodexRecord(t *testing.T) hostedstore.Attachment {
	t.Helper()
	return hostedstore.Attachment{Provider: "codex", NativeThreadID: "codex-thread-1", Cwd: t.TempDir(), Model: "gpt-test", AttachedAt: "2026-07-28T10:00:00Z"}
}
func rememberedClaudeSession(t *testing.T, bin string) hostedstore.Attachment {
	t.Helper()
	return hostedstore.Attachment{Provider: "claude", NativeThreadID: claudeResumeThreadID, Cwd: t.TempDir(), Model: "sonnet", Bin: bin, Args: []string{"--permission-mode", "bypassPermissions"}, AttachedAt: "2026-07-27T10:00:00Z"}
}
func seedBootAttachments(t *testing.T, s *bootTestCoordinator) {
	t.Helper()
	for _, rec := range s.Attachments.DetachedExcept(nil) {
		if rec.HostedThreadID == "" {
			rec.HostedThreadID = "nht_boot_" + hostedstore.AgentHash([]byte(rec.Provider + "\x00" + rec.NativeThreadID))[:16]
			s.Attachments.Remember(rec)
		}
		if rec.Provider == "claude" {
			if err := os.WriteFile(filepath.Join(s.Defaults.ClaudeShimSpools, rec.HostedThreadID+".jsonl"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := fixtureEnsureSessionWithIdentity(s.Executions, rec.HostedThreadID, rec.Provider, "shared", rec.InvokerHostedThreadID, rec.InvokerNativeThreadID, rec.InvokerProvider); err != nil {
			t.Fatal(err)
		}
		if err := s.Metadata.ClaimNativeID(rec.HostedThreadID, rec.Provider, rec.NativeThreadID, rec.Provisional()); err != nil {
			t.Fatal(err)
		}
		gen, err := s.Metadata.BeginTransportBinding(rec.HostedThreadID)
		if err != nil {
			t.Fatal(err)
		}
		cwd := rec.Cwd
		if cwd == "" {
			cwd = s.Defaults.CWD
		}
		model := rec.Model
		if model == "" {
			model = "gpt-test"
			if rec.Provider == "claude" {
				model = "sonnet"
			}
		}
		for _, fact := range []hostedstore.FieldValue{{Field: "cwd", Value: cwd}, {Field: hostedstore.PostureFieldModel, Value: model}, {Field: hostedstore.PostureFieldPermissionMode, Value: testsupport.ArgumentValue(rec.Args, "--permission-mode")}} {
			if fact.Value == "" {
				continue
			}
			if _, err := s.Metadata.AppendEvent(hostedstore.MetadataEvent{HostedThreadID: rec.HostedThreadID, Field: fact.Field, Value: fact.Value, Disposition: hostedstore.DispositionRequested, Source: hostedstore.PostureSourceCreation, Freshness: hostedstore.FreshnessLive, AttachmentGeneration: gen, SourceEventKey: rec.HostedThreadID + fact.Field}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func openTurnsForBoot(t *testing.T, s *bootTestCoordinator) {
	t.Helper()
	for _, r := range s.Attachments.DetachedExcept(nil) {
		if _, _, err := fixtureBeginTurn(s.Executions, r.HostedThreadID, "nano:test:open-turn", "hash"); err != nil {
			t.Fatal(err)
		}
	}
}
func startingCodexCoordinator(t *testing.T) (*bootTestCoordinator, *codexfixture.Server, hostedstore.Attachment) {
	t.Helper()
	daemon := codexfixture.NewServer(t)
	s, store := newBootCoordinator(t, "", daemon.WSURL())
	rec := bootCodexRecord(t)
	store.Remember(rec)
	seedBootAttachments(t, s)
	rec = s.Attachments.DetachedExcept(nil)[0]
	s.PrepareBoot()
	return s, daemon, rec
}

func bootResumeScript(attach func(*shimfixture.Conn, shimfixture.Call)) func(*shimfixture.Conn) {
	return func(c *shimfixture.Conn) {
		for {
			call, ok := c.Next()
			if !ok {
				return
			}
			switch call.Method {
			case shim.MethodAttach:
				attach(c, call)
			default:
				c.Reply(call, nil)
			}
		}
	}
}

// The rank fixtures write the same completed source evidence that the sidebar
// consumes in production. Boot still reads the materialized sidebar controller.
func materializeBootSidebar(t *testing.T, s *bootTestCoordinator, rows []sidebar.Thread, now time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, row := range rows {
		root := history.SourceRoot{Provider: history.Provider(row.Provider), Kind: history.RootCodexSessions, Path: t.TempDir()}
		if row.Provider == "claude" {
			root.Kind = history.RootClaudeProjects
		}
		if _, err := store.EnsureRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		title := row.NativeID
		result := history.ProjectionResult{Projection: history.ThreadProjection{Provider: root.Provider, NativeThreadID: row.NativeID, IdentityEvidence: history.EvidenceNativeDurable, Kind: history.ThreadMain, CreatedAt: &now, UpdatedAt: &now, Title: &title}, CompleteOffset: 123, CompleteLines: 3}
		src := history.SourceCandidate{Root: root, Path: filepath.Join(root.Path, row.NativeID+".jsonl"), RelativePath: row.NativeID + ".jsonl", Kind: history.ThreadMain, Size: 123, ModTime: now, Device: 1, Inode: 1}
		applied, err := store.ApplySource(ctx, history.SourceProjection{Source: src, Result: &result, ParserVersion: "test-v1", Status: history.SourcePresent, IndexedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		roots, err := store.ListRoots(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var rootID int64
		for _, r := range roots {
			if r.Root.Path == root.Path {
				rootID = r.ID
			}
		}
		stored, ok, err := store.LoadSource(ctx, rootID, src.Path)
		if err != nil || !ok {
			t.Fatalf("source: %v %v", ok, err)
		}
		turn := history.TranscriptTurn{Key: "ntr_" + row.NativeID, Ordinal: 1, StartSeq: 1, EndSeq: 1, Status: history.TurnCompleted, CompletionEvidence: history.CompletionTaskComplete, FinalItemID: "nit_" + row.NativeID}
		item := history.TranscriptItem{ID: turn.FinalItemID, ProjectedSeq: 1, SourceKey: row.NativeID, TurnKey: turn.Key, TurnOrdinal: 1, Type: history.ItemAgentMessage, Status: history.ItemStatusCompleted, Text: "Finished", IsFinalAnswer: true, SourceLine: 1, SourceRowType: "test", SourceLineSHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
		_, err = store.ApplyTranscript(ctx, history.TranscriptApplyRequest{Mode: history.TranscriptRebuild, ThreadID: applied.ThreadID, SourceID: stored.ID, SourceGeneration: stored.Generation, Provider: root.Provider, ParserVersion: "test-v1", CompletionRuleVersion: "test-rules-v1", Result: history.TranscriptProjectionResult{ProjectionKey: "ntp_" + row.NativeID, CompleteOffset: 1, CompleteLines: 1, ParserState: json.RawMessage(`{}`), Turns: []history.TranscriptTurn{turn}, Items: []history.TranscriptItem{item}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := threadindex.New(ctx, store, threadindex.Options{Roots: []history.SourceRoot{}, DisableWatcher: true, NativeIDProvisional: sessions.NativeThreadIDProvisional, ClaudeProjectSlug: setup.ClaudeProjectSlug})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(catalog.Close)
	s.Catalog = catalog
	s.Runtime = sidebar.New(sidebar.Sources{Catalog: catalog, Registry: s.Registry, Executions: s.Executions, Metadata: s.Metadata, Attachments: s.Attachments, Boot: s.Boot, RuntimeEvents: s.events})
	if err := s.Runtime.StartSidebarMaterializer(ctx, now); err != nil {
		t.Fatal(err)
	}
}
