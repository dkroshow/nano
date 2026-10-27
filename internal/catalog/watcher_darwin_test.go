//go:build darwin

package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type watchHints chan string

func startTestWatcher(t *testing.T) (*FileWatcher, watchHints) {
	t.Helper()
	hints := make(watchHints, 64)
	w, err := StartFileWatcher(func(path string) { hints <- path }, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w, hints
}

func (h watchHints) expect(t *testing.T, path string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-h:
			if got == path {
				return
			}
		case <-deadline:
			t.Fatalf("no hint for %s", path)
		}
	}
}

func (h watchHints) expectNone(t *testing.T) {
	t.Helper()
	select {
	case got := <-h:
		t.Fatalf("unexpected hint for %s", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func (h watchHints) drain() {
	for {
		select {
		case <-h:
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

func waitStats(t *testing.T, w *FileWatcher, want func(WatchStats) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !want(w.Stats()) {
		if time.Now().After(deadline) {
			t.Fatalf("stats = %+v", w.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func openDescriptors(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

// Failure caught: a writer that keeps its descriptor open (Codex's app server)
// appends without closing, which FSEvents never reported. Boundary: kqueue
// delivery for a held-open writer, one hint per observed write.
func TestFileWatcherHintsEveryAppendFromHeldOpenWriter(t *testing.T) {
	w, hints := startTestWatcher(t)
	home := t.TempDir()
	path := filepath.Join(home, "rollout.jsonl")
	writer, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	w.Watch(path, home)
	for _, line := range []string{"{\"a\":1}\n", "{\"b\":2}\n", "{\"c\":"} {
		if _, err := writer.WriteString(line); err != nil {
			t.Fatal(err)
		}
		hints.expect(t, path)
	}
	if stats := w.Stats(); stats.Files != 1 || stats.Directories != 0 || stats.Waiting != 0 {
		t.Fatalf("stats = %+v, want one direct file watch", stats)
	}
}

// Failure caught: an attached thread whose file (and dated directory) does not
// exist yet would never be ingested without a timer or a full scan.
// Boundary: absent-file waiting turns into a file watch on creation.
func TestFileWatcherWaitsForAbsentFileAndMissingDirectories(t *testing.T) {
	w, hints := startTestWatcher(t)
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "10", "03")
	first := filepath.Join(day, "rollout-a.jsonl")
	second := filepath.Join(day, "rollout-b.jsonl")
	w.Watch(first, home)
	w.Watch(second, home)
	if stats := w.Stats(); stats.Waiting != 2 || stats.Directories != 1 || stats.Files != 0 {
		t.Fatalf("waiting stats = %+v, want two paths sharing one directory watch", stats)
	}
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	hints.expectNone(t)
	if err := os.WriteFile(first, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hints.expect(t, first)
	waitStats(t, w, func(stats WatchStats) bool { return stats.Files == 1 && stats.Waiting == 1 && stats.Directories == 1 })
	w.Unwatch(second)
	if stats := w.Stats(); stats.Directories != 0 || stats.Waiting != 0 {
		t.Fatalf("after release stats = %+v, want the directory watch released", stats)
	}
}

// Failure caught: an atomic replacement left the watch on the unlinked file,
// so later writes to the new file were silent, and a write through the old
// descriptor was mistaken for the new file. Boundary: rebind on replacement.
func TestFileWatcherFollowsReplacementAndIgnoresTheOldFile(t *testing.T) {
	w, hints := startTestWatcher(t)
	home := t.TempDir()
	path := filepath.Join(home, "summary.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	w.Watch(path, home)
	next := filepath.Join(home, "summary.json.tmp")
	if err := os.WriteFile(next, []byte("{\"v\":2}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	hints.expect(t, path)
	hints.expectNone(t)
	if _, err := old.WriteString(" "); err != nil {
		t.Fatal(err)
	}
	hints.expectNone(t)
	if err := os.WriteFile(path, []byte("{\"v\":3}"), 0o600); err != nil {
		t.Fatal(err)
	}
	hints.expect(t, path)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	hints.expect(t, path)

	hints.drain()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	hints.expect(t, path)
	waitStats(t, w, func(stats WatchStats) bool { return stats.Waiting == 1 && stats.Directories == 1 })
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	hints.expect(t, path)
}

// Failure caught: released or shut-down watches kept descriptors open, so
// attach/detach cycles leaked them. Boundary: descriptor count and ownership
// counts return to their baseline.
func TestFileWatcherReleaseAndCloseReturnDescriptors(t *testing.T) {
	home := t.TempDir()
	present := filepath.Join(home, "present.jsonl")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(home, "later", "absent.jsonl")
	baseline := openDescriptors(t)
	w, _ := startTestWatcher(t)
	for cycle := 0; cycle < 3; cycle++ {
		w.Watch(present, home)
		w.Watch(present, home)
		w.Watch(absent, home)
		if stats := w.Stats(); stats.Paths != 2 || stats.Files != 1 || stats.Directories != 1 {
			t.Fatalf("cycle %d held stats = %+v", cycle, stats)
		}
		w.Unwatch(present)
		w.Unwatch(absent)
		if stats := w.Stats(); stats != (WatchStats{}) {
			t.Fatalf("cycle %d released stats = %+v, want zero", cycle, stats)
		}
	}
	w.Watch(present, home)
	w.Close()
	deadline := time.Now().Add(2 * time.Second)
	for openDescriptors(t) != baseline {
		if time.Now().After(deadline) {
			t.Fatalf("descriptors = %d after close, want baseline %d", openDescriptors(t), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
