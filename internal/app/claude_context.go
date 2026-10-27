package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type claudeContextSource struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Scope    string `json:"scope"`
	Load     string `json:"load"`
	Evidence string `json:"evidence"`
	Detail   string `json:"detail,omitempty"`
	Exists   bool   `json:"exists"`
}

type claudeContextAssumptions struct {
	Sources       []claudeContextSource `json:"sources"`
	ConfigSources []claudeContextSource `json:"configSources"`
	Docs          []map[string]string   `json:"docs"`
	Notes         []string              `json:"notes"`
}

// claudeContext reconstructs Claude Code startup/context inputs from documented
// loading rules. These sources are not a wire-attested assembled prompt.
func claudeContext(cwd string, initRaw json.RawMessage) claudeContextAssumptions {
	return claudeContextAssumptions{
		Sources:       claudeContextSources(cwd, initRaw),
		ConfigSources: claudeConfigSources(cwd),
		Docs: []map[string]string{
			{"label": "Memory / CLAUDE.md", "url": "https://code.claude.com/docs/en/memory"},
			{"label": "Settings scopes", "url": "https://code.claude.com/docs/en/settings"},
			{"label": "Permissions", "url": "https://code.claude.com/docs/en/permissions"},
			{"label": "Output styles", "url": "https://code.claude.com/docs/en/output-styles"},
		},
		Notes: []string{
			"system.init is wire-attested runtime inventory; it is not the assembled prompt.",
			"Claude instruction files listed below are openable source files reconstructed from documented Claude Code loading rules.",
			"Path-scoped rules and subdirectory CLAUDE.md files can enter context later when Claude reads matching files.",
		},
	}
}

func claudeContextSources(cwd string, initRaw json.RawMessage) []claudeContextSource {
	var sources []claudeContextSource
	seen := map[string]bool{}
	add := func(src claudeContextSource) {
		if src.Path == "" || seen[src.Path] {
			return
		}
		if _, err := os.Stat(src.Path); err != nil {
			return
		}
		seen[src.Path] = true
		src.Exists = true
		src.Evidence = instructionEvidenceDocumented
		sources = append(sources, src)
	}

	for _, p := range managedClaudeMDPaths() {
		add(claudeContextSource{
			Path:   p,
			Kind:   "managed-claude-md",
			Scope:  "managed",
			Load:   "startup",
			Detail: "Organization-wide CLAUDE.md; cannot be excluded by user settings.",
		})
	}

	if home, err := os.UserHomeDir(); err == nil {
		add(claudeContextSource{
			Path:   filepath.Join(home, ".claude", "CLAUDE.md"),
			Kind:   "claude-md",
			Scope:  "user",
			Load:   "startup",
			Detail: "User instructions loaded before project instructions.",
		})
		for _, p := range markdownFiles(filepath.Join(home, ".claude", "rules")) {
			add(claudeContextSource{
				Path:   p,
				Kind:   "claude-rule",
				Scope:  "user",
				Load:   ruleLoad(p),
				Detail: "User-level rule; project rules have higher priority.",
			})
		}
	}

	for _, dir := range ancestorDirs(cwd) {
		add(claudeContextSource{
			Path:   filepath.Join(dir, "CLAUDE.md"),
			Kind:   "claude-md",
			Scope:  "project",
			Load:   "startup",
			Detail: "Loaded by walking from filesystem root toward the session cwd.",
		})
		add(claudeContextSource{
			Path:   filepath.Join(dir, ".claude", "CLAUDE.md"),
			Kind:   "claude-md",
			Scope:  "project",
			Load:   "startup",
			Detail: "Project instructions in .claude/ for this directory level.",
		})
		add(claudeContextSource{
			Path:   filepath.Join(dir, "CLAUDE.local.md"),
			Kind:   "claude-md",
			Scope:  "local",
			Load:   "startup",
			Detail: "Local instructions append after CLAUDE.md at the same directory level.",
		})
		for _, p := range markdownFiles(filepath.Join(dir, ".claude", "rules")) {
			add(claudeContextSource{
				Path:   p,
				Kind:   "claude-rule",
				Scope:  "project",
				Load:   ruleLoad(p),
				Detail: "Rule discovered recursively from .claude/rules.",
			})
		}
	}

	if p := claudeAutoMemoryPath(cwd, initRaw); p != "" {
		add(claudeContextSource{
			Path:   p,
			Kind:   "auto-memory",
			Scope:  "repository",
			Load:   "startup-truncated",
			Detail: "MEMORY.md startup entrypoint; Claude loads the first 200 lines or 25KB.",
		})
	}

	return sources
}

func claudeInstructionFiles(cwd string) []string {
	sources := claudeContextSources(cwd, nil)
	paths := make([]string, 0, len(sources))
	for _, src := range sources {
		paths = append(paths, src.Path)
	}
	return paths
}

func claudeInstructionSources(cwd string, initRaw json.RawMessage) []instructionFileSource {
	contextSources := claudeContextSources(cwd, initRaw)
	sources := make([]instructionFileSource, 0, len(contextSources))
	for _, src := range contextSources {
		sources = append(sources, instructionFileSource{
			Path:         src.Path,
			SourceKind:   src.Kind,
			SourceScope:  src.Scope,
			SourceLoad:   src.Load,
			ProvenLoaded: false,
			Evidence:     src.Evidence,
		})
	}
	return sources
}

func managedClaudeMDPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Library/Application Support/ClaudeCode/CLAUDE.md"}
	case "linux":
		return []string{"/etc/claude-code/CLAUDE.md"}
	case "windows":
		return []string{`C:\Program Files\ClaudeCode\CLAUDE.md`}
	default:
		return nil
	}
}

func managedClaudeSettingsPaths() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"/Library/Application Support/ClaudeCode/managed-settings.json"}
	case "linux":
		return []string{"/etc/claude-code/managed-settings.json"}
	case "windows":
		return []string{`C:\Program Files\ClaudeCode\managed-settings.json`}
	default:
		return nil
	}
}

func ancestorDirs(cwd string) []string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = filepath.Clean(cwd)
	}
	var dirs []string
	for dir := abs; ; {
		dirs = append(dirs, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

func markdownFiles(root string) []string {
	var paths []string
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".md") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths
}

func ruleLoad(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return "startup-or-conditional"
	}
	text := strings.TrimSpace(string(content))
	if !strings.HasPrefix(text, "---") {
		return "startup"
	}
	rest := strings.TrimPrefix(text, "---")
	end := strings.Index(rest, "---")
	if end < 0 {
		return "startup"
	}
	frontmatter := rest[:end]
	if strings.Contains(frontmatter, "\npaths:") || strings.HasPrefix(strings.TrimSpace(frontmatter), "paths:") {
		return "conditional"
	}
	return "startup"
}

func claudeAutoMemoryPath(cwd string, initRaw json.RawMessage) string {
	var init struct {
		MemoryPaths struct {
			Auto string `json:"auto"`
		} `json:"memory_paths"`
	}
	if len(initRaw) > 0 && json.Unmarshal(initRaw, &init) == nil && init.MemoryPaths.Auto != "" {
		p := expandHome(init.MemoryPaths.Auto)
		if filepath.Base(p) == "MEMORY.md" {
			return p
		}
		return filepath.Join(p, "MEMORY.md")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	root := repoRoot(cwd)
	if root == "" {
		root = cwd
	}
	return filepath.Join(home, ".claude", "projects", claudeSlugRE.ReplaceAllString(root, "-"), "memory", "MEMORY.md")
}

func repoRoot(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = filepath.Clean(cwd)
	}
	for dir := abs; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func claudeConfigSources(cwd string) []claudeContextSource {
	var sources []claudeContextSource
	seen := map[string]bool{}
	add := func(path, kind, scope, detail string) {
		if path == "" || seen[path] {
			return
		}
		if _, err := os.Stat(path); err != nil {
			return
		}
		seen[path] = true
		sources = append(sources, claudeContextSource{
			Path:     path,
			Kind:     kind,
			Scope:    scope,
			Load:     "startup",
			Evidence: instructionEvidencePresent,
			Detail:   detail,
			Exists:   true,
		})
	}
	for _, path := range managedClaudeSettingsPaths() {
		add(path, "settings", "managed", "Organization-managed Claude settings.")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".claude", "settings.json"), "settings", "user", "User settings scope.")
		add(filepath.Join(home, ".claude.json"), "mcp", "user", "Global/per-project Claude Code data and MCP configuration.")
	}
	for _, dir := range ancestorDirs(cwd) {
		add(filepath.Join(dir, ".mcp.json"), "mcp", "project", "Project MCP server configuration.")
		add(filepath.Join(dir, ".claude", "settings.json"), "settings", "project", "Project settings scope.")
		add(filepath.Join(dir, ".claude", "settings.local.json"), "settings", "local", "Local project settings scope.")
	}
	return sources
}
