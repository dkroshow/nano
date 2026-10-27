package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	serverLogMaxBytes = 10 << 20
	serverLogBackup   = ".1"
)

// rotatingLogWriter keeps the process log useful after the terminal that
// launched nano has gone away. It retains one previous file and never writes
// provider prompt text itself; callers use the standard logger as before.
type rotatingLogWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	file     *os.File
	size     int64
}

func newRotatingLogWriter(path string, maxBytes int64) (*rotatingLogWriter, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("server log path is empty")
	}
	if maxBytes <= 0 {
		return nil, errors.New("server log size must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create server log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("protect server log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat server log: %w", err)
	}
	return &rotatingLogWriter{path: path, maxBytes: maxBytes, file: file, size: info.Size()}, nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			// Keep logging to the current file if rotation is temporarily
			// unavailable. A failed rotation must not make stdout disappear.
			_, _ = fmt.Fprintf(os.Stderr, "WARNING: rotate server log %s: %v\n", w.path, err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingLogWriter) rotateLocked() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	backup := w.path + serverLogBackup
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return w.reopenCurrentLocked(err)
	}
	if err := os.Rename(w.path, backup); err != nil {
		return w.reopenCurrentLocked(err)
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.Rename(backup, w.path)
		return w.reopenCurrentLocked(err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Rename(backup, w.path)
		return w.reopenCurrentLocked(err)
	}
	w.file = file
	w.size = 0
	return nil
}

func (w *rotatingLogWriter) reopenCurrentLocked(cause error) error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("%v; reopen server log: %w", cause, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("%v; stat reopened server log: %w", cause, statErr)
	}
	w.file = file
	w.size = info.Size()
	return cause
}

func (w *rotatingLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// installPersistentServerLog keeps the standard logger's stdout behavior and
// adds a bounded on-disk copy. It is deliberately best-effort: logging must
// never prevent nano from starting when the user's log directory is not
// writable. When stdout already is that file (the launchd job redirects it
// there), the rotating writer is the only writer, so each line appears once.
func installPersistentServerLog(path string) io.Closer {
	writer, err := newRotatingLogWriter(path, serverLogMaxBytes)
	if err != nil {
		log.Printf("WARNING: persistent server log unavailable (%s): %v", path, err)
		return nil
	}
	log.SetOutput(io.MultiWriter(os.Stdout, writer))
	if stdout, err := os.Stdout.Stat(); err == nil {
		if file, err := writer.file.Stat(); err == nil && os.SameFile(stdout, file) {
			log.SetOutput(writer)
		}
	}
	log.Printf("persistent server log using %s", path)
	return writer
}
