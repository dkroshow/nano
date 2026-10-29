package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// TailJSONLines returns the valid JSON lines in the last tail bytes of an
// append-only ledger, oldest first; a missing file has none. The alert and
// restart ledgers share it.
func TailJSONLines(path string, tail int64) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(0, info.Size()-tail)
	data := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if start > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:] // drop the partial first line
		}
	}
	lines := []json.RawMessage{}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if json.Valid(line) {
			lines = append(lines, json.RawMessage(line))
		}
	}
	return lines, nil
}
