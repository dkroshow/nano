package app

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

func TestHistoricalClaudeResolverSharesWorkspaceAndSeparatesEvidence(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	repo := filepath.Join(root, "repo")
	child := filepath.Join(repo, "nested")
	for _, directory := range []string{
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".claude", "skills", "user-skill"),
		filepath.Join(repo, ".git"),
		filepath.Join(repo, ".claude", "rules"),
		filepath.Join(repo, ".claude", "skills", "project-skill"),
		filepath.Join(repo, "docs"),
		child,
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)

	writeClaudeTestFile(t, filepath.Join(home, ".claude", "settings.json"), `{
  "model": "user-model",
  "permissions": {"defaultMode": "acceptEdits", "allow": ["Read"]},
  "enabledPlugins": {"user-plugin@example": true}
}`)
	writeClaudeTestFile(t, filepath.Join(repo, ".claude", "settings.json"), `{
  "model": "configured-now",
  "effortLevel": "high",
  "autoMemoryEnabled": true,
  "permissions": {"defaultMode": "manual", "deny": ["Read(./secret/**)"]},
  "enabledPlugins": {"project-plugin@example": true}
}`)
	writeClaudeTestFile(t, filepath.Join(home, ".claude.json"), `{
  "apiKey": "must-not-be-cached",
  "projects": {
    "`+repo+`": {"mcpServers": {"docs": {"headers": {"Authorization": "secret"}}}}
  }
}`)
	writeClaudeTestFile(t, filepath.Join(home, ".claude", "CLAUDE.md"), "user instructions")
	writeClaudeTestFile(t, filepath.Join(repo, "CLAUDE.md"), "project instructions\nSee @docs/shared.md")
	writeClaudeTestFile(t, filepath.Join(repo, "docs", "shared.md"), "shared instructions")
	writeClaudeTestFile(t, filepath.Join(repo, ".claude", "rules", "typescript.md"), "---\npaths:\n  - \"**/*.ts\"\n---\nTypeScript rule")
	writeClaudeTestFile(t, filepath.Join(home, ".claude", "skills", "user-skill", "SKILL.md"), "---\nname: user-skill\ndescription: User skill\n---")
	writeClaudeTestFile(t, filepath.Join(repo, ".claude", "skills", "project-skill", "SKILL.md"), "---\nname: project-skill\ndescription: Project skill\n---")

	memoryPath := setup.ClaudeAutoMemoryPath(child, nil)
	if err := os.MkdirAll(filepath.Dir(memoryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeClaudeTestFile(t, memoryPath, "remembered")

	versionBinary := filepath.Join(root, "2.1.220")
	writeClaudeTestFile(t, versionBinary, "")
	if err := os.Chmod(versionBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeBin := filepath.Join(root, "claude")
	if err := os.Symlink(versionBinary, claudeBin); err != nil {
		t.Fatal(err)
	}

	transcript := filepath.Join(root, "thread.jsonl")
	writeClaudeTestFile(t, transcript, strings.Join([]string{
		`{"type":"user","sessionId":"one","cwd":"` + child + `","version":"2.1.207","entrypoint":"cli","permissionMode":"bypassPermissions"}`,
		`{"type":"assistant","sessionId":"one","cwd":"` + child + `","version":"2.1.207","message":{"model":"recorded-model"}}`,
	}, "\n"))

	store, err := history.Open(filepath.Join(root, "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := newHistoricalClaudeResolver(store, claudeBin)
	resolver.now = func() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }

	first := testClaudeCatalogThread(child, "recorded-model", "bypassPermissions", transcript)
	resolution := resolver.Resolve(context.Background(), first)
	if resolution.State != "ready" {
		t.Fatalf("resolution state = %s diagnostics=%v", resolution.State, resolution.Diagnostics)
	}
	if got := resolution.Values["model"].Value; got != "recorded-model" {
		t.Fatalf("recorded model = %#v", got)
	}
	if got := resolution.Values["configuredModel"].Value; got != "configured-now" {
		t.Fatalf("configured model = %#v", got)
	}
	if got := resolution.Values["permissionMode"].Value; got != "bypassPermissions" {
		t.Fatalf("recorded permission = %#v", got)
	}
	if got := resolution.Values["configuredPermissionMode"].Value; got != "manual" {
		t.Fatalf("configured permission = %#v", got)
	}
	if got := resolution.Values["recordedClaudeVersion"].Value; got != "2.1.207" {
		t.Fatalf("recorded version = %#v", got)
	}
	if got := resolution.Values["currentClaudeVersion"].Value; got != "2.1.220" {
		t.Fatalf("current version = %#v", got)
	}

	// Ancestor sources are allowed by design. Assert the complete fixture-owned
	// set without counting unrelated sources above the temporary directory.
	instructions := map[string]string{}
	for _, source := range resolution.Values["instructionSources"].Value.([]setup.ClaudeContextSource) {
		if strings.HasPrefix(source.Path, root+string(os.PathSeparator)) {
			instructions[source.Path] = source.Kind
		}
	}
	wantInstructions := map[string]string{
		filepath.Join(home, ".claude", "CLAUDE.md"):              "claude-md",
		filepath.Join(repo, "CLAUDE.md"):                         "claude-md",
		filepath.Join(repo, "docs", "shared.md"):                 "claude-import",
		filepath.Join(repo, ".claude", "rules", "typescript.md"): "claude-rule",
		memoryPath: "auto-memory",
	}
	if !maps.Equal(instructions, wantInstructions) {
		t.Fatalf("fixture instruction sources = %#v, want %#v", instructions, wantInstructions)
	}
	if got := resolution.Values["mcpServers"].Value.([]string); len(got) != 1 || got[0] != "docs" {
		t.Fatalf("MCP names = %#v", got)
	}
	skills := map[string]resolvedSkill{}
	for _, skill := range resolution.Values["skills"].Value.([]resolvedSkill) {
		if strings.HasPrefix(skill.Path, root+string(os.PathSeparator)) {
			skills[skill.Path] = skill
		}
	}
	userSkillPath := filepath.Join(home, ".claude", "skills", "user-skill", "SKILL.md")
	projectSkillPath := filepath.Join(repo, ".claude", "skills", "project-skill", "SKILL.md")
	wantSkills := map[string]resolvedSkill{
		userSkillPath:    {Name: "user-skill", Description: "User skill", Path: userSkillPath, Scope: "user"},
		projectSkillPath: {Name: "project-skill", Description: "Project skill", Path: projectSkillPath, Scope: "project"},
	}
	if !maps.Equal(skills, wantSkills) {
		t.Fatalf("fixture skills = %#v, want %#v", skills, wantSkills)
	}

	// A second thread in the same workspace reuses the snapshot. Changing the
	// file between reads must not create thread-by-thread filesystem work.
	writeClaudeTestFile(t, filepath.Join(repo, ".claude", "settings.json"), `{"model":"changed-after-snapshot"}`)
	second := testClaudeCatalogThread(child, "other-recorded-model", "manual", transcript)
	secondResolution := resolver.Resolve(context.Background(), second)
	if got := secondResolution.Values["model"].Value; got != "other-recorded-model" {
		t.Fatalf("second recorded model = %#v", got)
	}
	if got := secondResolution.Values["configuredModel"].Value; got != "configured-now" {
		t.Fatalf("workspace snapshot was not reused: %#v", got)
	}

	row, ok, err := store.LoadClaudeWorkspaceResolution(context.Background(), child, historicalClaudeResolverVersion)
	if err != nil || !ok {
		t.Fatalf("load cached Claude resolution: ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(row.Payload), "must-not-be-cached") ||
		strings.Contains(string(row.Payload), "Authorization") {
		t.Fatal("cached payload contains Claude configuration secrets")
	}
}

func TestHistoricalClaudeResolverDoesNotRunForMissingCWD(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := newHistoricalClaudeResolver(store, "/missing/claude")
	resolution := resolver.Resolve(context.Background(), sidebar.Thread{Thread: threadindex.Thread{Provider: "claude"}})
	if resolution.State != "unavailable" {
		t.Fatalf("resolution state = %s", resolution.State)
	}
}

func testClaudeCatalogThread(cwd, model, permission, sourcePath string) sidebar.Thread {
	evidence := "native_durable"
	providerVersion := "2.1.207"
	sources := []threadindex.SourceDetail{{Path: sourcePath}}
	return sidebar.Thread{Thread: threadindex.Thread{Provider: "claude", ProviderVersion: &providerVersion, Metadata: threadindex.Metadata{
		CWD:        threadindex.Value{State: "known", Value: &cwd, Evidence: &evidence},
		Model:      threadindex.Value{State: "known", Value: &model, Evidence: &evidence},
		Effort:     threadindex.Value{State: "unknown"},
		Permission: threadindex.Value{State: "known", Value: &permission, Evidence: &evidence},
	}, Sources: &sources}}
}

func writeClaudeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
