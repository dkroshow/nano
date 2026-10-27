// Package configpaths defines Nano's default data directory. Server callers
// resolve overrides once and pass explicit paths to stores.
package configpaths

import (
	"os"
	"path/filepath"
)

// DataDir returns the default data directory below home.
func DataDir(home string) string { return filepath.Join(home, ".nanod") }

// DefaultPath resolves a standalone command's default below DataDir.
func DefaultPath(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{DataDir(home)}, parts...)...), nil
}
