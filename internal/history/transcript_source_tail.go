package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

// sourceTailBytes bounds the digest to the end of the parsed bytes: provider
// transcripts are append-only, so an in-place rewrite shows up there.
const sourceTailBytes = 64 << 10

func sourceTailDigest(file io.ReaderAt, end int64) (int64, string, error) {
	start := end - sourceTailBytes
	if start < 0 {
		start = 0
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(file, start, end-start)); err != nil {
		return 0, "", err
	}
	return start, hex.EncodeToString(hash.Sum(nil)), nil
}

// TranscriptSourceTailMatches reports whether the bytes the checkpoint last
// parsed are still the same in file. A checkpoint written before digests
// existed never matches, so its thread rebuilds once and records one.
func TranscriptSourceTailMatches(checkpoint TranscriptCheckpoint, file io.ReaderAt) bool {
	var state parsedFileCheckpoint
	if json.Unmarshal(checkpoint.ParserState, &state) != nil || state.TailSHA256 == "" {
		return false
	}
	if state.TailStart < 0 || state.TailStart > checkpoint.ReadOffset {
		return false
	}
	start, digest, err := sourceTailDigest(file, checkpoint.ReadOffset)
	return err == nil && start == state.TailStart && digest == state.TailSHA256
}
