package inspection

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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

// Catches: the file reader disagreeing with git about the root, branch and
// commit it reports. Git itself builds the fixtures and gives the expected
// answers: a plain repository from a subfolder, packed refs, a linked
// worktree and a detached HEAD. Boundary: ObserveGit, which panes and the
// setup panel read.
func TestObserveGitAgreesWithGit(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, worktree := filepath.Join(base, "repo"), filepath.Join(base, "worktree")
	gitRun(t, base, "init", "-q", "-b", "main", repo)
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	expect := func(name, cwd, root string) {
		t.Helper()
		want := Git{Root: root, Commit: gitRun(t, cwd, "rev-parse", "--short=12", "HEAD")}
		if branch, err := gitOutputErr(cwd, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
			want.Branch = branch
		}
		if got := ObserveGit(context.Background(), cwd); got != want {
			t.Errorf("%s: ObserveGit = %+v, git says %+v", name, got, want)
		}
	}
	expect("plain", filepath.Join(repo, "sub"), repo)
	gitRun(t, repo, "pack-refs", "--all")
	expect("packed refs", repo, repo)
	gitRun(t, repo, "worktree", "add", "-q", "-b", "feature", worktree)
	expect("linked worktree", worktree, worktree)
	gitRun(t, repo, "checkout", "-q", "--detach")
	expect("detached", repo, repo)
}

// Catches: git metadata reached from a safe folder that leads into a folder
// macOS protects being read: a .git link to a gitdir file naming it, a
// worktree whose commondir file is a link into it, a gitdir path with ".."
// after a link, and a repository found through the folder's physical rather
// than spelled ancestry, and a HEAD link through a missing folder and ".."
// (each seen in review). Boundary: ObserveGit.
func TestObserveGitNeverReadsAProtectedFolder(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy folders exist only on darwin")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	protected := filepath.Join(home, "Documents", "deep", "repo")
	gitRun(t, home, "init", "-q", "-b", "protected-branch", protected)
	gitRun(t, protected, "commit", "-q", "--allow-empty", "-m", "first")
	code := filepath.Join(home, "Code")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	// A .git link to a safe gitdir file that names the protected repository.
	write(filepath.Join(code, "gitfile"), "gitdir: "+filepath.Join(protected, ".git")+"\n")
	link(filepath.Join(code, "gitfile"), filepath.Join(code, "a", ".git"))
	// A worktree gitdir outside, whose commondir file links into the folder.
	write(filepath.Join(code, "wt-meta", "HEAD"), "ref: refs/heads/protected-branch\n")
	write(filepath.Join(home, "Documents", "commondir"), filepath.Join(protected, ".git")+"\n")
	link(filepath.Join(home, "Documents", "commondir"), filepath.Join(code, "wt-meta", "commondir"))
	write(filepath.Join(code, "b", ".git"), "gitdir: "+filepath.Join(code, "wt-meta")+"\n")
	// ".." after a link: jump/../repo/.git is Documents/deep/repo/.git.
	link(filepath.Join(home, "Documents", "deep", "x"), filepath.Join(code, "c", "jump"))
	write(filepath.Join(code, "c", ".git"), "gitdir: jump/../repo/.git\n")
	// A spelled path whose physical parent holds a .git link into the folder.
	link(filepath.Join(protected, ".git"), filepath.Join(code, "physical", ".git"))
	if err := os.MkdirAll(filepath.Join(code, "physical", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link(filepath.Join(code, "physical", "sub"), filepath.Join(code, "alias", "sub"))
	if err := os.MkdirAll(filepath.Join(home, "Documents", "deep", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// HEAD naming missing/../secret-head: the kernel fails on "missing", so
	// nothing may read the secret-head link beside it, which leads inside.
	write(filepath.Join(home, "Documents", "HEAD"), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	link(filepath.Join(home, "Documents", "HEAD"), filepath.Join(code, "d", ".git", "secret-head"))
	link("missing/../secret-head", filepath.Join(code, "d", ".git", "HEAD"))

	for _, cwd := range []string{"a", "b", "c", "d", filepath.Join("alias", "sub")} {
		dir := filepath.Join(code, cwd)
		if got := ObserveGit(context.Background(), dir); got.Branch != "" || got.Commit != "" {
			t.Errorf("%s: read repository metadata in a protected folder: %+v", cwd, got)
		}
		if got := gitBranch(context.Background(), dir); got != "" {
			t.Errorf("%s: read the branch %q in a protected folder", cwd, got)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutputErr(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func gitOutputErr(dir string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := command.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
