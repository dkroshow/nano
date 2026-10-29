package hosted

import (
	"context"
	"encoding/json"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/threadindex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalResumeProviderResolversPreserveKnownAndUnknownPosture(t *testing.T) {
	s, _ := newResumeCoordinator(t)
	s.Defaults.AllowedTools = "Read,Write"

	for _, provider := range []string{"claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			nativeID := "external-" + provider
			catalog := externalResumeCatalog(t, provider, nativeID, s.Defaults.CWD)
			// Unknown permission must remain unknown; Nano must not substitute
			// its new-session allowlist or a broader permission mode.
			catalog.setPermission(t, "")
			s.Catalog = catalog.index
			resolved, err := s.resolveSpawn(SpawnRequest{ProviderRequest: sessions.ProviderRequest{Provider: provider, Mode: "resume", ThreadID: nativeID}})
			if err != nil {
				t.Fatal(err)
			}
			if !resolved.ExternalResumePosture || resolved.Cwd != s.Defaults.CWD || resolved.PermissionMode != "" {
				t.Fatalf("resolved = %#v", resolved)
			}
			switch provider {
			case "claude":
				joined := strings.Join(resolved.ClaudeArgs, " ")
				if !strings.Contains(joined, "--model gpt-external") || !strings.Contains(joined, "--effort high") {
					t.Fatalf("Claude args lost recorded posture: %q", joined)
				}
				if strings.Contains(joined, "--allowedTools") || strings.Contains(joined, "Read,Write") {
					t.Fatalf("Claude args leaked new-session defaults: %q", joined)
				}
			case "grok":
				if resolved.Model != "" || resolved.Effort != "" || resolved.GrokResumeModel != "gpt-external" {
					t.Fatalf("Grok resume posture = %#v", resolved)
				}
			}
		})
	}
}

type externalResumeFixture struct {
	index                                   *threadindex.Service
	store                                   *history.Store
	root                                    history.SourceRoot
	path, provider, native, cwd, permission string
}

func externalResumeCatalog(t *testing.T, provider, nativeID, cwd string) *externalResumeFixture {
	t.Helper()
	f := &externalResumeFixture{provider: provider, native: nativeID, cwd: cwd, permission: "never"}
	kind := history.RootCodexSessions
	root := t.TempDir()
	f.path = filepath.Join(root, "rollout-"+nativeID+".jsonl")
	if provider == "claude" {
		kind = history.RootClaudeProjects
		f.path = filepath.Join(root, "-workspace", nativeID+".jsonl")
	}
	if provider == "grok" {
		kind = history.RootGrokSessions
		f.path = filepath.Join(root, nativeID, "summary.json")
	}
	f.root = history.SourceRoot{Provider: history.Provider(provider), Kind: kind, Path: root}
	f.write(t, nativeID)
	var err error
	f.store, err = history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.index, err = threadindex.New(context.Background(), f.store, threadindex.Options{NativeIDProvisional: sessions.NativeThreadIDProvisional, ClaudeProjectSlug: setup.ClaudeProjectSlug, Roots: []history.SourceRoot{f.root}, DisableWatcher: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.index.Close)
	f.refresh(t)
	return f
}

func (f *externalResumeFixture) write(t *testing.T, native string) {
	t.Helper()
	var rows []map[string]any
	switch f.provider {
	case "codex":
		rows = []map[string]any{
			{"type": "session_meta", "payload": map[string]any{"id": native, "cwd": f.cwd}},
			{"type": "turn_context", "payload": map[string]any{"cwd": f.cwd, "model": "gpt-external", "effort": "high", "service_tier": "priority", "approval_policy": f.permission}},
		}
	case "claude":
		rows = []map[string]any{
			{"type": "user", "sessionId": native, "cwd": f.cwd, "effort": "high", "permissionMode": f.permission, "timestamp": "2026-10-01T00:00:00Z", "message": map[string]any{"role": "user", "content": "Resume fixture"}},
			{"type": "assistant", "sessionId": native, "timestamp": "2026-10-01T00:00:01Z", "message": map[string]any{"role": "assistant", "model": "gpt-external", "content": []any{map[string]any{"type": "text", "text": "Saved answer"}}}},
		}
	case "grok":
		rows = []map[string]any{{"info": map[string]any{"id": native, "cwd": f.cwd}, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:01Z", "current_model_id": "gpt-external", "reasoning_effort": "high", "sandbox_profile": f.permission}}
	}
	var out []byte
	for _, row := range rows {
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, data...), '\n')
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, out, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *externalResumeFixture) refresh(t *testing.T) {
	t.Helper()
	f.index.RequestRefresh()
	waitForOwnerCatalog(t, func() bool {
		page, err := f.index.ListThreads(context.Background(), threadindex.ListQuery{IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: "include", Limit: 50})
		return err == nil && len(page.Items) == 1
	})
}

func (f *externalResumeFixture) setPermission(t *testing.T, value string) {
	t.Helper()
	f.permission = value
	f.write(t, f.native)
	f.index.FileChanged(f.path)
	waitForOwnerCatalog(t, func() bool {
		page, err := f.index.ListThreads(context.Background(), threadindex.ListQuery{IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true, Archived: "include", Limit: 50})
		return err == nil && len(page.Items) == 1 && inspection.KnownMetadataValue(page.Items[0].Metadata.Permission.State, page.Items[0].Metadata.Permission.Value) == value
	})
}
func waitForOwnerCatalog(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("catalog condition did not become true")
}
