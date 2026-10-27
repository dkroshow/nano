package history

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// parsedSpoolPattern names ProjectParsedFile's scratch spools.
const parsedSpoolPattern = "nano-parsed-*.json.gz"

func SweepStaleParsedSpools(dir string, minAge time.Duration, now time.Time) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, parsedSpoolPattern))
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures error
	for _, path := range matches {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < minAge {
			continue
		}
		if removeErr := os.Remove(path); removeErr == nil {
			removed++
		} else if !errors.Is(removeErr, os.ErrNotExist) {
			failures = errors.Join(failures, removeErr)
		}
	}
	return removed, failures
}
