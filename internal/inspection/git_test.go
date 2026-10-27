package inspection

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("NANO_TEST_GIT") == "1" {
		log, err := os.OpenFile(os.Getenv("GIT_CALLS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, writeErr := fmt.Fprintln(log, strings.Join(os.Args[1:], " "))
		closeErr := log.Close()
		if writeErr != nil || closeErr != nil {
			fmt.Fprintf(os.Stderr, "write call log: %v; close: %v\n", writeErr, closeErr)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "feature/current")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestGitCacheResolvesOncePerTTL(t *testing.T) {
	now := time.Unix(0, 0)
	calls := 0
	cache := NewGitCache()
	cache.now = func() time.Time { return now }
	cache.resolve = func(context.Context, string) string {
		calls++
		return "main"
	}
	if got := cache.Branch(context.Background(), "/repo"); got != "main" {
		t.Fatalf("branch = %q", got)
	}
	if got := cache.Branch(context.Background(), "/repo"); got != "main" || calls != 1 {
		t.Fatalf("cached branch = %q, calls = %d", got, calls)
	}
	now = now.Add(gitTTL + time.Second)
	if got := cache.Branch(context.Background(), "/repo"); got != "main" || calls != 2 {
		t.Fatalf("refreshed branch = %q, calls = %d", got, calls)
	}
}

func TestObserveGitReadsThisRepo(t *testing.T) {
	git := ObserveGit(context.Background(), "../..")
	if git.Root == "" {
		t.Skip("test is not in a git worktree")
	}
	if git.Commit == "" {
		t.Fatal("git worktree did not report a commit")
	}
}

// Branch-only reads must not launch root, commit, or dirty-status inspection:
// those extra subprocesses made a cold session-state read stall.
func TestGitCacheBranchRunsOnlyBranchCommand(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the running binary: a new executable script can launch cold past
	// the production deadline when other tests launch scripts concurrently.
	if err := os.Symlink(executable, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GIT_CALLS", log)
	t.Setenv("NANO_TEST_GIT", "1")
	if got := NewGitCache().Branch(context.Background(), "/repo"); got != "feature/current" {
		t.Fatalf("branch = %q", got)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(calls)); got != "-C /repo symbolic-ref --quiet --short HEAD" {
		t.Fatalf("branch read ran unrelated Git inspection: %q", got)
	}
}
