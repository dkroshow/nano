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
	// The boot warm-up calls workspace directly for every saved thread's folder
	// (seen live after the v0.1.1 deploy: git ran in ~/Documents and macOS asked).
	if _, _, err := codex.workspace(context.Background(), cwd); err == nil {
		t.Error("codex workspace resolved a protected folder")
	}
	if _, _, err := claude.workspace(context.Background(), cwd); err == nil {
		t.Error("claude workspace resolved a protected folder")
	}
	if len(fake.methods) != 0 {
		t.Errorf("asked the app server %v for a protected folder", fake.methods)
	}

	// A memory override can point into the folder from a safe cwd (seen in review).
	memory := filepath.Join(home, "Documents", "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(memory), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memory, []byte("memory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, _ := resolveHistoricalClaudeInstructions(filepath.Join(home, "Code"), inspection.ClaudeSettings{AutoMemoryDirectory: filepath.Dir(memory)})
	for _, source := range sources {
		if source.Path == memory {
			t.Errorf("listed the memory override %s in a protected folder", memory)
		}
	}
}

// Catches: a saved thread's folder outside the protected ones whose inputs
// link into one (instructions, Claude settings, a skill), and the cache
// fingerprint statting such a link; each read went through (seen in review).
// Boundary: the resolvers' workspace, which the warm-up and refreshers call.
func TestProtectedLinksFromASafeFolderAreNotRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy folders exist only on darwin")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "Code", "repo")
	documents := filepath.Join(home, "Documents")
	for _, dir := range []string{filepath.Join(cwd, ".claude", "skills", "example"), documents} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		filepath.Join(cwd, "AGENTS.md"):                                "instructions\n",
		filepath.Join(cwd, ".claude", "settings.json"):                 `{"model":"protected-model"}`,
		filepath.Join(cwd, ".claude", "skills", "example", "SKILL.md"): "---\nname: protected-skill\n---\n",
	}
	for link, content := range links {
		target := filepath.Join(documents, filepath.Base(link))
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	codex, _, err := NewCodexResolver(&fakePassiveCodex{}, store, filepath.Join(home, ".codex")).workspace(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range codex.Instructions {
		if source.BytesRead > 0 && filepath.Base(source.Path) == "AGENTS.md" {
			t.Errorf("read %d bytes of instructions through a link into a protected folder", source.BytesRead)
		}
	}
	claude, _, err := NewClaudeResolver(store, "/missing/claude").workspace(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if claude.Settings.Model == "protected-model" {
		t.Error("read Claude settings through a link into a protected folder")
	}
	for _, skill := range claude.Skills {
		if skill.Name == "protected-skill" {
			t.Error("read a skill's metadata through a link into a protected folder")
		}
	}
	target := filepath.Join(documents, "SKILL.md")
	before := fingerprintPaths([]string{target})
	if err := os.WriteFile(target, []byte("longer content than before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fingerprintPaths([]string{target}) != before {
		t.Error("the cache fingerprint statted a file in a protected folder")
	}
}
