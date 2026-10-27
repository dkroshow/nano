package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/history"
)

type evidenceRangeReader struct {
	start int64
	data  []byte
}

func (r evidenceRangeReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset != r.start {
		return 0, fmt.Errorf("unexpected prefix read at %d", offset)
	}
	return copy(p, r.data), nil
}

func TestTranscriptEvidenceRangeChecksExactBytes(t *testing.T) {
	for _, ending := range []string{"", "\n", "\r\n"} {
		t.Run(fmt.Sprintf("terminator-%d", len(ending)), func(t *testing.T) {
			native := ` {"type":"user","text":"native whitespace stays"} `
			raw := native + ending
			ref := history.TranscriptEvidenceRef{HasByteRange: true, ByteStart: 10000000, ByteLength: int64(len(raw)), CompleteOffset: 10000000 + int64(len(raw)), SourceLineSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(native)))}
			got, err := readTranscriptEvidenceRange(context.Background(), evidenceRangeReader{ref.ByteStart, []byte(raw)}, ref)
			if err != nil || string(got) != native {
				t.Fatalf("range bytes=%q err=%v", got, err)
			}
			changed := strings.ReplaceAll(raw, "native", "edited")
			if _, err := readTranscriptEvidenceRange(context.Background(), evidenceRangeReader{ref.ByteStart, []byte(changed)}, ref); !errors.Is(err, errTranscriptEvidenceUnavailable) {
				t.Fatalf("changed bytes accepted: %v", err)
			}
		})
	}
	// A whole JSON document includes its trailing newline in the native digest.
	document := "{\n  \"messages\": []\n}\n"
	ref := history.TranscriptEvidenceRef{HasByteRange: true, ByteLength: int64(len(document)), CompleteOffset: int64(len(document)), SourceLineSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(document)))}
	got, err := readTranscriptEvidenceRange(context.Background(), strings.NewReader(document), ref)
	if err != nil || string(got) != document {
		t.Fatalf("document whitespace changed: %q %v", got, err)
	}
	for _, invalid := range []history.TranscriptEvidenceRef{
		{ByteStart: -1, ByteLength: 1, CompleteOffset: 100},
		{ByteStart: 0, ByteLength: 0, CompleteOffset: 100},
		{ByteStart: 99, ByteLength: 2, CompleteOffset: 100},
		{ByteStart: 1 << 62, ByteLength: 1 << 62, CompleteOffset: 1 << 62},
	} {
		if _, err := readTranscriptEvidenceRange(context.Background(), strings.NewReader(document), invalid); !errors.Is(err, errTranscriptEvidenceUnavailable) {
			t.Fatalf("invalid bounds accepted: %+v %v", invalid, err)
		}
	}
	if _, err := readTranscriptEvidenceRange(context.Background(), strings.NewReader(document[:5]), ref); !errors.Is(err, errTranscriptEvidenceUnavailable) {
		t.Fatalf("truncated range accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTranscriptEvidenceRange(ctx, strings.NewReader(document), ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
