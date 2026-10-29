package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

// Failure caught: an attachment whose file cannot be resolved vanished from
// monitoring, so it looked unwatched rather than unresolved. Boundary: a
// session's bind through the real catalog to the monitor's ingest view.
func TestUnresolvedAttachmentStaysVisibleUntilBoundOrReleased(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := newThreadCatalogService(context.Background(), store, threadindex.Options{
		Roots: []history.SourceRoot{{Provider: history.ProviderCodex, Kind: history.RootCodexSessions, Path: root}}, DisableWatcher: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	view := func() (int, int, map[string]int) {
		status := service.IngestStatus()
		return status.Attachments, status.HeldSources, status.UnresolvedAttachments
	}
	sess := newSession(runtimeFixtureConfig(t, sessions.Config{ID: "s1", Provider: "codex", HostedThreadID: "catalog-owner", CWD: "/work"}))
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ConfigureRegistration(nil, nil, false, service)
	sess.ConfigureSavedReaders(nil, nil, nil, nil, func(string) {})
	sess.DidRegister()
	if attachments, held, unresolved := view(); attachments != 1 || held != 0 || unresolved["awaiting native identity"] != 1 {
		t.Fatalf("before identity: attachments %d, held %d, unresolved %v", attachments, held, unresolved)
	}
	sess.ReserveNativeID("01a0aaaa-0000-7000-8000-000000000001")
	if attachments, held, unresolved := view(); attachments != 1 || held != 0 || unresolved["no single saved source for this identity"] != 1 {
		t.Fatalf("identity with no saved source: attachments %d, held %d, unresolved %v", attachments, held, unresolved)
	}
	fake := newFakeAppServer(t)
	fake.SetResumeResult(map[string]any{"thread": map[string]any{"id": sess.State().ThreadID(), "path": filepath.Join(root, "2026", "10", "03", "rollout-01a0aaaa-0000-7000-8000-000000000001.jsonl")}})
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.State().ThreadID()}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	waitForClaude(t, "catalog path held", func() bool { _, held, _ := view(); return held == 1 })
	if attachments, held, unresolved := view(); attachments != 1 || held != 1 || len(unresolved) != 0 {
		t.Fatalf("resolved: attachments %d, held %d, unresolved %v", attachments, held, unresolved)
	}
	sess.Close()
	if attachments, held, _ := view(); attachments != 0 || held != 0 {
		t.Fatalf("released: attachments %d, held %d", attachments, held)
	}
	claude := newSession(sessions.Config{ID: "s2", Provider: "claude", CWD: "/work"})
	if err := claude.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	claude.ConfigureRegistration(nil, nil, false, service)
	claude.ReserveNativeID("cs_0123456789abcdef")
	if _, held, unresolved := view(); held != 0 || unresolved["provisional identity"] != 1 {
		t.Fatalf("provisional Claude handle: held %d, unresolved %v", held, unresolved)
	}
}
