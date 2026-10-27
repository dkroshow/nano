package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveStandardRootsPrecedenceAndOverrides(t *testing.T) {
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	initialized := filepath.Join(home, "initialized-codex")
	claude := filepath.Join(home, "claude-config")
	roots, err := ResolveStandardRoots(RootOptions{
		HomeDir: home, CodexHomeFromInitialize: initialized,
		LookupEnv: func(key string) (string, bool) {
			values := map[string]string{"CODEX_HOME": filepath.Join(home, "env-codex"), "CLAUDE_CONFIG_DIR": claude}
			value, ok := values[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 4 {
		t.Fatalf("roots = %d, want 4", len(roots))
	}
	if got, want := roots[0].Path, filepath.Join(initialized, "sessions"); got != want {
		t.Fatalf("Codex active root = %q, want %q", got, want)
	}
	if got, want := roots[1].Path, filepath.Join(initialized, "archived_sessions"); got != want || !roots[1].Archived {
		t.Fatalf("Codex archived root = %#v, want path %q archived", roots[1], want)
	}
	if got, want := roots[2].Path, filepath.Join(claude, "projects"); got != want {
		t.Fatalf("Claude root = %q, want %q", got, want)
	}
	if got, want := roots[3].Path, filepath.Join(canonicalHome, ".grok", "sessions"); got != want || roots[3].Provider != ProviderGrok || roots[3].Kind != RootGrokSessions {
		t.Fatalf("Grok root = %#v, want path %q", roots[3], want)
	}
}

func TestResolveStandardRootsUsesEnvironmentThenDefaults(t *testing.T) {
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	codexEnv := filepath.Join(home, "codex-env")
	roots, err := ResolveStandardRoots(RootOptions{
		HomeDir: home,
		LookupEnv: func(key string) (string, bool) {
			if key == "CODEX_HOME" {
				return codexEnv, true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := roots[0].Path, filepath.Join(codexEnv, "sessions"); got != want {
		t.Fatalf("Codex root = %q, want %q", got, want)
	}
	if got, want := roots[2].Path, filepath.Join(canonicalHome, ".claude", "projects"); got != want {
		t.Fatalf("Claude default root = %q, want %q", got, want)
	}
	if got, want := roots[3].Path, filepath.Join(canonicalHome, ".grok", "sessions"); got != want {
		t.Fatalf("Grok default root = %q, want %q", got, want)
	}
}

func TestResolveStandardRootsCanonicalizesExistingChildSymlinks(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	claudeHome := filepath.Join(home, "claude")
	codexTarget := filepath.Join(home, "actual-codex-sessions")
	claudeTarget := filepath.Join(home, "actual-claude-projects")
	for _, path := range []string{codexHome, claudeHome, codexTarget, claudeTarget} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(codexTarget, filepath.Join(codexHome, "sessions")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(claudeTarget, filepath.Join(claudeHome, "projects")); err != nil {
		t.Fatal(err)
	}
	roots, err := ResolveStandardRoots(RootOptions{
		HomeDir: home, CodexHomeFromInitialize: codexHome,
		LookupEnv: func(key string) (string, bool) {
			if key == "CLAUDE_CONFIG_DIR" {
				return claudeHome, true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalCodex, _ := filepath.EvalSymlinks(codexTarget)
	canonicalClaude, _ := filepath.EvalSymlinks(claudeTarget)
	if roots[0].Path != canonicalCodex || roots[2].Path != canonicalClaude {
		t.Fatalf("child roots not canonical: Codex %q Claude %q", roots[0].Path, roots[2].Path)
	}
}

func TestDiscoverRootProviderPatternsAndSubagentClassification(t *testing.T) {
	temp := t.TempDir()
	codexRoot := SourceRoot{Provider: ProviderCodex, Kind: RootCodexSessions, Path: filepath.Join(temp, "codex", "sessions")}
	codexArchivedRoot := SourceRoot{Provider: ProviderCodex, Kind: RootCodexArchived, Path: filepath.Join(temp, "codex", "archived_sessions"), Archived: true}
	claudeRoot := SourceRoot{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: filepath.Join(temp, "claude", "projects")}

	writeTestFile(t, filepath.Join(codexRoot.Path, "2026", "07", "rollout-2026-07-09-11111111-1111-4111-8111-111111111111.jsonl"))
	writeTestFile(t, filepath.Join(codexRoot.Path, "2026", "07", "other.jsonl"))
	writeTestFile(t, filepath.Join(codexArchivedRoot.Path, "rollout-2026-07-08-22222222-2222-4222-8222-222222222222.jsonl"))
	writeTestFile(t, filepath.Join(claudeRoot.Path, "project", "33333333-3333-4333-8333-333333333333.jsonl"))
	writeTestFile(t, filepath.Join(claudeRoot.Path, "project", "33333333-3333-4333-8333-333333333333", "subagents", "agent-worker-1.jsonl"))
	writeTestFile(t, filepath.Join(claudeRoot.Path, "project", "notes.txt"))

	external := filepath.Join(temp, "external")
	writeTestFile(t, filepath.Join(external, "should-not-be-seen.jsonl"))
	if err := os.Symlink(external, filepath.Join(claudeRoot.Path, "escape")); err != nil {
		t.Fatal(err)
	}

	codex, err := DiscoverRoot(context.Background(), codexRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !codex.Present || len(codex.Candidates) != 1 {
		t.Fatalf("Codex discovery = present %v, %d candidates; want true, 1", codex.Present, len(codex.Candidates))
	}
	if codex.Candidates[0].Kind != ThreadMain || codex.Candidates[0].Size == 0 {
		t.Fatalf("unexpected Codex candidate: %#v", codex.Candidates[0])
	}
	archived, err := DiscoverRoot(context.Background(), codexArchivedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived.Candidates) != 1 || !archived.Candidates[0].Archived {
		t.Fatalf("unexpected Codex archived discovery: %#v", archived)
	}

	claude, err := DiscoverRoot(context.Background(), claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(claude.Candidates) != 2 {
		t.Fatalf("Claude candidates = %d, want 2: %#v", len(claude.Candidates), claude.Candidates)
	}
	kinds := map[ThreadKind]int{}
	for _, candidate := range claude.Candidates {
		kinds[candidate.Kind]++
		if candidate.RelativePath == "" || !filepath.IsAbs(candidate.Path) {
			t.Fatalf("candidate path metadata missing: %#v", candidate)
		}
	}
	if kinds[ThreadMain] != 1 || kinds[ThreadSubagent] != 1 {
		t.Fatalf("Claude kinds = %#v, want one main and one subagent", kinds)
	}
}

func TestDiscoverRootGrokSessionShape(t *testing.T) {
	temp := t.TempDir()
	grokRoot := SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: filepath.Join(temp, "grok", "sessions")}

	// Current-format session: summary.json plus updates.jsonl.
	writeTestFile(t, filepath.Join(grokRoot.Path, "-home-user-example", "11111111-1111-4111-8111-111111111111", "summary.json"))
	writeTestFile(t, filepath.Join(grokRoot.Path, "-home-user-example", "11111111-1111-4111-8111-111111111111", "updates.jsonl"))
	// Older session: summary.json only, no updates.jsonl yet.
	writeTestFile(t, filepath.Join(grokRoot.Path, "-home-user-example", "22222222-2222-4222-8222-222222222222", "summary.json"))
	// Non-history files under a session directory must not become candidates.
	writeTestFile(t, filepath.Join(grokRoot.Path, "-home-user-example", "11111111-1111-4111-8111-111111111111", "chat_history.jsonl"))
	writeTestFile(t, filepath.Join(grokRoot.Path, "-home-user-example", "11111111-1111-4111-8111-111111111111", "auth.json"))

	result, err := DiscoverRoot(context.Background(), grokRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Present || len(result.Candidates) != 2 {
		t.Fatalf("Grok discovery = present %v, %d candidates; want true, 2: %#v", result.Present, len(result.Candidates), result.Candidates)
	}
	wantBases := map[string]int{"updates.jsonl": 1, "summary.json": 1}
	for _, candidate := range result.Candidates {
		wantBases[filepath.Base(candidate.Path)]--
		if candidate.Kind != ThreadMain {
			t.Fatalf("Grok candidate kind = %v, want ThreadMain: %#v", candidate.Kind, candidate)
		}
	}
	for base, remaining := range wantBases {
		if remaining != 0 {
			t.Fatalf("Grok candidate count for %s = %d, candidates %#v", base, 1-remaining, result.Candidates)
		}
	}
}

func TestDiscoverRootGrokSubagentMetaEstablishesLineage(t *testing.T) {
	temp := t.TempDir()
	root := SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: filepath.Join(temp, "grok", "sessions")}
	parent := "11111111-1111-4111-8111-111111111111"
	child := "22222222-2222-4222-8222-222222222222"
	encoded := "-home-user-example"
	writeTestFile(t, filepath.Join(root.Path, encoded, parent, "summary.json"))
	writeTestFile(t, filepath.Join(root.Path, encoded, child, "summary.json"))
	writeTestFile(t, filepath.Join(root.Path, encoded, child, "updates.jsonl"))
	meta := filepath.Join(root.Path, encoded, parent, "subagents", child, "meta.json")
	if err := os.MkdirAll(filepath.Dir(meta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(`{"parent_session_id":"`+parent+`","child_session_id":"`+child+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := DiscoverRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.Candidates {
		if filepath.Base(filepath.Dir(candidate.Path)) != child {
			continue
		}
		if candidate.Kind != ThreadSubagent || candidate.ParentNativeID != parent {
			t.Fatalf("child candidate = %#v", candidate)
		}
		return
	}
	t.Fatalf("child %s was not discovered: %#v", child, result.Candidates)
}

func TestDiscoverRootGrokConflictingLineageIsQuarantined(t *testing.T) {
	temp := t.TempDir()
	root := SourceRoot{Provider: ProviderGrok, Kind: RootGrokSessions, Path: filepath.Join(temp, "grok", "sessions")}
	child := "22222222-2222-4222-8222-222222222222"
	writeTestFile(t, filepath.Join(root.Path, "child-cwd", child, "summary.json"))
	for _, parent := range []string{"parent-a", "parent-b"} {
		meta := filepath.Join(root.Path, parent+"-cwd", parent, "subagents", child, "meta.json")
		if err := os.MkdirAll(filepath.Dir(meta), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(meta, []byte(`{"parent_session_id":"`+parent+`","child_session_id":"`+child+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	result, err := DiscoverRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.IdentityConflicts != 1 {
		t.Fatalf("identity conflicts = %#v", result.Summary)
	}
	for _, candidate := range result.Candidates {
		if filepath.Base(filepath.Dir(candidate.Path)) == child && (candidate.Kind != ThreadMain || candidate.ParentNativeID != "") {
			t.Fatalf("conflicting lineage was applied: %#v", candidate)
		}
	}
}

func TestDiscoverRootMissingIsSuccessful(t *testing.T) {
	root := SourceRoot{Provider: ProviderCodex, Kind: RootCodexArchived, Path: filepath.Join(t.TempDir(), "missing"), Archived: true}
	result, err := DiscoverRoot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Present || len(result.Candidates) != 0 {
		t.Fatalf("missing root result = %#v", result)
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
