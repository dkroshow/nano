package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/configpaths"
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
		if err := ValidateCatalogLocation(database, providerHomes); err == nil {
			t.Fatalf("provider-owned database path %q was accepted", database)
		}
	}
	if err := ValidateCatalogLocation(filepath.Join(home, ".nanod", "history.sqlite"), providerHomes); err != nil {
		t.Fatalf("Nano-owned database path was rejected: %v", err)
	}

	link := filepath.Join(home, "codex-link")
	if err := os.Symlink(codexHome, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := configpaths.CanonicalProspective(filepath.Join(link, "nested", "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCatalogLocation(resolved, providerHomes); err == nil {
		t.Fatalf("symlinked provider-owned database path %q was accepted", resolved)
	}
}
