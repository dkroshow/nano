package inspection

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/files"
)

const gitTTL = 30 * time.Second

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
	ttl     time.Duration
	now     func() time.Time
	resolve func(context.Context, string) string

	mu      sync.Mutex
	entries map[string]gitEntry
}

func NewGitCache() *GitCache {
	return &GitCache{
		ttl: gitTTL, now: time.Now, resolve: gitBranch,
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
	branch := cache.resolve(ctx, cwd)
	cache.mu.Lock()
	cache.entries[cwd] = gitEntry{branch: branch, resolvedAt: cache.now()}
	cache.mu.Unlock()
	return branch
}

// Git details are read from the repository's own files, not by running git:
// git follows config includes, object alternates and links wherever they
// lead, and any read inside a folder macOS protects asks the user for
// permission. Every file here is opened only after files.Physical shows where
// its path really leads. Whether the worktree has changes needs git status,
// so Dirty stays unknown.
func gitBranch(_ context.Context, cwd string) string {
	repo, ok := findRepository(cwd)
	if !ok {
		return ""
	}
	ref, _ := repo.head()
	return strings.TrimPrefix(ref, "refs/heads/")
}

func ObserveGit(_ context.Context, cwd string) Git {
	repo, ok := findRepository(cwd)
	if !ok {
		return Git{}
	}
	ref, commit := repo.head()
	if ref != "" {
		commit = repo.resolve(ref)
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return Git{Root: repo.root, Branch: strings.TrimPrefix(ref, "refs/heads/"), Commit: commit}
}

type repository struct {
	root      string // the worktree
	gitDir    string // HEAD lives here
	commonDir string // refs live here; a linked worktree's main repository
}

// findRepository finds the repository git would use for cwd: the nearest
// physical ancestor holding .git, a directory or a file naming the gitdir.
func findRepository(cwd string) (repository, bool) {
	dir, ok := files.Physical(cwd)
	if !ok || cwd == "" {
		return repository{}, false
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return openRepository(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return repository{}, false
		}
		dir = parent
	}
}

func openRepository(root string) (repository, bool) {
	gitDir, ok := files.Physical(filepath.Join(root, ".git"))
	if !ok {
		return repository{}, false
	}
	if info, err := os.Stat(gitDir); err != nil {
		return repository{}, false
	} else if info.Mode().IsRegular() {
		if gitDir, ok = linkedDir(gitDir, "gitdir: "); !ok {
			return repository{}, false
		}
	}
	commonDir := gitDir
	if _, err := os.Lstat(filepath.Join(gitDir, "commondir")); err == nil {
		if commonDir, ok = linkedDir(filepath.Join(gitDir, "commondir"), ""); !ok {
			return repository{}, false
		}
	}
	return repository{root: root, gitDir: gitDir, commonDir: commonDir}, true
}

// linkedDir follows a git metadata file that names a directory on its first
// line, relative to the file's own directory.
func linkedDir(path, prefix string) (string, bool) {
	line, ok := firstLine(path)
	if !ok || !strings.HasPrefix(line, prefix) || line == prefix {
		return "", false
	}
	target := strings.TrimPrefix(line, prefix)
	if !filepath.IsAbs(target) {
		target = filepath.Dir(path) + "/" + target // not cleaned: ".." applies after links
	}
	return files.Physical(target)
}

// head returns the ref HEAD names, or "" and the commit when it is detached.
func (repo repository) head() (string, string) {
	line, ok := firstLine(filepath.Join(repo.gitDir, "HEAD"))
	if !ok {
		return "", ""
	}
	if ref, symbolic := strings.CutPrefix(line, "ref: "); symbolic {
		return ref, ""
	}
	return "", line
}

func (repo repository) resolve(ref string) string {
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") {
		return ""
	}
	if commit, ok := firstLine(filepath.Join(repo.commonDir, filepath.FromSlash(ref))); ok {
		return commit
	}
	content, ok := readFile(filepath.Join(repo.commonDir, "packed-refs"))
	if !ok {
		return ""
	}
	for _, line := range strings.Split(content, "\n") {
		if commit, name, found := strings.Cut(strings.TrimSpace(line), " "); found && name == ref {
			return commit
		}
	}
	return ""
}

func firstLine(path string) (string, bool) {
	content, ok := readFile(path)
	if !ok {
		return "", false
	}
	line, _, _ := strings.Cut(content, "\n")
	line = strings.TrimSpace(line)
	return line, line != ""
}

// readFile reads a repository file unless reaching it would enter a folder
// macOS protects.
func readFile(path string) (string, bool) {
	physical, ok := files.Physical(path)
	if !ok {
		return "", false
	}
	content, err := os.ReadFile(physical)
	return string(content), err == nil
}
