package historical

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	filemodel "github.com/dkroshow/nano/internal/files"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/setup"

	"github.com/dkroshow/nano/internal/threadindex"
)

const historicalClaudeResolverVersion = "claude-docs-2.1.220+nano-1"

type claudeWorkspaceSnapshot struct {
	ResolvedAt    time.Time
	TargetCWD     string
	Settings      inspection.ClaudeSettings
	Instructions  []setup.ClaudeContextSource
	ConfigSources []setup.ClaudeContextSource
	Skills        []resolvedSkill
	MCPServers    []string
	Plugins       []string
	CLIVersion    string
	Git           inspection.Git
	Diagnostics   []string
}

type persistedClaudeWorkspaceSnapshot struct {
	Settings      inspection.ClaudeSettings   `json:"settings"`
	Instructions  []setup.ClaudeContextSource `json:"instructions"`
	ConfigSources []setup.ClaudeContextSource `json:"configSources"`
	Skills        []resolvedSkill             `json:"skills"`
	MCPServers    []string                    `json:"mcpServers"`
	Plugins       []string                    `json:"plugins"`
	CLIVersion    string                      `json:"cliVersion,omitempty"`
	Git           inspection.Git              `json:"git"`
	Diagnostics   []string                    `json:"diagnostics"`
}

// ClaudeResolver resolves documented current Claude workspace inputs.
type ClaudeResolver struct {
	store     *history.Store
	claudeBin string
	ttl       time.Duration
	now       func() time.Time

	mu            sync.Mutex
	memory        map[string]claudeWorkspaceSnapshot
	inflight      map[string]chan struct{}
	refreshErrors map[string]string
}

type claudeResolutionTarget struct {
	CWD             string
	RecordedVersion string
	Entrypoint      string
	Diagnostics     []string
}

// NewClaudeResolver shares workspace snapshots through the existing history store.
func NewClaudeResolver(store *history.Store, claudeBin string) *ClaudeResolver {
	if store == nil {
		return nil
	}
	return &ClaudeResolver{
		store: store, claudeBin: claudeBin,
		ttl: historicalResolutionTTL, now: time.Now,
		memory: map[string]claudeWorkspaceSnapshot{}, inflight: map[string]chan struct{}{},
		refreshErrors: map[string]string{},
	}
}

func (r *ClaudeResolver) Resolve(ctx context.Context, thread threadindex.Thread) inspection.CurrentResolution {
	target := readClaudeResolutionTarget(thread)
	if target.CWD == "" {
		return unavailableClaudeResolution("", "The Claude transcript does not record a working directory.")
	}
	cwd := filepath.Clean(target.CWD)
	if filemodel.Protected(cwd) {
		return unavailableClaudeResolution(cwd, filemodel.ProtectedNotice)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return unavailableClaudeResolution(cwd, fmt.Sprintf("The recorded working directory is unavailable: %v", err))
	}
	if !info.IsDir() {
		return unavailableClaudeResolution(cwd, "The recorded working directory is not a directory.")
	}

	snapshot, stale, err := r.workspace(ctx, cwd)
	if err != nil {
		return inspection.CurrentResolution{
			State: "failed", ResolverVersion: historicalClaudeResolverVersion, TargetCWD: cwd,
			Values: map[string]inspection.ResolvedValue{}, Diagnostics: []string{err.Error()},
		}
	}
	resolution := overlayClaudeThreadResolution(thread, snapshot, stale, target)
	resolution.Diagnostics = append(resolution.Diagnostics, target.Diagnostics...)
	return resolution
}

func unavailableClaudeResolution(cwd, diagnostic string) inspection.CurrentResolution {
	return inspection.CurrentResolution{
		State: "unavailable", ResolverVersion: historicalClaudeResolverVersion, TargetCWD: cwd,
		Values: map[string]inspection.ResolvedValue{}, Diagnostics: []string{diagnostic},
	}
}

func (r *ClaudeResolver) workspace(ctx context.Context, cwd string) (claudeWorkspaceSnapshot, bool, error) {
	if filemodel.Protected(cwd) {
		return claudeWorkspaceSnapshot{}, false, errors.New(filemodel.ProtectedNotice)
	}
	for {
		r.mu.Lock()
		if snapshot, ok := r.memory[cwd]; ok && r.now().Sub(snapshot.ResolvedAt) < r.ttl {
			r.mu.Unlock()
			return snapshot, false, nil
		}
		if wait, ok := r.inflight[cwd]; ok {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return claudeWorkspaceSnapshot{}, false, ctx.Err()
			}
		}
		wait := make(chan struct{})
		r.inflight[cwd] = wait
		r.mu.Unlock()

		snapshot, fallback, err := r.resolveOrLoad(ctx, cwd)
		r.mu.Lock()
		if err == nil {
			r.memory[cwd] = snapshot
		}
		delete(r.inflight, cwd)
		close(wait)
		r.mu.Unlock()
		return snapshot, fallback, err
	}
}

func (r *ClaudeResolver) resolveOrLoad(ctx context.Context, cwd string) (claudeWorkspaceSnapshot, bool, error) {
	cached, cachedOK, cacheErr := r.loadPersisted(ctx, cwd)
	if cacheErr != nil {
		cachedOK = false
	}
	if cachedOK && r.now().Sub(cached.ResolvedAt) < r.ttl {
		return cached, false, nil
	}

	fresh, err := r.resolveWorkspace(ctx, cwd)
	if err != nil {
		if cachedOK {
			cached.Diagnostics = append(cached.Diagnostics, "Refresh failed; showing the last successful resolution: "+err.Error())
			return cached, true, nil
		}
		return claudeWorkspaceSnapshot{}, false, err
	}
	if err := r.savePersisted(ctx, fresh); err != nil {
		fresh.Diagnostics = append(fresh.Diagnostics, "Resolved setup could not be cached: "+err.Error())
	}
	return fresh, false, nil
}

func (r *ClaudeResolver) resolveWorkspace(ctx context.Context, cwd string) (claudeWorkspaceSnapshot, error) {
	settings, settingsDiagnostics := resolveClaudeSettings(cwd)
	instructions, instructionDiagnostics := resolveHistoricalClaudeInstructions(cwd, settings)
	configSources := setup.ClaudeConfigSources(cwd)
	skills, skillDiagnostics := resolveClaudeSkills(cwd)
	mcpServers, mcpDiagnostics := resolveClaudeMCPNames(cwd, configSources)

	plugins := make([]string, 0, len(settings.EnabledPlugins))
	for name, enabled := range settings.EnabledPlugins {
		if enabled {
			plugins = append(plugins, name)
		}
	}
	sort.Strings(plugins)

	diagnostics := append([]string{}, settingsDiagnostics...)
	diagnostics = append(diagnostics, instructionDiagnostics...)
	diagnostics = append(diagnostics, skillDiagnostics...)
	diagnostics = append(diagnostics, mcpDiagnostics...)
	return claudeWorkspaceSnapshot{
		ResolvedAt: r.now().UTC(), TargetCWD: cwd, Settings: settings,
		Instructions: instructions, ConfigSources: configSources, Skills: skills,
		MCPServers: mcpServers, Plugins: plugins, CLIVersion: inferClaudeBinaryVersion(r.claudeBin),
		Git: observeGit(ctx, cwd), Diagnostics: diagnostics,
	}, nil
}

func overlayClaudeThreadResolution(
	thread threadindex.Thread,
	snapshot claudeWorkspaceSnapshot,
	stale bool,
	target claudeResolutionTarget,
) inspection.CurrentResolution {
	values := map[string]inspection.ResolvedValue{
		"cwd":                knownResolved(snapshot.TargetCWD, "thread_transcript"),
		"instructionSources": knownResolved(snapshot.Instructions, "documented_rules_now"),
		"configSources":      knownResolved(snapshot.ConfigSources, "present_now"),
		"skills":             knownResolved(snapshot.Skills, "discovered_now"),
		"mcpServers":         knownResolved(snapshot.MCPServers, "configured_now"),
		"plugins":            knownResolved(snapshot.Plugins, "configured_now"),
		"permissions":        knownResolved(snapshot.Settings.Permissions, "file_settings_now"),
		"systemPrompt":       {State: "unsupported", Source: "claude_wire_boundary"},
		"liveTools":          {State: "unsupported", Source: "live_only"},
	}
	values["model"] = catalogResolved(thread.Metadata.Model, "thread_recorded")
	values["reasoningEffort"] = catalogResolved(thread.Metadata.Effort, "thread_recorded")
	values["permissionMode"] = catalogResolved(thread.Metadata.Permission, "thread_recorded")
	values["configuredModel"] = stringResolved(snapshot.Settings.Model, "file_settings_now")
	values["configuredEffort"] = stringResolved(snapshot.Settings.Effort, "file_settings_now")
	values["configuredPermissionMode"] = stringResolved(snapshot.Settings.Permissions.DefaultMode, "file_settings_now")
	values["outputStyle"] = stringResolved(snapshot.Settings.OutputStyle, "file_settings_now")
	values["recordedClaudeVersion"] = stringResolved(target.RecordedVersion, "thread_transcript")
	values["currentClaudeVersion"] = stringResolved(snapshot.CLIVersion, "claude_binary_path")
	values["entrypoint"] = stringResolved(target.Entrypoint, "thread_transcript")
	if snapshot.Settings.AutoMemoryEnabled == nil {
		values["autoMemoryEnabled"] = inspection.ResolvedValue{State: "unknown", Source: "file_settings_now"}
	} else {
		values["autoMemoryEnabled"] = knownResolved(*snapshot.Settings.AutoMemoryEnabled, "file_settings_now")
	}
	if snapshot.Git.Root == "" {
		values["git"] = inspection.ResolvedValue{State: "unsupported", Source: "resolved_now"}
	} else {
		values["git"] = knownResolved(snapshot.Git, "resolved_now")
	}

	state := "ready"
	if stale {
		state = "stale"
	}
	return inspection.CurrentResolution{
		State: state, ResolverVersion: historicalClaudeResolverVersion,
		ResolvedAt: snapshot.ResolvedAt.Format(time.RFC3339Nano), TargetCWD: snapshot.TargetCWD,
		Stale: stale, Values: values, Diagnostics: append([]string{}, snapshot.Diagnostics...),
	}
}

func catalogResolved(value threadindex.Value, source string) inspection.ResolvedValue {
	if known := knownCatalogValue(value); known != "" {
		return knownResolved(known, source)
	}
	return inspection.ResolvedValue{State: value.State, Source: source}
}

func stringResolved(value, source string) inspection.ResolvedValue {
	if strings.TrimSpace(value) == "" {
		return inspection.ResolvedValue{State: "unknown", Source: source}
	}
	return knownResolved(value, source)
}

func readClaudeResolutionTarget(thread threadindex.Thread) claudeResolutionTarget {
	target := claudeResolutionTarget{
		CWD:        knownCatalogValue(thread.Metadata.CWD),
		Entrypoint: knownCatalogValue(thread.Metadata.Origin),
	}
	if thread.ProviderVersion != nil {
		target.RecordedVersion = strings.TrimSpace(*thread.ProviderVersion)
	}
	return target
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func resolveClaudeSettings(cwd string) (inspection.ClaudeSettings, []string) {
	var paths []string
	for _, path := range claudeSettingsLayerPaths(cwd) {
		if !filemodel.Protected(path) {
			paths = append(paths, path)
		}
	}
	return inspection.ReadClaudeSettings(paths)
}

func claudeSettingsLayerPaths(cwd string) []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".claude", "settings.json"))
	}
	for _, dir := range setup.AncestorDirs(cwd) {
		paths = append(paths,
			filepath.Join(dir, ".claude", "settings.json"),
			filepath.Join(dir, ".claude", "settings.local.json"),
		)
	}
	paths = append(paths, setup.ManagedClaudeSettingsPaths()...)
	return uniqueNonempty(paths)
}

func resolveHistoricalClaudeInstructions(cwd string, settings inspection.ClaudeSettings) ([]setup.ClaudeContextSource, []string) {
	sources := setup.ClaudeContextSources(cwd, nil)
	filtered := make([]setup.ClaudeContextSource, 0, len(sources))
	for _, source := range sources {
		if source.Kind == "auto-memory" {
			continue
		}
		if source.Scope != "managed" && matchesClaudeExclude(source.Path, settings.ClaudeMDExcludes) {
			continue
		}
		filtered = append(filtered, source)
	}
	if settings.AutoMemoryEnabled == nil || *settings.AutoMemoryEnabled {
		memoryPath := ""
		if settings.AutoMemoryDirectory != "" {
			memoryPath = filepath.Join(setup.ExpandHome(settings.AutoMemoryDirectory), "MEMORY.md")
		} else {
			memoryPath = setup.ClaudeAutoMemoryPath(cwd, nil)
		}
		// An override can point into a folder macOS protects even when cwd does not.
		if !filemodel.Protected(memoryPath) {
			if info, err := os.Stat(memoryPath); err == nil && info.Mode().IsRegular() {
				filtered = append(filtered, setup.ClaudeContextSource{
					Path: memoryPath, Kind: "auto-memory", Scope: "repository", Load: "startup-truncated",
					Evidence: setup.InstructionEvidenceDocumented, Detail: "MEMORY.md startup entrypoint; Claude loads the first 200 lines or 25KB.",
					Exists: true,
				})
			}
		}
	}

	imports, diagnostics := resolveClaudeImports(filtered, settings.ClaudeMDExcludes)
	filtered = append(filtered, imports...)
	return dedupeClaudeSources(filtered), diagnostics
}

var claudeImportRE = regexp.MustCompile(`@([~./A-Za-z0-9_-][^[:space:]` + "`" + `"'<>]*)`)

func resolveClaudeImports(sources []setup.ClaudeContextSource, excludes []string) ([]setup.ClaudeContextSource, []string) {
	seen := map[string]bool{}
	for _, source := range sources {
		seen[source.Path] = true
	}
	var imports []setup.ClaudeContextSource
	var diagnostics []string
	var walk func(setup.ClaudeContextSource, int)
	walk = func(parent setup.ClaudeContextSource, depth int) {
		if depth >= 5 || parent.Kind == "auto-memory" || parent.Load == "conditional" {
			return
		}
		content, err := os.ReadFile(parent.Path)
		if err != nil {
			return
		}
		for _, match := range claudeImportRE.FindAllStringSubmatch(string(content), -1) {
			raw := strings.TrimRight(match[1], ".,;:!?)]}")
			if raw == "" || (!strings.Contains(raw, "/") && filepath.Ext(raw) == "") {
				continue
			}
			path := setup.ExpandHome(raw)
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(parent.Path), path)
			}
			path = filepath.Clean(path)
			if seen[path] || matchesClaudeExclude(path, excludes) || filemodel.Protected(path) {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			seen[path] = true
			source := setup.ClaudeContextSource{
				Path: path, Kind: "claude-import", Scope: parent.Scope, Load: parent.Load,
				Evidence: setup.InstructionEvidenceDocumented,
				Detail:   "Imported by " + parent.Path + "; external imports also depend on Claude's approval record.",
				Exists:   true,
			}
			imports = append(imports, source)
			walk(source, depth+1)
		}
	}
	for _, source := range sources {
		walk(source, 0)
	}
	return imports, diagnostics
}

func dedupeClaudeSources(sources []setup.ClaudeContextSource) []setup.ClaudeContextSource {
	seen := map[string]bool{}
	result := make([]setup.ClaudeContextSource, 0, len(sources))
	for _, source := range sources {
		if source.Path == "" || seen[source.Path] {
			continue
		}
		seen[source.Path] = true
		result = append(result, source)
	}
	return result
}

func matchesClaudeExclude(path string, patterns []string) bool {
	slashPath := filepath.ToSlash(path)
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(setup.ExpandHome(strings.TrimSpace(pattern)))
		if pattern == "" {
			continue
		}
		expression := regexp.QuoteMeta(pattern)
		expression = strings.ReplaceAll(expression, `\*\*`, `.*`)
		expression = strings.ReplaceAll(expression, `\*`, `[^/]*`)
		expression = strings.ReplaceAll(expression, `\?`, `[^/]`)
		if matched, _ := regexp.MatchString("^"+expression+"$", slashPath); matched {
			return true
		}
	}
	return false
}

func resolveClaudeSkills(cwd string) ([]resolvedSkill, []string) {
	type root struct {
		path  string
		scope string
	}
	var roots []root
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			root{filepath.Join(home, ".claude", "skills"), "user"},
			root{filepath.Join(home, ".claude", "commands"), "user-command"},
		)
	}
	for _, dir := range setup.AncestorDirs(cwd) {
		roots = append(roots,
			root{filepath.Join(dir, ".claude", "skills"), "project"},
			root{filepath.Join(dir, ".claude", "commands"), "project-command"},
		)
	}

	var skills []resolvedSkill
	var diagnostics []string
	seen := map[string]bool{}
	for _, candidate := range roots {
		if filemodel.Protected(candidate.path) {
			continue
		}
		err := filepath.WalkDir(candidate.path, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			isSkill := entry.Name() == "SKILL.md"
			isCommand := strings.Contains(candidate.scope, "command") && strings.EqualFold(filepath.Ext(path), ".md")
			// The walk never follows a link, but opening an entry would.
			if (!isSkill && !isCommand) || seen[path] || filemodel.Protected(path) {
				return nil
			}
			seen[path] = true
			name, description := readClaudeExtensionMetadata(path)
			if name == "" {
				if isSkill {
					name = filepath.Base(filepath.Dir(path))
				} else {
					relative, relErr := filepath.Rel(candidate.path, path)
					if relErr == nil {
						name = strings.TrimSuffix(filepath.ToSlash(relative), filepath.Ext(relative))
					}
				}
			}
			skills = append(skills, resolvedSkill{
				Name: name, Description: description, Path: path, Scope: candidate.scope,
			})
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			diagnostics = append(diagnostics, fmt.Sprintf("Discover Claude extensions under %s: %v", candidate.path, err))
		}
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Name == skills[j].Name {
			return skills[i].Path < skills[j].Path
		}
		return skills[i].Name < skills[j].Name
	})
	return skills, diagnostics
}

func readClaudeExtensionMetadata(path string) (string, string) {
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	name, description := "", ""
	inFrontmatter := false
	for line := 0; line < 80 && scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if line == 0 && text == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter && text == "---" {
			break
		}
		if !inFrontmatter {
			continue
		}
		key, value, ok := strings.Cut(text, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	return name, description
}

func resolveClaudeMCPNames(cwd string, sources []setup.ClaudeContextSource) ([]string, []string) {
	names := map[string]bool{}
	var diagnostics []string
	for _, source := range sources {
		if source.Kind != "mcp" {
			continue
		}
		content, err := os.ReadFile(source.Path)
		if err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("Read Claude MCP source %s: %v", source.Path, err))
			continue
		}
		var document struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			} `json:"projects"`
		}
		if err := json.Unmarshal(content, &document); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("Parse Claude MCP source %s: %v", source.Path, err))
			continue
		}
		for name := range document.MCPServers {
			names[name] = true
		}
		for _, projectPath := range uniqueNonempty([]string{cwd, setup.RepoRoot(cwd)}) {
			if project, ok := document.Projects[projectPath]; ok {
				for name := range project.MCPServers {
					names[name] = true
				}
			}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, diagnostics
}

var claudeVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+].*)?$`)

func inferClaudeBinaryVersion(bin string) string {
	if strings.TrimSpace(bin) == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	base := filepath.Base(resolved)
	if claudeVersionRE.MatchString(base) {
		return base
	}
	return ""
}

func (r *ClaudeResolver) loadPersisted(ctx context.Context, cwd string) (claudeWorkspaceSnapshot, bool, error) {
	row, ok, err := r.store.LoadClaudeWorkspaceResolution(ctx, cwd, historicalClaudeResolverVersion)
	if err != nil || !ok {
		return claudeWorkspaceSnapshot{}, ok, err
	}
	var payload persistedClaudeWorkspaceSnapshot
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return claudeWorkspaceSnapshot{}, false, err
	}
	return claudeWorkspaceSnapshot{
		ResolvedAt: row.ResolvedAt, TargetCWD: row.TargetCWD, Settings: payload.Settings,
		Instructions: payload.Instructions, ConfigSources: payload.ConfigSources, Skills: payload.Skills,
		MCPServers: payload.MCPServers, Plugins: payload.Plugins, CLIVersion: payload.CLIVersion,
		Git: payload.Git, Diagnostics: payload.Diagnostics,
	}, true, nil
}

func (r *ClaudeResolver) savePersisted(ctx context.Context, snapshot claudeWorkspaceSnapshot) error {
	payload, err := json.Marshal(persistedClaudeWorkspaceSnapshot{
		Settings: snapshot.Settings, Instructions: snapshot.Instructions,
		ConfigSources: snapshot.ConfigSources, Skills: snapshot.Skills,
		MCPServers: snapshot.MCPServers, Plugins: snapshot.Plugins, CLIVersion: snapshot.CLIVersion,
		Git: snapshot.Git, Diagnostics: snapshot.Diagnostics,
	})
	if err != nil {
		return err
	}
	return r.store.SaveClaudeWorkspaceResolution(ctx, history.ClaudeWorkspaceResolution{
		TargetCWD: snapshot.TargetCWD, ResolverVersion: historicalClaudeResolverVersion,
		ResolvedAt: snapshot.ResolvedAt, Payload: payload,
	})
}

// WarmCatalog primes workspace snapshots for saved Claude threads.
func (r *ClaudeResolver) WarmCatalog(ctx context.Context, catalog *threadindex.Service) {
	if r == nil || catalog == nil {
		return
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := catalog.ListThreads(ctx, threadindex.ListQuery{
			IncludeSubagents: true, IncludeProgrammatic: true, Archived: string(history.ArchivedInclude),
			Limit: threadindex.MaxThreadCatalogLimit, Cursor: cursor,
		})
		if err != nil {
			return
		}
		for _, thread := range page.Items {
			if thread.Provider != "claude" {
				continue
			}
			cwd := knownCatalogValue(thread.Metadata.CWD)
			if cwd == "" {
				continue
			}
			cwd = filepath.Clean(cwd)
			if seen[cwd] {
				continue
			}
			seen[cwd] = true
			resolveCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, _, _ = r.workspace(resolveCtx, cwd)
			cancel()
		}
		if page.NextCursor == nil {
			return
		}
		cursor = *page.NextCursor
	}
}
