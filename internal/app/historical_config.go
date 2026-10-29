package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

const (
	historicalResolverVersion = "codex-0.144+nano-2"
	historicalResolutionTTL   = 15 * time.Minute
	defaultProjectDocMaxBytes = 32 << 10
)

type resolvedInstructionSource struct {
	Path      string `json:"path"`
	Scope     string `json:"scope"`
	BytesRead int64  `json:"bytesRead"`
	Truncated bool   `json:"truncated,omitempty"`
}

type resolvedSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Path        string `json:"path,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

type resolvedModel struct {
	ID             string   `json:"id"`
	Model          string   `json:"model"`
	DefaultEffort  string   `json:"defaultEffort,omitempty"`
	ServiceTierIDs []string `json:"serviceTierIds"`
	Default        bool     `json:"default,omitempty"`
}

type resolvedGit = inspection.Git

type codexWorkspaceSnapshot struct {
	ResolvedAt         time.Time
	TargetCWD          string
	Fingerprint        string
	WatchPaths         []string
	Config             map[string]json.RawMessage
	Instructions       []resolvedInstructionSource
	Skills             []resolvedSkill
	MCPServers         []string
	PermissionProfiles []string
	Models             []resolvedModel
	Git                resolvedGit
	Diagnostics        []string
}

type persistedWorkspaceSnapshot struct {
	Fingerprint        string                      `json:"fingerprint"`
	WatchPaths         []string                    `json:"watchPaths"`
	Config             map[string]json.RawMessage  `json:"config"`
	Instructions       []resolvedInstructionSource `json:"instructions"`
	Skills             []resolvedSkill             `json:"skills"`
	MCPServers         []string                    `json:"mcpServers"`
	PermissionProfiles []string                    `json:"permissionProfiles"`
	Models             []resolvedModel             `json:"models"`
	Git                resolvedGit                 `json:"git"`
	Diagnostics        []string                    `json:"diagnostics"`
}

type codexPassiveCaller interface {
	Call(context.Context, string, any, any) error
}

type historicalConfigResolver struct {
	codex     codexPassiveCaller
	store     *history.Store
	codexHome string
	ttl       time.Duration
	now       func() time.Time

	mu             sync.Mutex
	memory         map[string]codexWorkspaceSnapshot
	inflight       map[string]chan struct{}
	refreshErrors  map[string]string
	targets        map[string]codexResolutionTarget
	targetInflight map[string]bool
}

type codexResolutionTarget struct {
	CWD               string
	ModelProvider     string
	ApprovalsReviewer string
	Diagnostics       []string
}

type codexConfigLayer struct {
	Name struct {
		Type string `json:"type"`
		File string `json:"file"`
	} `json:"name"`
	Config         map[string]json.RawMessage `json:"config"`
	DisabledReason *string                    `json:"disabledReason"`
}

func newHistoricalConfigResolver(codex codexPassiveCaller, store *history.Store, codexHome string) *historicalConfigResolver {
	if codex == nil || store == nil {
		return nil
	}
	return &historicalConfigResolver{
		codex: codex, store: store, codexHome: codexHome,
		ttl: historicalResolutionTTL, now: time.Now,
		memory: map[string]codexWorkspaceSnapshot{}, inflight: map[string]chan struct{}{},
		refreshErrors: map[string]string{}, targets: map[string]codexResolutionTarget{},
		targetInflight: map[string]bool{},
	}
}

func (r *historicalConfigResolver) Resolve(ctx context.Context, thread sidebar.Thread) inspection.CurrentResolution {
	target := readCodexResolutionTarget(thread)
	cwd := target.CWD
	if cwd == "" {
		return unavailableResolution("", "The rollout does not record a session working directory.")
	}
	cwd = filepath.Clean(cwd)
	info, err := os.Stat(cwd)
	if err != nil {
		return unavailableResolution(cwd, fmt.Sprintf("The recorded working directory is unavailable: %v", err))
	}
	if !info.IsDir() {
		return unavailableResolution(cwd, "The recorded working directory is not a directory.")
	}

	snapshot, stale, err := r.workspace(ctx, cwd)
	if err != nil {
		return inspection.CurrentResolution{
			State: "failed", ResolverVersion: historicalResolverVersion, TargetCWD: cwd,
			Values: map[string]inspection.ResolvedValue{}, Diagnostics: []string{err.Error()},
		}
	}
	resolution := overlayThreadResolution(thread, snapshot, stale, target.ModelProvider, target.ApprovalsReviewer)
	resolution.Diagnostics = append(resolution.Diagnostics, target.Diagnostics...)
	return resolution
}

func unavailableResolution(cwd, diagnostic string) inspection.CurrentResolution {
	return inspection.CurrentResolution{
		State: "unavailable", ResolverVersion: historicalResolverVersion, TargetCWD: cwd,
		Values: map[string]inspection.ResolvedValue{}, Diagnostics: []string{diagnostic},
	}
}

func (r *historicalConfigResolver) workspace(ctx context.Context, cwd string) (codexWorkspaceSnapshot, bool, error) {
	for {
		r.mu.Lock()
		snapshot, cached := r.memory[cwd]
		r.mu.Unlock()
		if cached && !r.workspaceSnapshotStale(snapshot) {
			return snapshot, false, nil
		}

		r.mu.Lock()
		if wait, ok := r.inflight[cwd]; ok {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return codexWorkspaceSnapshot{}, false, ctx.Err()
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

func (r *historicalConfigResolver) resolveOrLoad(ctx context.Context, cwd string) (codexWorkspaceSnapshot, bool, error) {
	cached, cachedOK, cacheErr := r.loadPersisted(ctx, cwd)
	if cacheErr != nil {
		cachedOK = false
	}
	if cachedOK && !r.workspaceSnapshotStale(cached) {
		return cached, false, nil
	}

	fresh, err := r.resolveWorkspace(ctx, cwd)
	if err != nil {
		if cachedOK {
			cached.Diagnostics = append(cached.Diagnostics, "Refresh failed; showing the last successful resolution: "+err.Error())
			return cached, true, nil
		}
		return codexWorkspaceSnapshot{}, false, err
	}
	if err := r.savePersisted(ctx, fresh); err != nil {
		fresh.Diagnostics = append(fresh.Diagnostics, "Resolved setup could not be cached: "+err.Error())
	}
	return fresh, false, nil
}

func (r *historicalConfigResolver) resolveWorkspace(ctx context.Context, cwd string) (codexWorkspaceSnapshot, error) {
	var configResponse struct {
		Config map[string]json.RawMessage `json:"config"`
		Layers []codexConfigLayer         `json:"layers"`
	}
	if err := r.codex.Call(ctx, "config/read", map[string]any{
		"cwd": cwd, "includeLayers": true,
	}, &configResponse); err != nil {
		return codexWorkspaceSnapshot{}, fmt.Errorf("resolve current Codex configuration: %w", err)
	}
	if configResponse.Config == nil {
		configResponse.Config = map[string]json.RawMessage{}
	}

	snapshot := codexWorkspaceSnapshot{
		ResolvedAt: r.now().UTC(), TargetCWD: cwd, Config: allowlistedConfig(configResponse.Config),
		Skills: []resolvedSkill{}, MCPServers: configuredMCPNames(configResponse.Config),
		PermissionProfiles: []string{}, Models: []resolvedModel{}, Diagnostics: []string{},
	}

	projectRootMarkers := []string{".git"}
	if markers, ok := nonProjectRootMarkers(configResponse.Layers); ok {
		projectRootMarkers = markers
	}
	instructions, instructionDiagnostics := resolveCodexInstructions(
		r.codexHome, cwd, projectRootMarkers,
		stringSliceConfig(configResponse.Config, "project_doc_fallback_filenames", nil),
		intConfig(configResponse.Config, "project_doc_max_bytes", defaultProjectDocMaxBytes),
	)
	snapshot.Instructions = instructions
	snapshot.Diagnostics = append(snapshot.Diagnostics, instructionDiagnostics...)

	var skillsResponse struct {
		Data []struct {
			Skills []struct {
				Name             string `json:"name"`
				Description      string `json:"description"`
				ShortDescription string `json:"shortDescription"`
				Path             string `json:"path"`
				Scope            string `json:"scope"`
				Enabled          bool   `json:"enabled"`
			} `json:"skills"`
			Errors []json.RawMessage `json:"errors"`
		} `json:"data"`
	}
	if err := r.codex.Call(ctx, "skills/list", map[string]any{
		"cwds": []string{cwd},
	}, &skillsResponse); err != nil {
		snapshot.Diagnostics = append(snapshot.Diagnostics, "Current skills are unavailable: "+err.Error())
	} else if len(skillsResponse.Data) > 0 {
		for _, skill := range skillsResponse.Data[0].Skills {
			if !skill.Enabled {
				continue
			}
			description := skill.ShortDescription
			if description == "" {
				description = skill.Description
			}
			snapshot.Skills = append(snapshot.Skills, resolvedSkill{
				Name: skill.Name, Description: description, Path: skill.Path, Scope: skill.Scope,
			})
		}
		if len(skillsResponse.Data[0].Errors) > 0 {
			snapshot.Diagnostics = append(snapshot.Diagnostics,
				fmt.Sprintf("Codex reported %d skill discovery errors.", len(skillsResponse.Data[0].Errors)))
		}
	}

	var profilesResponse struct {
		Data []struct {
			ID      string `json:"id"`
			Allowed bool   `json:"allowed"`
		} `json:"data"`
	}
	if err := r.codex.Call(ctx, "permissionProfile/list", map[string]any{"cwd": cwd}, &profilesResponse); err != nil {
		snapshot.Diagnostics = append(snapshot.Diagnostics, "Permission profiles are unavailable: "+err.Error())
	} else {
		for _, profile := range profilesResponse.Data {
			if profile.Allowed {
				snapshot.PermissionProfiles = append(snapshot.PermissionProfiles, profile.ID)
			}
		}
		sort.Strings(snapshot.PermissionProfiles)
	}

	var modelsResponse struct {
		Data []struct {
			ID                     string `json:"id"`
			Model                  string `json:"model"`
			DefaultReasoningEffort string `json:"defaultReasoningEffort"`
			IsDefault              bool   `json:"isDefault"`
			ServiceTiers           []struct {
				ID string `json:"id"`
			} `json:"serviceTiers"`
		} `json:"data"`
	}
	if err := r.codex.Call(ctx, "model/list", map[string]any{"limit": 100, "includeHidden": true}, &modelsResponse); err != nil {
		snapshot.Diagnostics = append(snapshot.Diagnostics, "Model defaults are unavailable: "+err.Error())
	} else {
		for _, model := range modelsResponse.Data {
			entry := resolvedModel{
				ID: model.ID, Model: model.Model, DefaultEffort: model.DefaultReasoningEffort,
				ServiceTierIDs: []string{}, Default: model.IsDefault,
			}
			for _, tier := range model.ServiceTiers {
				entry.ServiceTierIDs = append(entry.ServiceTierIDs, tier.ID)
			}
			snapshot.Models = append(snapshot.Models, entry)
		}
	}

	snapshot.Git = observeGit(ctx, cwd)
	snapshot.WatchPaths = codexWorkspaceWatchPaths(r.codexHome, cwd, configResponse.Layers, snapshot)
	snapshot.Fingerprint = fingerprintPaths(snapshot.WatchPaths)
	return snapshot, nil
}

// workspaceSnapshotStale checks only local filesystem metadata. It keeps the
// detail endpoint non-blocking while detecting configuration, instruction, and
// skill changes before the TTL backstop expires.
func (r *historicalConfigResolver) workspaceSnapshotStale(snapshot codexWorkspaceSnapshot) bool {
	if snapshot.Fingerprint == "" || len(snapshot.WatchPaths) == 0 {
		return true // cached rows from before fingerprints are safely refreshed.
	}
	return r.now().Sub(snapshot.ResolvedAt) >= r.ttl || fingerprintPaths(snapshot.WatchPaths) != snapshot.Fingerprint
}

func codexWorkspaceWatchPaths(codexHome, cwd string, layers []codexConfigLayer, snapshot codexWorkspaceSnapshot) []string {
	paths := []string{
		codexHome,
		filepath.Join(codexHome, "config.toml"),
		filepath.Join(codexHome, "skills"),
		cwd,
	}
	for _, layer := range layers {
		if layer.Name.File != "" {
			paths = append(paths, layer.Name.File)
		}
	}
	for _, instruction := range snapshot.Instructions {
		paths = append(paths, instruction.Path)
	}
	for _, skill := range snapshot.Skills {
		if skill.Path != "" {
			paths = append(paths, skill.Path, filepath.Dir(skill.Path))
		}
	}
	return uniqueNonempty(paths)
}

func fingerprintPaths(paths []string) string {
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	hash := sha256.New()
	for _, path := range ordered {
		_, _ = hash.Write([]byte(path))
		_, _ = hash.Write([]byte{0})
		info, err := os.Stat(path)
		if err != nil {
			_, _ = hash.Write([]byte("missing:" + err.Error()))
			_, _ = hash.Write([]byte{0})
			continue
		}
		_, _ = hash.Write([]byte(fmt.Sprintf("%t:%d:%d", info.IsDir(), info.Size(), info.ModTime().UnixNano())))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func nonProjectRootMarkers(layers []codexConfigLayer) ([]string, bool) {
	for _, layer := range layers {
		if layer.DisabledReason != nil || layer.Name.Type == "project" {
			continue
		}
		raw, ok := layer.Config["project_root_markers"]
		if !ok {
			continue
		}
		var markers []string
		if json.Unmarshal(raw, &markers) == nil {
			return markers, true
		}
	}
	return nil, false
}

func allowlistedConfig(config map[string]json.RawMessage) map[string]json.RawMessage {
	keys := []string{
		"model", "model_provider", "model_reasoning_effort", "service_tier",
		"approval_policy", "approvals_reviewer", "sandbox_mode", "default_permissions",
		"features", "multi_agent_mode",
	}
	allowed := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		if value, ok := config[key]; ok && string(value) != "null" {
			allowed[key] = append(json.RawMessage(nil), value...)
		}
	}
	return allowed
}

func configuredMCPNames(config map[string]json.RawMessage) []string {
	raw := config["mcp_servers"]
	var servers map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &servers) != nil {
		return []string{}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveCodexInstructions(codexHome, cwd string, markers, fallbacks []string, maxBytes int) ([]resolvedInstructionSource, []string) {
	if maxBytes <= 0 {
		return []resolvedInstructionSource{}, nil
	}
	var sources []resolvedInstructionSource
	var diagnostics []string

	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		path := filepath.Join(codexHome, name)
		if source, ok, err := inspectResolvedInstruction(path, "global", -1); err != nil {
			diagnostics = append(diagnostics, err.Error())
		} else if ok {
			sources = append(sources, source)
			break
		}
	}

	root := nearestProjectRoot(cwd, markers)
	directories := pathFromRoot(root, cwd)
	candidates := append([]string{"AGENTS.override.md", "AGENTS.md"}, fallbacks...)
	candidates = uniqueNonempty(candidates)
	remaining := int64(maxBytes)
	for _, directory := range directories {
		for _, name := range candidates {
			path := filepath.Join(directory, name)
			source, ok, err := inspectResolvedInstruction(path, "project", remaining)
			if err != nil {
				diagnostics = append(diagnostics, err.Error())
				break
			}
			if !ok {
				continue
			}
			if source.BytesRead > 0 {
				sources = append(sources, source)
				remaining -= source.BytesRead
			}
			break
		}
		if remaining <= 0 {
			break
		}
	}
	return sources, diagnostics
}

func inspectResolvedInstruction(path, scope string, remaining int64) (resolvedInstructionSource, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return resolvedInstructionSource{}, false, nil
	}
	if err != nil {
		return resolvedInstructionSource{}, false, fmt.Errorf("Inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return resolvedInstructionSource{}, false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return resolvedInstructionSource{}, false, fmt.Errorf("Read %s: %w", path, err)
	}
	if strings.TrimSpace(string(content)) == "" {
		return resolvedInstructionSource{}, false, nil
	}
	bytesRead := int64(len(content))
	truncated := false
	if remaining >= 0 && bytesRead > remaining {
		bytesRead = remaining
		truncated = true
	}
	return resolvedInstructionSource{
		Path: path, Scope: scope, BytesRead: bytesRead, Truncated: truncated,
	}, true, nil
}

func nearestProjectRoot(cwd string, markers []string) string {
	if len(markers) == 0 {
		return cwd
	}
	for cursor := cwd; ; cursor = filepath.Dir(cursor) {
		for _, marker := range markers {
			if marker == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(cursor, marker)); err == nil {
				return cursor
			}
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
	}
	return cwd
}

func pathFromRoot(root, cwd string) []string {
	var reversed []string
	for cursor := cwd; ; cursor = filepath.Dir(cursor) {
		reversed = append(reversed, cursor)
		if cursor == root {
			break
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return []string{cwd}
		}
	}
	directories := make([]string, len(reversed))
	for index := range reversed {
		directories[len(reversed)-1-index] = reversed[index]
	}
	return directories
}

func uniqueNonempty(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func stringSliceConfig(config map[string]json.RawMessage, key string, fallback []string) []string {
	raw, ok := config[key]
	if !ok {
		return append([]string(nil), fallback...)
	}
	var value []string
	if json.Unmarshal(raw, &value) != nil {
		return append([]string(nil), fallback...)
	}
	return value
}

func intConfig(config map[string]json.RawMessage, key string, fallback int) int {
	raw, ok := config[key]
	if !ok {
		return fallback
	}
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return fallback
	}
	return value
}

func observeGit(ctx context.Context, cwd string) resolvedGit { return inspection.ObserveGit(ctx, cwd) }

func overlayThreadResolution(
	thread sidebar.Thread,
	snapshot codexWorkspaceSnapshot,
	stale bool,
	recordedProvider,
	recordedReviewer string,
) inspection.CurrentResolution {
	values := map[string]inspection.ResolvedValue{
		"cwd": knownResolved(snapshot.TargetCWD, "thread_session_meta"),
		// config/read does not expose the app-server's runtime workspace-root
		// materialization. Do not turn that absence into a false "none" claim.
		"workspaceRoots":     inspection.ResolvedValue{State: "unknown", Source: "offline_resolver_limit"},
		"instructionSources": knownResolved(snapshot.Instructions, "resolved_now"),
		"skills":             knownResolved(snapshot.Skills, "resolved_now"),
		"mcpServers":         knownResolved(snapshot.MCPServers, "current_config"),
		"permissionProfiles": knownResolved(snapshot.PermissionProfiles, "current_config"),
	}
	configFields := map[string]string{
		"modelProvider": "model_provider", "serviceTier": "service_tier",
		"approvalPolicy": "approval_policy", "approvalsReviewer": "approvals_reviewer",
		"sandbox": "sandbox_mode", "permissionProfile": "default_permissions",
		"multiAgentMode": "multi_agent_mode",
	}
	for output, input := range configFields {
		values[output] = rawResolved(snapshot.Config[input], "current_config")
	}
	if values["modelProvider"].State != "known" && recordedProvider != "" {
		values["modelProvider"] = knownResolved(recordedProvider, "thread_session_meta")
	}
	if recordedReviewer != "" {
		values["approvalsReviewer"] = knownResolved(recordedReviewer, "thread_persisted")
	}
	if values["multiAgentMode"].State != "known" {
		values["multiAgentMode"] = knownResolved("explicitRequestOnly", "codex-0.144-default")
	}
	values["model"] = preferRecorded(thread.Metadata.Model, snapshot.Config["model"])
	model := resolvedModelFor(values["model"], snapshot.Models)
	if values["model"].State != "known" && model.Model != "" {
		values["model"] = knownResolved(model.Model, "codex_model_default")
	}
	values["reasoningEffort"] = preferRecorded(thread.Metadata.Effort, snapshot.Config["model_reasoning_effort"])
	if values["reasoningEffort"].State != "known" && model.DefaultEffort != "" {
		values["reasoningEffort"] = knownResolved(model.DefaultEffort, "codex_model_default")
	}
	values["serviceTier"] = resolvedServiceTier(snapshot.Config, model)
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
		State: state, ResolverVersion: historicalResolverVersion,
		ResolvedAt: snapshot.ResolvedAt.Format(time.RFC3339Nano), TargetCWD: snapshot.TargetCWD,
		Stale: stale, Values: values, Diagnostics: append([]string{}, snapshot.Diagnostics...),
	}
}

func readCodexResolutionTarget(thread sidebar.Thread) codexResolutionTarget {
	fallback := knownCatalogValue(thread.Metadata.CWD)
	if thread.Sources == nil {
		return codexResolutionTarget{CWD: fallback}
	}
	for _, source := range *thread.Sources {
		if source.Path == "" {
			continue
		}
		file, err := os.Open(source.Path)
		if err != nil {
			continue
		}
		decoder := json.NewDecoder(bufio.NewReader(file))
		for count := 0; count < 8; count++ {
			var record struct {
				Type    string `json:"type"`
				Payload struct {
					CWD           string `json:"cwd"`
					ModelProvider string `json:"model_provider"`
				} `json:"payload"`
			}
			if err := decoder.Decode(&record); err != nil {
				break
			}
			if record.Type == "session_meta" {
				_ = file.Close()
				return codexResolutionTarget{
					CWD: strings.TrimSpace(record.Payload.CWD), ModelProvider: record.Payload.ModelProvider,
					ApprovalsReviewer: readLatestCodexReviewer(source.Path),
				}
			}
		}
		_ = file.Close()
	}
	target := codexResolutionTarget{CWD: fallback}
	if fallback != "" {
		target.Diagnostics = []string{"The session metadata could not be reread; using the cataloged working directory."}
	}
	return target
}

func readLatestCodexReviewer(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	const tailBytes int64 = 8 << 20
	start := info.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	data := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(data, start); err != nil && len(data) == 0 {
		return ""
	}
	if start > 0 {
		if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	lines := strings.Split(string(data), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type              string `json:"type"`
				ApprovalsReviewer string `json:"approvals_reviewer"`
				ThreadSettings    struct {
					ApprovalsReviewer string `json:"approvals_reviewer"`
				} `json:"thread_settings"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(lines[index]), &record) != nil {
			continue
		}
		if record.Type == "turn_context" && record.Payload.ApprovalsReviewer != "" {
			return record.Payload.ApprovalsReviewer
		}
		if record.Type == "event_msg" && record.Payload.Type == "thread_settings_applied" &&
			record.Payload.ThreadSettings.ApprovalsReviewer != "" {
			return record.Payload.ThreadSettings.ApprovalsReviewer
		}
	}
	return ""
}

func knownCatalogValue(value threadindex.Value) string {
	return inspection.KnownMetadataValue(value.State, value.Value)
}

func preferRecorded(recorded threadindex.Value, configured json.RawMessage) inspection.ResolvedValue {
	if value := knownCatalogValue(recorded); value != "" {
		return knownResolved(value, "thread_persisted")
	}
	return rawResolved(configured, "current_config")
}

func knownResolved(value any, source string) inspection.ResolvedValue {
	return inspection.ResolvedValue{State: "known", Source: source, Value: value}
}

func rawResolved(raw json.RawMessage, source string) inspection.ResolvedValue {
	if len(raw) == 0 || string(raw) == "null" {
		return inspection.ResolvedValue{State: "unknown", Source: source}
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return inspection.ResolvedValue{State: "unknown", Source: source}
	}
	return knownResolved(value, source)
}

func (r *historicalConfigResolver) loadPersisted(ctx context.Context, cwd string) (codexWorkspaceSnapshot, bool, error) {
	row, ok, err := r.store.LoadCodexWorkspaceResolution(ctx, cwd, historicalResolverVersion)
	if err != nil || !ok {
		return codexWorkspaceSnapshot{}, ok, err
	}
	var payload persistedWorkspaceSnapshot
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return codexWorkspaceSnapshot{}, false, err
	}
	return codexWorkspaceSnapshot{
		ResolvedAt: row.ResolvedAt, TargetCWD: row.TargetCWD,
		Fingerprint: payload.Fingerprint, WatchPaths: payload.WatchPaths, Config: payload.Config,
		Instructions: payload.Instructions, Skills: payload.Skills, MCPServers: payload.MCPServers,
		PermissionProfiles: payload.PermissionProfiles, Models: payload.Models,
		Git: payload.Git, Diagnostics: payload.Diagnostics,
	}, true, nil
}

func (r *historicalConfigResolver) savePersisted(ctx context.Context, snapshot codexWorkspaceSnapshot) error {
	payload, err := json.Marshal(persistedWorkspaceSnapshot{
		Fingerprint: snapshot.Fingerprint, WatchPaths: snapshot.WatchPaths,
		Config: snapshot.Config, Instructions: snapshot.Instructions, Skills: snapshot.Skills,
		MCPServers: snapshot.MCPServers, PermissionProfiles: snapshot.PermissionProfiles,
		Models: snapshot.Models, Git: snapshot.Git, Diagnostics: snapshot.Diagnostics,
	})
	if err != nil {
		return err
	}
	return r.store.SaveCodexWorkspaceResolution(ctx, history.CodexWorkspaceResolution{
		TargetCWD: snapshot.TargetCWD, ResolverVersion: historicalResolverVersion,
		ResolvedAt: snapshot.ResolvedAt, Payload: payload,
	})
}

func resolvedModelFor(value inspection.ResolvedValue, models []resolvedModel) resolvedModel {
	if value.State == "known" {
		name, _ := value.Value.(string)
		for _, model := range models {
			if model.ID == name || model.Model == name {
				return model
			}
		}
	}
	for _, model := range models {
		if model.Default {
			return model
		}
	}
	return resolvedModel{}
}

func resolvedServiceTier(config map[string]json.RawMessage, model resolvedModel) inspection.ResolvedValue {
	tier := rawResolved(config["service_tier"], "current_config")
	if tier.State != "known" {
		return tier
	}
	name, _ := tier.Value.(string)
	if name == "" || name == "default" {
		return tier
	}
	if !configFeatureEnabled(config["features"], "fast_mode") {
		return inspection.ResolvedValue{State: "unsupported", Source: "codex-0.144-fast-mode-rule"}
	}
	for _, supported := range model.ServiceTierIDs {
		if supported == name {
			return tier
		}
	}
	return inspection.ResolvedValue{State: "unsupported", Source: "codex-0.144-model-capability"}
}

func configFeatureEnabled(raw json.RawMessage, name string) bool {
	var features map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &features) != nil {
		return false
	}
	var enabled bool
	return json.Unmarshal(features[name], &enabled) == nil && enabled
}

func (r *historicalConfigResolver) WarmCatalog(ctx context.Context, catalog threadCatalog) {
	if r == nil || catalog == nil {
		return
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := catalog.ListThreads(ctx, threadindex.ListQuery{
			IncludeSubagents: true, IncludeProgrammatic: true, Archived: archivedIncludeFilter,
			Limit: threadindex.MaxThreadCatalogLimit, Cursor: cursor,
		})
		if err != nil {
			return
		}
		for _, thread := range page.Items {
			if thread.Provider != "codex" {
				continue
			}
			target := codexResolutionTarget{CWD: knownCatalogValue(thread.Metadata.CWD)}
			if detail, err := catalog.LoadThread(ctx, thread.ID); err == nil {
				target = readCodexResolutionTarget(detail)
				if target.CWD == "" {
					target.CWD = knownCatalogValue(thread.Metadata.CWD)
				}
				r.mu.Lock()
				r.targets[threadResolutionTargetKey(thread)] = target
				r.mu.Unlock()
			}
			cwd := target.CWD
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
