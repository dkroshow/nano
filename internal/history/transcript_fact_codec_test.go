package history

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestTranscriptFactsCodecPreservesExactJSON(t *testing.T) {
	for _, raw := range []string{`null`, `0`, `false`, " {\n\"unknown\":null,\"zero\":0,\"text\":\"雪\\u0061\"\n} ", `{"text":"` + strings.Repeat("repeated facts 雪 ", 1000) + `"}`} {
		stored, err := encodeTranscriptFacts([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeTranscriptFacts(stored)
		if err != nil || !bytes.Equal(decoded, []byte(raw)) {
			t.Fatalf("facts changed: %v", err)
		}
		if len(raw) > 1000 && (stored[1] != 'Z' || len(stored) >= len(raw)/2) {
			t.Fatal("large repeated facts were not compacted")
		}
	}
}

func TestTranscriptFactsCodecRejectsCorruption(t *testing.T) {
	stored, err := encodeTranscriptFacts([]byte(`{"facts":"` + strings.Repeat("value ", 1000) + `"}`))
	if err != nil || stored[1] != 'Z' {
		t.Fatalf("compressed setup: %v", err)
	}
	size, n := binary.Uvarint(stored[2:])
	wrongLength := append(binary.AppendUvarint([]byte{1, 'Z'}, size-1), stored[2+n:]...)
	badChecksum := append([]byte(nil), stored...)
	badChecksum[len(badChecksum)-1] ^= 1
	for name, bad := range map[string][]byte{
		"missing": nil, "version": {2, 'J', '0'}, "kind": {1, 'X'},
		"invalid JSON": {1, 'J', '{'}, "missing length": {1, 'Z'},
		"oversized":    binary.AppendUvarint([]byte{1, 'Z'}, maxStoredContinuationBytes+1),
		"wrong length": wrongLength, "checksum": badChecksum, "truncated": stored[:len(stored)-2],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeTranscriptFacts(bad); err == nil {
				t.Fatal("damaged facts accepted")
			}
		})
	}
}
