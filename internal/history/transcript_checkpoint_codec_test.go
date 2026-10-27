package history

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"strings"
	"testing"
)

func TestTranscriptContinuationStorageRoundTrip(t *testing.T) {
	large := []byte("{\n  \"pending\": " + `"` + strings.Repeat(`a\n世界`, 16000) + `", "nativeTime": null, "zero": 0 }`)
	for _, raw := range [][]byte{nil, []byte(`{}`), []byte(`{"nested":{"_nanoContinuationCodec":"provider-value"}}`), large} {
		stored, err := encodeTranscriptContinuation(raw)
		if err != nil {
			t.Fatal(err)
		}
		if raw != nil && (len(stored) < 2 || stored[0] != 1 || stored[1] != 'J' && stored[1] != 'Z') {
			t.Fatalf("continuation did not use the transcript fact header: %x", stored[:min(2, len(stored))])
		}
		if len(raw) == len(large) && len(stored) >= len(raw)/4 {
			t.Fatal("repeated continuation was stored as another full JSON copy")
		}
		got, err := decodeTranscriptContinuation(stored)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("checkpoint bytes changed: %v", err)
		}
		legacy, err := decodeTranscriptContinuation(raw)
		if err != nil || !bytes.Equal(legacy, raw) {
			t.Fatalf("legacy inline checkpoint changed: %v", err)
		}
	}
}

func TestTranscriptContinuationStorageRejectsCorruption(t *testing.T) {
	raw := []byte(`{"pending":"` + strings.Repeat("payload", 10000) + `"}`)
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	original := storedContinuation{Codec: continuationCodec, Size: len(raw), Data: compressed.Bytes()}
	legacy, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeTranscriptContinuation(legacy)
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("legacy compressed continuation changed: %v", err)
	}
	for _, name := range []string{"unknown codec", "oversized", "understated size", "overstated size", "damaged stream"} {
		t.Run(name, func(t *testing.T) {
			bad := original
			switch name {
			case "unknown codec":
				bad.Codec = "zlib-v999"
			case "oversized":
				bad.Size = maxStoredContinuationBytes + 1
			case "understated size":
				bad.Size = 8
			case "overstated size":
				bad.Size++
			case "damaged stream":
				bad.Data = append([]byte(nil), bad.Data...)
				bad.Data[len(bad.Data)-1] ^= 1
			}
			raw, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeTranscriptContinuation(raw); err == nil {
				t.Fatal("unreadable continuation accepted")
			}
		})
	}
	if _, err := decodeTranscriptContinuation([]byte{1, 'X'}); err == nil {
		t.Fatal("invalid shared codec header was accepted")
	}
}
