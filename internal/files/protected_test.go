package files

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Catches: a folder macOS guards with a privacy prompt missing from the list,
// a lookalike (~/Documents-old, /VolumesX) caught by a loose prefix, and a
// file link into one being opened, directly, through a symlink outside it, or
// by the suffix search walking down from home (each seen in review).
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
	alias := filepath.Join(home, "notes.md")
	if err := os.Symlink(file, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Documents"), filepath.Join(home, "docs")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, alias, filepath.Join(home, "docs", "notes.md")} {
		if got, _, err := Resolve(path, "", "", nil); err == nil || err.Message != ProtectedNotice {
			t.Errorf("Resolve(%s) opened %q in a protected folder (err %v)", path, got, err)
		}
	}
	if dir := CleanDir(filepath.Join(home, "docs")); dir != "" {
		t.Errorf("CleanDir followed a link into a protected folder: %q", dir)
	}
	deep := filepath.Join(home, "Documents", "deep", "only.md")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deep, []byte("only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _, err := Resolve("deep/only.md", home, "", nil); err == nil {
		t.Errorf("the suffix search from home walked into a protected folder and found %q", got)
	}
}
