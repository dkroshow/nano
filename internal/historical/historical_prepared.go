package historical

import (
	"context"
	"fmt"
	"github.com/dkroshow/nano/internal/threadindex"
	"os"
	"path/filepath"
	"time"

	"github.com/dkroshow/nano/internal/inspection"
)

// ResolvePrepared keeps Codex thread detail a read path. It returns a
// persisted or memory snapshot immediately and refreshes missing/stale work in
// the background.
func (r *CodexResolver) ResolvePrepared(_ context.Context, thread threadindex.Thread) inspection.CurrentResolution {
	target, targetPending := r.preparedTarget(thread)
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

	snapshot, found, stale, refreshErr := r.preparedWorkspace(cwd)
	if !found {
		diagnostics := append([]string{}, target.Diagnostics...)
		if refreshErr != "" {
			diagnostics = append(diagnostics, "Current setup refresh failed: "+refreshErr)
		}
		return inspection.CurrentResolution{
			State: "resolving", ResolverVersion: historicalResolverVersion, TargetCWD: cwd,
			Values: map[string]inspection.ResolvedValue{}, Diagnostics: diagnostics,
		}
	}
	resolution := overlayThreadResolution(thread, snapshot, stale || targetPending, target.ModelProvider, target.ApprovalsReviewer)
	resolution.Diagnostics = append(resolution.Diagnostics, target.Diagnostics...)
	if targetPending {
		resolution.Diagnostics = append(resolution.Diagnostics, "Refreshing thread-specific Codex evidence in the background.")
	}
	if refreshErr != "" {
		resolution.Diagnostics = append(resolution.Diagnostics, "Current setup refresh failed: "+refreshErr)
	}
	return resolution
}

func (r *CodexResolver) preparedWorkspace(cwd string) (codexWorkspaceSnapshot, bool, bool, string) {
	r.mu.Lock()
	snapshot, found := r.memory[cwd]
	refreshErr := r.refreshErrors[cwd]
	r.mu.Unlock()
	if found {
		stale := r.workspaceSnapshotStale(snapshot)
		if stale {
			r.refreshWorkspaceAsync(cwd)
		}
		return snapshot, true, stale, refreshErr
	}

	snapshot, ok, err := r.loadPersisted(context.Background(), cwd)
	if err == nil && ok {
		stale := r.workspaceSnapshotStale(snapshot)
		r.mu.Lock()
		r.memory[cwd] = snapshot
		refreshErr := r.refreshErrors[cwd]
		r.mu.Unlock()
		if stale {
			r.refreshWorkspaceAsync(cwd)
		}
		return snapshot, true, stale, refreshErr
	}
	r.refreshWorkspaceAsync(cwd)
	r.mu.Lock()
	refreshErr = r.refreshErrors[cwd]
	r.mu.Unlock()
	return codexWorkspaceSnapshot{}, false, true, refreshErr
}

func (r *CodexResolver) refreshWorkspaceAsync(cwd string) {
	r.mu.Lock()
	if _, running := r.inflight[cwd]; running {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, _, err := r.workspace(ctx, cwd)
		cancel()
		r.mu.Lock()
		if err != nil {
			r.refreshErrors[cwd] = err.Error()
		} else {
			delete(r.refreshErrors, cwd)
		}
		r.mu.Unlock()
	}()
}

func (r *CodexResolver) preparedTarget(thread threadindex.Thread) (codexResolutionTarget, bool) {
	key := threadResolutionTargetKey(thread)
	r.mu.Lock()
	if target, ok := r.targets[key]; ok {
		r.mu.Unlock()
		return target, false
	}
	if r.targetInflight[key] || thread.Sources == nil || len(*thread.Sources) == 0 {
		pending := r.targetInflight[key]
		r.mu.Unlock()
		return codexResolutionTarget{CWD: knownCatalogValue(thread.Metadata.CWD)}, pending
	}
	r.targetInflight[key] = true
	r.mu.Unlock()

	fallback := codexResolutionTarget{CWD: knownCatalogValue(thread.Metadata.CWD)}
	go func() {
		target := readCodexResolutionTarget(thread)
		if target.CWD == "" {
			target.CWD = fallback.CWD
		}
		r.mu.Lock()
		r.targets[key] = target
		delete(r.targetInflight, key)
		r.mu.Unlock()
	}()
	return fallback, true
}

func threadResolutionTargetKey(thread threadindex.Thread) string {
	if thread.ID != "" {
		return thread.ID
	}
	return thread.Provider + ":" + thread.NativeID
}

// ResolvePrepared is Claude's filesystem-only equivalent.
func (r *ClaudeResolver) ResolvePrepared(_ context.Context, thread threadindex.Thread) inspection.CurrentResolution {
	target := readClaudeResolutionTarget(thread)
	if target.CWD == "" {
		return unavailableClaudeResolution("", "The Claude transcript does not record a working directory.")
	}
	cwd := filepath.Clean(target.CWD)
	info, err := os.Stat(cwd)
	if err != nil {
		return unavailableClaudeResolution(cwd, fmt.Sprintf("The recorded working directory is unavailable: %v", err))
	}
	if !info.IsDir() {
		return unavailableClaudeResolution(cwd, "The recorded working directory is not a directory.")
	}

	snapshot, found, stale, refreshErr := r.preparedWorkspace(cwd)
	if !found {
		diagnostics := append([]string{}, target.Diagnostics...)
		if refreshErr != "" {
			diagnostics = append(diagnostics, "Current setup refresh failed: "+refreshErr)
		}
		return inspection.CurrentResolution{
			State: "resolving", ResolverVersion: historicalClaudeResolverVersion, TargetCWD: cwd,
			Values: map[string]inspection.ResolvedValue{}, Diagnostics: diagnostics,
		}
	}
	resolution := overlayClaudeThreadResolution(thread, snapshot, stale, target)
	if refreshErr != "" {
		resolution.Diagnostics = append(resolution.Diagnostics, "Current setup refresh failed: "+refreshErr)
	}
	return resolution
}

func (r *ClaudeResolver) preparedWorkspace(cwd string) (claudeWorkspaceSnapshot, bool, bool, string) {
	r.mu.Lock()
	if snapshot, ok := r.memory[cwd]; ok {
		stale := r.now().Sub(snapshot.ResolvedAt) >= r.ttl
		refreshErr := r.refreshErrors[cwd]
		r.mu.Unlock()
		if stale {
			r.refreshWorkspaceAsync(cwd)
		}
		return snapshot, true, stale, refreshErr
	}
	r.mu.Unlock()

	snapshot, ok, err := r.loadPersisted(context.Background(), cwd)
	if err == nil && ok {
		r.mu.Lock()
		r.memory[cwd] = snapshot
		refreshErr := r.refreshErrors[cwd]
		r.mu.Unlock()
		stale := r.now().Sub(snapshot.ResolvedAt) >= r.ttl
		if stale {
			r.refreshWorkspaceAsync(cwd)
		}
		return snapshot, true, stale, refreshErr
	}
	r.refreshWorkspaceAsync(cwd)
	r.mu.Lock()
	refreshErr := r.refreshErrors[cwd]
	r.mu.Unlock()
	return claudeWorkspaceSnapshot{}, false, true, refreshErr
}

func (r *ClaudeResolver) refreshWorkspaceAsync(cwd string) {
	r.mu.Lock()
	if _, running := r.inflight[cwd]; running {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, _, err := r.workspace(ctx, cwd)
		cancel()
		r.mu.Lock()
		if err != nil {
			r.refreshErrors[cwd] = err.Error()
		} else {
			delete(r.refreshErrors, cwd)
		}
		r.mu.Unlock()
	}()
}
