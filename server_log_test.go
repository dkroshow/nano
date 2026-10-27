package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingLogWriterKeepsOnePreviousFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "server.log")
	writer, err := newRotatingLogWriter(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(path + serverLogBackup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(previous), "one") || !strings.Contains(string(current), "two") {
		t.Fatalf("rotation files = previous %q current %q", previous, current)
	}
}

func TestRotatingLogWriterCreatesProtectedDirectoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "server.log")
	writer, err := newRotatingLogWriter(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("server log mode = %o, want 600", mode)
	}
}

// launchd sends Nano's stdout to the same file the rotating writer owns; a
// line written to both appears twice in server.log.
func TestServerLogWritesEachLineOnceWhenStdoutIsTheLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	stdout, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	savedStdout, savedOutput := os.Stdout, log.Writer()
	os.Stdout = stdout
	t.Cleanup(func() {
		os.Stdout = savedStdout
		log.SetOutput(savedOutput)
		_ = stdout.Close()
	})

	closer := installPersistentServerLog(path)
	if closer == nil {
		t.Fatal("persistent server log not installed")
	}
	log.Print("one-line-marker")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(data), "one-line-marker"); count != 1 {
		t.Fatalf("marker written %d times:\n%s", count, data)
	}
}
