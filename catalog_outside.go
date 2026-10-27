package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// Sessions Nano did not start ("outside" sessions, such as one opened in a
// terminal) are found from file events, never from a walk. One FSEvents
// stream over the provider roots names the files that changed; a file nothing
// holds is checked against its root's rules, held like an attached file, and
// read by the same one-file pass. Holding it matters: FSEvents does not report
// Codex appends made through a descriptor the writer keeps open, and kqueue
// does. So an outside hold is not released for being quiet, since a writer
// may still hold the file open; at the cap, the least recently active hold
// gives way, and its file is held again on its next FSEvents event.

// outsideHoldLimit caps outside holds, and with them their kqueue descriptors.
const outsideHoldLimit = 256

// outsideHold is one outside session's held files, and when the hold began.
type outsideHold struct {
	paths []string
	since time.Time
}

// outsideChanged is the discovery stream's handler. Files an attachment or an
// outside hold already watches are left to their kqueue watch.
func (s *threadCatalogService) outsideChanged(paths []string) {
	s.attachedMu.Lock()
	unheld := paths[:0:0]
	for _, path := range paths {
		if s.held[path] == nil {
			unheld = append(unheld, path)
		}
	}
	s.attachedMu.Unlock()
	if len(unheld) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var sessions [][]string
	for _, path := range unheld {
		root, ok := s.rootFor(path)
		if !ok || root.Root.Archived {
			continue
		}
		files, source := []string{path}, path
		if base := filepath.Base(path); root.Root.Provider == history.ProviderGrok && (base == "summary.json" || base == "updates.jsonl") {
			// A Grok session's summary carries its title and its updates the
			// transcript; both are held, as for an attachment, and a summary
			// is read as its session's source.
			dir := filepath.Dir(path)
			files = []string{filepath.Join(dir, "summary.json"), filepath.Join(dir, "updates.jsonl")}
			source = grokTranscriptFor(root.Root, path)
		}
		if _, exists, err := history.DiscoverFile(ctx, root.Root, source); err == nil && exists {
			sessions = append(sessions, files)
		}
	}
	if len(sessions) == 0 {
		return
	}
	s.holdOutside(sessions)
	var queued []string
	for _, files := range sessions {
		queued = append(queued, files...)
	}
	s.requestPaths(queued, true)
}

// holdOutside watches each session's files before their pass is queued, so a
// write after the pass's read is always a new event.
func (s *threadCatalogService) holdOutside(sessions [][]string) {
	now := time.Now()
	s.attachedMu.Lock()
	defer s.attachedMu.Unlock()
	for _, files := range sessions {
		if s.held[files[0]] != nil {
			continue // the same session twice in one batch
		}
		if len(s.outside) >= outsideHoldLimit {
			s.releaseIdlestOutsideLocked()
		}
		for _, path := range files {
			s.holdLocked(path)
		}
		s.outside[files[0]] = &outsideHold{paths: files, since: now}
	}
}

// releaseIdlestOutsideLocked ends the outside hold whose files a pass saved
// least recently (or, never saved, that began first).
func (s *threadCatalogService) releaseIdlestOutsideLocked() {
	var idlest string
	var idlestAt time.Time
	for key, hold := range s.outside {
		active := hold.since
		for _, path := range hold.paths {
			if held := s.held[path]; held != nil && held.progressAt.After(active) {
				active = held.progressAt
			}
		}
		if idlest == "" || active.Before(idlestAt) {
			idlest, idlestAt = key, active
		}
	}
	for _, path := range s.outside[idlest].paths {
		s.unholdLocked(path)
	}
	delete(s.outside, idlest)
}
