package app

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"testing"
)

// Failure caught: the 2026-10-22 macOS 26.7.1 reboot renumbered the data volume
// (st_dev 16777267 -> 16777266), and every unchanged saved file then read as
// replaced. A sweep re-read all of them (hours), and no source evidence was
// served for any of them. A source stored with the old device number is
// adopted on its own saved bytes: not re-read, and its evidence served. A
// file whose saved bytes changed at the same size is still re-read. Boundary:
// one recovery sweep and the evidence read, over real files.
func TestSourceStoredBeforeAVolumeRenumberIsAdoptedNotReread(t *testing.T) {
	f, _ := newIndexedCodexFixture(t, 2)
	current := f.storedAt(t, f.paths[0]).Source
	if current.Device == 0 || current.Inode == 0 {
		t.Fatalf("a new source has no file identity: %+v", current)
	}
	page, err := f.service.LoadTranscript(f.ctx, f.threadID, transcriptListQuery{Limit: 10, View: devThreadTranscriptView, HiddenSpecified: true})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("transcript = %+v, %v", page, err)
	}
	itemID := page.Items[0].ID
	// The live shape after the reboot: the device number before it.
	oldDevice := uint64(16777267)
	if current.Device == oldDevice {
		oldDevice = 16777266
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE source_files SET device=?`, oldDevice); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.LoadTranscriptEvidence(f.ctx, f.threadID, itemID); !errors.Is(err, errTranscriptEvidenceUnavailable) {
		t.Fatalf("evidence before adoption = %v; want unavailable, as the live store answers", err)
	}
	raw, err := os.ReadFile(f.paths[1])
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.Stat(f.paths[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.paths[1], bytes.Replace(raw, []byte("Prompt"), []byte("Promqt"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same size and the saved modification time: only its saved bytes can tell.
	if err := os.Chtimes(f.paths[1], saved.ModTime(), saved.ModTime()); err != nil {
		t.Fatal(err)
	}
	unchangedBefore, rewrittenBefore := f.storedAt(t, f.paths[0]).Generation, f.storedAt(t, f.paths[1]).Generation

	if projected := f.sweep(t); projected != 1 {
		t.Fatalf("sweep re-read %d sources; want only the one whose saved bytes changed", projected)
	}
	kept := f.storedAt(t, f.paths[0])
	if kept.Generation != unchangedBefore || kept.Source.Device != current.Device {
		t.Fatalf("unchanged file = generation %d (was %d), device %d; want it adopted at device %d",
			kept.Generation, unchangedBefore, kept.Source.Device, current.Device)
	}
	if rewritten := f.storedAt(t, f.paths[1]); rewritten.Generation == rewrittenBefore {
		t.Fatal("a file whose saved bytes changed kept its generation")
	}
	if _, err := f.service.LoadTranscriptEvidence(f.ctx, f.threadID, itemID); err != nil {
		t.Fatalf("evidence after adoption: %v", err)
	}
}
