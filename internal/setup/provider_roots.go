package setup

import (
	"os"
	"path/filepath"

	filemodel "github.com/dkroshow/nano/internal/files"
)

// ProtectedProviderRoots returns the provider-owned trees that file views may read only.
func ProtectedProviderRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filemodel.CanonicalPath(filepath.Join(home, ".codex")),
		filemodel.CanonicalPath(filepath.Join(home, ".claude")),
		filemodel.CanonicalPath(filepath.Join(home, ".grok")),
	}
}
