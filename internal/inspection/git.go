package inspection

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	gitTTL      = 30 * time.Second
	gitDeadline = 3 * time.Second
)

type Git struct {
	Root   string `json:"root,omitempty"`
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
	Dirty  *bool  `json:"dirty,omitempty"`
}

type gitEntry struct {
	branch     string
	resolvedAt time.Time
}

type GitCache struct {
	ttl      time.Duration
	deadline time.Duration
	now      func() time.Time
	resolve  func(context.Context, string) string

	mu      sync.Mutex
	entries map[string]gitEntry
}

func NewGitCache() *GitCache {
	return &GitCache{
		ttl: gitTTL, deadline: gitDeadline, now: time.Now, resolve: gitBranch,
		entries: make(map[string]gitEntry),
	}
}

func (cache *GitCache) Branch(ctx context.Context, cwd string) string {
	if cwd == "" {
		return ""
	}
	cache.mu.Lock()
	entry, ok := cache.entries[cwd]
	cache.mu.Unlock()
	if ok && cache.now().Sub(entry.resolvedAt) < cache.ttl {
		return entry.branch
	}
	bounded, cancel := context.WithTimeout(ctx, cache.deadline)
	defer cancel()
	branch := cache.resolve(bounded, cwd)
	cache.mu.Lock()
	cache.entries[cwd] = gitEntry{branch: branch, resolvedAt: cache.now()}
	cache.mu.Unlock()
	return branch
}

func gitBranch(ctx context.Context, cwd string) string {
	return gitOutput(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
}

func gitOutput(ctx context.Context, cwd string, args ...string) string {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func ObserveGit(ctx context.Context, cwd string) Git {
	root := gitOutput(ctx, cwd, "rev-parse", "--show-toplevel")
	if root == "" {
		return Git{}
	}
	branch := gitBranch(ctx, cwd)
	commit := gitOutput(ctx, cwd, "rev-parse", "--short=12", "HEAD")
	status, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain", "--untracked-files=no").Output()
	var dirty *bool
	if err == nil {
		value := len(status) > 0
		dirty = &value
	}
	return Git{Root: root, Branch: branch, Commit: commit, Dirty: dirty}
}
