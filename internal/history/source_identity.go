package history

import (
	"context"
	"io"
)

// DeviceRenumbered reports a stored source and a file now at its path with the
// same inode but another device number. A reboot can renumber a volume
// (macOS 26.7.1, 2026-10-22), so the file may be unchanged, yet every identity
// check treats it as replaced until AdoptSourceIdentity proves it.
func DeviceRenumbered(stored, current SourceCandidate) bool {
	return stored.Inode != 0 && stored.Inode == current.Inode &&
		stored.Device != 0 && current.Device != 0 && stored.Device != current.Device
}

// AdoptSourceIdentity records a renumbered device on a stored source once the
// file's own saved bytes prove it unchanged: the device was renumbered
// (DeviceRenumbered), the file has not shrunk, and the row's own transcript
// checkpoint still matches the bytes it last parsed
// (TranscriptSourceTailMatches). The generation stays, so nothing is read
// again. It reports false, and changes nothing, without that proof or if the
// row has changed since it was read; the file then stays replaced and is read
// once.
func (s *Store) AdoptSourceIdentity(ctx context.Context, stored StoredSource, current SourceCandidate, file io.ReaderAt) (bool, error) {
	if !DeviceRenumbered(stored.Source, current) || current.Size < stored.Source.Size || !stored.CanonicalTranscriptSource || stored.ThreadID == "" {
		return false, nil
	}
	checkpoint, exists, err := s.LoadTranscriptCheckpoint(ctx, stored.ThreadID)
	if err != nil || !exists {
		return false, err
	}
	if checkpoint.SourceID != stored.ID || checkpoint.SourceGeneration != stored.Generation || checkpoint.ErrorSummary != nil ||
		checkpoint.ReadOffset > current.Size || !TranscriptSourceTailMatches(checkpoint, file) {
		return false, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE source_files SET device=? WHERE id=? AND generation=? AND inode=? AND device=?`,
		current.Device, stored.ID, stored.Generation, current.Inode, stored.Source.Device)
	if err != nil {
		return false, err
	}
	return resultChanged(result)
}
