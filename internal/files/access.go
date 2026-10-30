package files

import (
	"os"
	"path/filepath"
)

// ModificationTimeTolerance is the instruction/file save conflict window, in seconds.
const ModificationTimeTolerance = 0.01

// CanonicalPath cleans, absolutizes, and resolves symlinks so policy checks
// compare real filesystem locations, not spellings.
func CanonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if Protected(abs) {
		return abs
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// ModificationTime returns a file's modification time as unix seconds,
// the unit the write-conflict contract compares.
func ModificationTime(fi os.FileInfo) float64 {
	return float64(fi.ModTime().UnixNano()) / 1e9
}

// WriteAtomic replaces a file with a complete write and preserves the requested mode.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".nano-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
