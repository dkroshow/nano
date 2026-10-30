package files

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Catches: a folder macOS guards with a privacy prompt missing from the list,
// a lookalike (~/Documents-old, /VolumesX) caught by a loose prefix, and a
// file link into one being opened. Boundary: Protected, which every reader
// asks first, and the link resolver that opens files.
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
	if path, _, err := Resolve(file, "", "", nil); err == nil || err.Message != ProtectedNotice {
		t.Errorf("Resolve opened %q in a protected folder (err %v)", path, err)
	}
}
