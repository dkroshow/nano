package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestThreadCatalogDatabaseCannotLiveInProviderDirectory(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	claudeHome := filepath.Join(home, ".claude")
	for _, directory := range []string{codexHome, claudeHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	providerHomes := []string{codexHome, claudeHome}
	for _, database := range []string{
		filepath.Join(codexHome, "nano.sqlite"),
		filepath.Join(claudeHome, "cache", "nano.sqlite"),
	} {
		if err := validateThreadCatalogDBLocation(database, providerHomes); err == nil {
			t.Fatalf("provider-owned database path %q was accepted", database)
		}
	}
	if err := validateThreadCatalogDBLocation(filepath.Join(home, ".nanod", "history.sqlite"), providerHomes); err != nil {
		t.Fatalf("Nano-owned database path was rejected: %v", err)
	}

	link := filepath.Join(home, "codex-link")
	if err := os.Symlink(codexHome, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := canonicalProspectivePath(filepath.Join(link, "nested", "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateThreadCatalogDBLocation(resolved, providerHomes); err == nil {
		t.Fatalf("symlinked provider-owned database path %q was accepted", resolved)
	}
}
