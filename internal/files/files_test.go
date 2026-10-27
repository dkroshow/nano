package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDirectFormsAndKeepsSearchInsideRepository(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	cwd := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(cwd, "notes.md")
	// Explicit parent paths may leave the repository; basename search must not.
	outside := filepath.Join(root, "README.md")
	if err := os.WriteFile(inside, []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	home := filepath.Join(root, "home.md")
	if err := os.WriteFile(home, []byte("home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		requested string
		want      string
		by        string
	}{
		{inside, inside, "absolute"},
		{"notes.md", inside, "cwd"},
		{"./notes.md", inside, "cwd"},
		{"../../README.md", outside, "cwd"},
		{"~/home.md", home, "absolute"},
	} {
		got, by, err := Resolve(test.requested, cwd, "", nil)
		if err != nil || got != canonicalPath(test.want) || by != test.by {
			t.Fatalf("Resolve(%q) = %q, %q, %v", test.requested, got, by, err)
		}
	}
	if _, _, err := Resolve("README.md", cwd, "", nil); err == nil || err.Code != "not_found" {
		t.Fatalf("missing = %#v", err)
	}
}

func TestResolveUnversionedDirectoryIsItsOwnBoundary(t *testing.T) {
	root := canonicalPath(t.TempDir())
	for dir := root; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			t.Skip("temp dir is inside a Git repository; the unversioned boundary cannot be isolated")
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	cwd := filepath.Join(root, "pkg")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve("README.md", cwd, "", nil); err == nil || err.Code != "not_found" {
		t.Fatalf("parent of an unversioned cwd was searched: %#v", err)
	}
}

func TestInspectRejectsContentAndMarksProtectedFilesReadOnly(t *testing.T) {
	root := t.TempDir()
	text := filepath.Join(root, "note.md")
	binary := filepath.Join(root, "binary")
	if err := os.WriteFile(text, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("before\x00after"), 0o644); err != nil {
		t.Fatal(err)
	}
	text = canonicalPath(text)
	root = canonicalPath(root)
	meta, content, err := Inspect(text, []string{root})
	if err != nil || meta.Editable || string(content) != "hello\n" {
		t.Fatalf("text = %+v, %q, %v", meta, content, err)
	}
	binary = canonicalPath(binary)
	if _, _, err := Inspect(binary, nil); err == nil || err.Code != "not_text" {
		t.Fatalf("binary = %#v", err)
	}
}
