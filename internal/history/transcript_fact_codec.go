package history

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Private derived facts and parser continuations share this container. The
// retained view keeps its existing format. Writers reuse the fixed deflate
// workspace because a rebuild can encode millions of small records.
var transcriptFactWriters = sync.Pool{New: func() any {
	writer, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return writer
}}

func encodeTranscriptFacts(raw []byte) ([]byte, error) {
	if len(raw) > maxStoredContinuationBytes {
		return nil, fmt.Errorf("transcript facts exceed storage bound")
	}
	inline := func() []byte { return append([]byte{1, 'J'}, raw...) }
	if len(raw) < 128 {
		return inline(), nil
	}
	var compressed bytes.Buffer
	writer := transcriptFactWriters.Get().(*zlib.Writer)
	writer.Reset(&compressed)
	_, writeErr := writer.Write(raw)
	closeErr := writer.Close()
	writer.Reset(io.Discard)
	transcriptFactWriters.Put(writer)
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	header := binary.AppendUvarint([]byte{1, 'Z'}, uint64(len(raw)))
	if len(header)+compressed.Len() >= len(raw)+2 {
		return inline(), nil
	}
	return append(header, compressed.Bytes()...), nil
}

func decodeTranscriptFacts(stored []byte) ([]byte, error) {
	if len(stored) < 2 || stored[0] != 1 || len(stored) > maxStoredContinuationBytes+12 {
		return nil, fmt.Errorf("invalid transcript facts version or size")
	}
	if stored[1] == 'J' {
		if len(stored)-2 > maxStoredContinuationBytes || !json.Valid(stored[2:]) {
			return nil, fmt.Errorf("invalid inline transcript facts")
		}
		return stored[2:], nil
	}
	if stored[1] != 'Z' {
		return nil, fmt.Errorf("unknown transcript facts codec")
	}
	size, n := binary.Uvarint(stored[2:])
	if n <= 0 || size > maxStoredContinuationBytes {
		return nil, fmt.Errorf("invalid transcript facts length")
	}
	reader, err := zlib.NewReader(bytes.NewReader(stored[2+n:]))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, int64(size)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) != int(size) || !json.Valid(raw) {
		return nil, fmt.Errorf("transcript facts length or JSON mismatch")
	}
	return raw, nil
}
