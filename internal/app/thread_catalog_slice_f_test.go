package app

import (
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
)

// TestSourceUnchangedDetectsEachEvidenceDimension exercises the exact predicate
// the batched refreshRoot uses to decide whether a candidate can skip all DB
// work. Each evidence dimension must independently flip the decision to
// "changed" so that a file altered along any dimension still goes through
// projectSource and carries a projection into reconcile. Parser-only drift
// with a healthy checkpoint belongs to the paced background backfill.
func TestSourceUnchangedDetectsEachEvidenceDimension(t *testing.T) {
	root := history.SourceRoot{Provider: history.ProviderClaude, Kind: history.RootClaudeProjects, Path: "/root"}
	modTime := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	const version = "p1"
	baseCandidate := func() history.SourceCandidate {
		return history.SourceCandidate{
			Root: root, Path: "/root/s.jsonl", Kind: history.ThreadMain, Archived: false,
			Size: 100, ModTime: modTime, Device: 1, Inode: 2,
		}
	}
	baseStored := func() history.StoredSource {
		return history.StoredSource{
			Status: history.SourcePresent, ParserVersion: version,
			Projection: &history.ThreadProjection{Provider: root.Provider, NativeThreadID: "n"},
			Source:     baseCandidate(),
		}
	}

	if !sourceUnchanged(baseStored(), baseCandidate(), version, history.TranscriptCheckpoint{}, false) {
		t.Fatal("identical stored and candidate were reported changed")
	}

	cases := []struct {
		name   string
		mutate func(*history.StoredSource, *history.SourceCandidate, *string)
	}{
		{"size grows (append)", func(_ *history.StoredSource, c *history.SourceCandidate, _ *string) { c.Size = 200 }},
		{"size shrinks (truncation/generation)", func(_ *history.StoredSource, c *history.SourceCandidate, _ *string) { c.Size = 50 }},
		{"mtime moves (in-place rewrite)", func(_ *history.StoredSource, c *history.SourceCandidate, _ *string) {
			c.ModTime = modTime.Add(time.Second)
		}},
		{"identity changes (device/inode)", func(_ *history.StoredSource, c *history.SourceCandidate, _ *string) { c.Inode = 99 }},
		{"parser version bumps", func(_ *history.StoredSource, _ *history.SourceCandidate, v *string) { *v = "p2" }},
		{"status not present", func(s *history.StoredSource, _ *history.SourceCandidate, _ *string) { s.Status = history.SourceError }},
		{"projection missing", func(s *history.StoredSource, _ *history.SourceCandidate, _ *string) { s.Projection = nil }},
		{"kind changes", func(_ *history.StoredSource, c *history.SourceCandidate, _ *string) { c.Kind = history.ThreadSubagent }},
		{"archived flips", func(_ *history.StoredSource, c *history.SourceCandidate, _ *string) { c.Archived = true }},
		{"Grok reparenting", func(s *history.StoredSource, c *history.SourceCandidate, _ *string) {
			s.Source.Root.Provider, c.Root.Provider = history.ProviderGrok, history.ProviderGrok
			s.Projection.Kind, s.Projection.ParentNativeID = history.ThreadSubagent, "parent-1"
			c.Kind, c.ParentNativeID = history.ThreadSubagent, "parent-2"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored, candidate, ver := baseStored(), baseCandidate(), version
			tc.mutate(&stored, &candidate, &ver)
			if sourceUnchanged(stored, candidate, ver, history.TranscriptCheckpoint{}, false) {
				t.Fatalf("%s was not detected as a change", tc.name)
			}
		})
	}
	// A parser-version-only difference on a source with its
	// own healthy checkpoint belongs to the paced backfill; with a checkpoint
	// error, or a checkpoint of another source of the thread, it stays a scan
	// change (recovery and metadata-only sources).
	drifted, current := baseStored(), "p2"
	drifted.ID, drifted.CanonicalTranscriptSource = 7, true
	broken := "projection failed"
	if !sourceUnchanged(drifted, baseCandidate(), current, history.TranscriptCheckpoint{SourceID: 7, ReadOffset: 100, ParserVersion: version}, true) {
		t.Fatal("parser-only drift with a healthy behind checkpoint entered the scan's change path")
	}
	if sourceUnchanged(drifted, baseCandidate(), current, history.TranscriptCheckpoint{SourceID: 7, ReadOffset: 100, ParserVersion: version, ErrorSummary: &broken}, true) {
		t.Fatal("parser-only drift with a checkpoint error skipped recovery")
	}
	if sourceUnchanged(drifted, baseCandidate(), current, history.TranscriptCheckpoint{SourceID: 8, ParserVersion: version}, true) {
		t.Fatal("parser-only drift on a side source used another source's checkpoint")
	}
	if sourceUnchanged(drifted, baseCandidate(), current, history.TranscriptCheckpoint{SourceID: 7, ReadOffset: 100, ParserVersion: current}, true) {
		t.Fatal("a behind source with a current transcript is no backfill candidate, so the scan must re-derive it")
	}
	// The source row commits before the transcript rows: a thread's
	// transcript source is read again unless its own current transcript
	// reached the file's end. A side source never needs a transcript.
	for _, tc := range []struct {
		name       string
		canonical  bool
		checkpoint *history.TranscriptCheckpoint
		unchanged  bool
	}{
		{"read to the end", true, &history.TranscriptCheckpoint{SourceID: 7, ReadOffset: 100}, true},
		{"short of the end", true, &history.TranscriptCheckpoint{SourceID: 7, ReadOffset: 60}, false},
		{"no transcript", true, nil, false},
		{"older generation", true, &history.TranscriptCheckpoint{SourceID: 7, SourceGeneration: 1, ReadOffset: 100}, false},
		{"recorded error", true, &history.TranscriptCheckpoint{SourceID: 7, ReadOffset: 60, ErrorSummary: &broken}, true},
		{"side source", false, nil, true},
	} {
		stored := baseStored()
		stored.ID, stored.CanonicalTranscriptSource = 7, tc.canonical
		checkpoint := history.TranscriptCheckpoint{}
		if tc.checkpoint != nil {
			checkpoint = *tc.checkpoint
			checkpoint.ParserVersion = version
		}
		if got := sourceUnchanged(stored, baseCandidate(), version, checkpoint, tc.checkpoint != nil); got != tc.unchanged {
			t.Errorf("%s: unchanged = %t, want %t", tc.name, got, tc.unchanged)
		}
	}
}
