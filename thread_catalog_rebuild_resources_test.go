package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Logical size includes pages still in WAL; the final-file ceiling must not
// depend on when SQLite happens to checkpoint. Physical WAL bytes are reported
// separately, while free volume space bounds all temporary allocation.
func copiedIndexResources(path string, logicalBytes, ceilingBytes uint64) (map[string]uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	fileBytes := uint64(info.Size())
	if logicalBytes < fileBytes {
		logicalBytes = fileBytes
	}
	var disk syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &disk); err != nil {
		return nil, err
	}
	freeBytes := uint64(disk.Bavail) * uint64(disk.Bsize)
	bounds := map[string]uint64{"fileBytes": fileBytes, "logicalBytes": logicalBytes, "freeBytes": freeBytes, "ceilingBytes": ceilingBytes, "freeFloorBytes": 40000000000}
	if wal, err := os.Stat(path + "-wal"); err == nil {
		bounds["walBytes"] = uint64(wal.Size())
	} else if !os.IsNotExist(err) {
		return bounds, err
	}
	return bounds, validateCopiedIndexResources(logicalBytes, freeBytes, ceilingBytes)
}

func validateCopiedIndexResources(candidateBytes, freeBytes, ceilingBytes uint64) error {
	if ceilingBytes != 20000000000 && ceilingBytes != 21000000000 {
		return fmt.Errorf("STOP: unsupported candidate ceiling: %d bytes", ceilingBytes)
	}
	if candidateBytes > ceilingBytes {
		return fmt.Errorf("STOP: candidate database exceeds %d bytes: %d bytes", ceilingBytes, candidateBytes)
	}
	if freeBytes < 40000000000 {
		return fmt.Errorf("STOP: volume free space is below 40 GB: %d bytes", freeBytes)
	}
	return nil
}

func TestCopiedIndexResourceStops(t *testing.T) {
	for _, test := range []struct {
		name                string
		size, free, ceiling uint64
		stop                bool
	}{
		{"at limits", 20000000000, 40000000000, 20000000000, false},
		{"below ceiling", 19999999999, 40000000001, 20000000000, false},
		{"candidate over", 20000000001, 40000000000, 20000000000, true},
		{"space below", 10000000000, 39999999999, 20000000000, true},
		{"continuation at limits", 21000000000, 40000000000, 21000000000, false},
		{"continuation candidate over", 21000000001, 40000000000, 21000000000, true},
		{"continuation space below", 20000000001, 39999999999, 21000000000, true},
		{"unsupported ceiling", 20000000000, 40000000000, 22000000000, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCopiedIndexResources(test.size, test.free, test.ceiling); (err != nil) != test.stop {
				t.Fatalf("stop=%t error=%v", test.stop, err)
			}
		})
	}
}
