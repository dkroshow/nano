package history

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// parkedReader signals that the parse is reading, which happens only after the
// spool exists, then parks the process until the parent kills it.
type parkedReader struct{ ready string }

func (r parkedReader) Read([]byte) (int, error) {
	_ = os.WriteFile(r.ready, nil, 0o600)
	time.Sleep(time.Hour)
	return 0, nil
}

// Catches: a parse spool surviving a Nano killed or restarted mid-parse
// (13.9 GiB of nano-parsed-* files had accumulated by 2026-09-21). Boundary: a
// real child process killed with SIGKILL while ProjectParsedFile is reading.
func TestParsedFileLeavesNoSpoolWhenProcessIsKilled(t *testing.T) {
	if dir := os.Getenv("NANO_PARSED_SPOOL_CHILD_DIR"); dir != "" {
		source := SourceCandidate{Root: SourceRoot{Provider: ProviderCodex}, Path: "/fixture/rollout.jsonl"}
		_, _ = ProjectParsedFile(context.Background(), ParsedFileRequest{Source: source, Reader: parkedReader{ready: filepath.Join(dir, "ready")}, SpoolDir: filepath.Join(dir, "spool")})
		return
	}
	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "spool")
	if err := os.Mkdir(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestParsedFileLeavesNoSpoolWhenProcessIsKilled$")
	child.Env = append(os.Environ(), "NANO_PARSED_SPOOL_CHILD_DIR="+dir)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never started reading")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	left, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range left {
		t.Errorf("killed parse left %s behind", entry.Name())
	}
}

func TestSweepStaleParsedSpoolsRemovesOnlyOldSpools(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return path
	}
	stale := write("nano-parsed-111.json.gz", 2*time.Hour)
	live := write("nano-parsed-222.json.gz", time.Minute)
	unrelated := write("other-333.json.gz", 2*time.Hour)
	removed, err := SweepStaleParsedSpools(dir, time.Hour, now)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale spool survived the sweep")
	}
	for _, kept := range []string{live, unrelated} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("sweep removed %s: %v", kept, err)
		}
	}
}
