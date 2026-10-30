package historical

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/inspection"
)

// Catches: a saved thread's current-setup panel reading inside a folder macOS
// guards with a privacy prompt (git status, AGENTS.md and CLAUDE.md lookup),
// which asked whether "codex-nano-bin" may access the Documents folder.
// Boundary: the Codex and Claude resolvers, cold and prepared.
func TestProtectedFolderResolutionReadsNothingThere(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy folders exist only on darwin")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "Documents", "repo")
	if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fake := &fakePassiveCodex{}
	codex := NewCodexResolver(fake, store, filepath.Join(home, ".codex"))
	claude := NewClaudeResolver(store, "/missing/claude")
	codexThread := testCodexCatalogThread(cwd, "recorded-model")
	claudeThread := testClaudeCatalogThread(cwd, "sonnet", "default", filepath.Join(home, ".claude", "projects", "p", "s.jsonl"))
	for name, resolve := range map[string]func() inspection.CurrentResolution{
		"codex":          func() inspection.CurrentResolution { return codex.Resolve(context.Background(), codexThread) },
		"codex prepared": func() inspection.CurrentResolution { return codex.ResolvePrepared(context.Background(), codexThread) },
		"claude":         func() inspection.CurrentResolution { return claude.Resolve(context.Background(), claudeThread) },
		"claude prepared": func() inspection.CurrentResolution {
			return claude.ResolvePrepared(context.Background(), claudeThread)
		},
	} {
		resolution := resolve()
		if resolution.State != "unavailable" || len(resolution.Diagnostics) != 1 || resolution.Diagnostics[0] != filemodel.ProtectedNotice {
			t.Errorf("%s: resolution = %s %q, want unavailable with the notice", name, resolution.State, resolution.Diagnostics)
		}
	}
	if len(fake.methods) != 0 {
		t.Errorf("asked the app server %v for a protected folder", fake.methods)
	}
}
