// Package configpaths owns default data locations and path resolution.
// Callers resolve overrides once and pass explicit paths to stores.
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

// CanonicalProspective resolves symlinks in the existing portion of a path
// without creating the target or any parent directory.
func CanonicalProspective(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolute), nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}
