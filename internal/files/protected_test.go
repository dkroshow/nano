package files

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Catches: a folder macOS guards with a privacy prompt missing from the list,
// a lookalike (~/Documents-old, /VolumesX) caught by a loose prefix, and a
// file link into one being opened, directly or through a symlink outside it:
// a plain link, a ".." after a link, and a chain longer than the walk follows
// (each seen in review).
// Boundary: Protected, which every reader asks first, and the link resolver.
func TestProtectedFoldersAreNeverOpened(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS privacy folders exist only on darwin")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for path, want := range map[string]bool{
		filepath.Join(home, "Documents"):                             true,
		filepath.Join(home, "documents", "Codex", "x"):               true,
		filepath.Join(home, "Desktop", "a"):                          true,
		filepath.Join(home, "Downloads", "a"):                        true,
		filepath.Join(home, "Library", "Mobile Documents", "a"):      true,
		filepath.Join(home, "Library", "CloudStorage", "Drive", "a"): true,
		filepath.Join(home, "Library", "Containers", "app"):          true,
		filepath.Join(home, "Library", "Group Containers", "g"):      true,
		"/Volumes/External/repo":                                     true,
		filepath.Join(home, "Documents-old", "a"):                    false,
		filepath.Join(home, "Code", "repo"):                          false,
		filepath.Join(home, "Library", "Application Support"):        false,
		"/VolumesX": false,
	} {
		if got := Protected(path); got != want {
			t.Errorf("Protected(%s) = %v, want %v", path, got, want)
		}
	}

	file := filepath.Join(home, "Documents", "notes.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "Documents", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(home, "Code")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		filepath.Join(home, "notes.md"): file,
		filepath.Join(home, "docs"):     filepath.Join(home, "Documents"),
		// ".." applies after jump is followed, so this lands in Documents.
		filepath.Join(code, "jump"):      filepath.Join(home, "Documents", "deep"),
		filepath.Join(code, "AGENTS.md"): "jump/../notes.md",
	}
	// A chain longer than the walk follows must be refused, not trusted.
	for i := 0; i < 40; i++ {
		target := file
		if i < 39 {
			target = filepath.Join(code, fmt.Sprintf("chain%d", i+1))
		}
		links[filepath.Join(code, fmt.Sprintf("chain%d", i))] = target
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		file, filepath.Join(home, "notes.md"), filepath.Join(home, "docs", "notes.md"),
		filepath.Join(code, "AGENTS.md"), filepath.Join(code, "chain0"),
	} {
		if got, _, err := Resolve(path, "", "", nil); err == nil || err.Message != ProtectedNotice {
			t.Errorf("Resolve(%s) opened %q in a protected folder (err %v)", path, got, err)
		}
	}
	if dir := CleanDir(filepath.Join(home, "docs")); dir != "" {
		t.Errorf("CleanDir followed a link into a protected folder: %q", dir)
	}
}
