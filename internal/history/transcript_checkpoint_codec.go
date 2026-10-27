package history

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type continuationEntry struct {
	field string
	key   string
	value json.RawMessage
	hash  [sha256.Size]byte
}

func splitTranscriptContinuation(raw []byte) ([]byte, []continuationEntry, error) {
	if raw == nil {
		return nil, nil, nil
	}
	var checkpoint parsedFileCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil || len(checkpoint.Reader) == 0 {
		encoded, encodeErr := encodeTranscriptContinuation(raw)
		return encoded, nil, encodeErr
	}
	var reader map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint.Reader, &reader); err != nil {
		encoded, encodeErr := encodeTranscriptContinuation(raw)
		return encoded, nil, encodeErr
	}
	var entries []continuationEntry
	for field, encoded := range reader {
		var values map[string]json.RawMessage
		if len(encoded) == 0 || encoded[0] != '{' || json.Unmarshal(encoded, &values) != nil {
			continue
		}
		for key, value := range values {
			entries = append(entries, continuationEntry{field: field, key: key, value: value, hash: sha256.Sum256(value)})
		}
		reader[field] = json.RawMessage(`{}`)
	}
	baseReader, err := json.Marshal(reader)
	if err != nil {
		return nil, nil, err
	}
	checkpoint.Reader = baseReader
	base, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, nil, err
	}
	encoded, err := encodeTranscriptContinuation(base)
	return encoded, entries, err
}

func restoreTranscriptContinuation(base []byte, entries []continuationEntry) (json.RawMessage, error) {
	raw, err := decodeTranscriptContinuation(base)
	if err != nil || len(entries) == 0 {
		return raw, err
	}
	var checkpoint parsedFileCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return nil, err
	}
	var reader map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint.Reader, &reader); err != nil {
		return nil, err
	}
	fields := make(map[string]map[string]json.RawMessage)
	for _, entry := range entries {
		values := fields[entry.field]
		if values == nil {
			values = make(map[string]json.RawMessage)
			fields[entry.field] = values
		}
		values[entry.key] = entry.value
	}
	for field, values := range fields {
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		reader[field] = encoded
	}
	encodedReader, err := json.Marshal(reader)
	if err != nil {
		return nil, err
	}
	checkpoint.Reader = encodedReader
	return json.Marshal(checkpoint)
}

func loadContinuationEntries(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectionID int64) ([]continuationEntry, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT field,entry_key,entry_json FROM conversation_parser_state_entries WHERE projection_id=? ORDER BY entry_sequence`, projectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []continuationEntry
	for rows.Next() {
		var entry continuationEntry
		if err := rows.Scan(&entry.field, &entry.key, &entry.value); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func syncContinuationEntries(ctx context.Context, tx *sql.Tx, projectionID int64, entries []continuationEntry) error {
	rows, err := tx.QueryContext(ctx, `SELECT entry_sequence,field,entry_key,entry_sha256 FROM conversation_parser_state_entries WHERE projection_id=?`, projectionID)
	if err != nil {
		return err
	}
	existing := make(map[string][]byte)
	var nextSequence int64
	for rows.Next() {
		var sequence int64
		var field, key string
		var hash []byte
		if err := rows.Scan(&sequence, &field, &key, &hash); err != nil {
			rows.Close()
			return err
		}
		existing[field+"\x00"+key] = hash
		if sequence >= nextSequence {
			nextSequence = sequence + 1
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, entry := range entries {
		identity := entry.field + "\x00" + entry.key
		if bytes.Equal(existing[identity], entry.hash[:]) {
			delete(existing, identity)
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_parser_state_entries(projection_id,entry_sequence,field,entry_key,entry_json,entry_sha256) VALUES(?,?,?,?,?,?) ON CONFLICT(projection_id,field,entry_key) DO UPDATE SET entry_json=excluded.entry_json,entry_sha256=excluded.entry_sha256`, projectionID, nextSequence, entry.field, entry.key, []byte(entry.value), entry.hash[:]); err != nil {
			return err
		}
		nextSequence++
		delete(existing, identity)
	}
	for identity := range existing {
		field, key, _ := strings.Cut(identity, "\x00")
		if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_parser_state_entries WHERE projection_id=? AND field=? AND entry_key=?`, projectionID, field, key); err != nil {
			return err
		}
	}
	return nil
}

// The SQL container is distinct from the provider's checkpoint version. The
// reader still receives exactly the JSON bytes it emitted. Keep small states
// inline and accept old inline states, including those from schema 16.
const continuationCodec = "zlib-v1"

// This bounds allocation when opening a damaged local index. Oversized states
// fail storage explicitly; they are never truncated or reported as persisted.
// It is separate from the module's advisory 4 MiB continuation bound.
const maxStoredContinuationBytes = 512 << 20

type storedContinuation struct {
	Codec string `json:"_nanoContinuationCodec"`
	Size  int    `json:"decodedBytes"`
	Data  []byte `json:"data"`
}

func encodeTranscriptContinuation(raw []byte) ([]byte, error) {
	if raw == nil {
		return nil, nil
	}
	return encodeTranscriptFacts(raw)
}

func decodeTranscriptContinuation(raw []byte) (json.RawMessage, error) {
	if len(raw) >= 2 && raw[0] == 1 {
		decoded, err := decodeTranscriptFacts(raw)
		return json.RawMessage(decoded), err
	}
	if len(raw) > maxStoredContinuationBytes {
		return nil, fmt.Errorf("stored transcript continuation exceeds decode bound")
	}
	// Inline checkpoints have no reserved container marker. Avoid decoding a
	// complete legacy provider state merely to identify its storage format.
	if !bytes.Contains(raw, []byte(`"_nanoContinuationCodec"`)) {
		return append(json.RawMessage(nil), raw...), nil
	}
	var stored storedContinuation
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode transcript continuation container: %w", err)
	}
	if stored.Codec == "" { // The token may occur inside a legacy payload.
		return append(json.RawMessage(nil), raw...), nil
	}
	if stored.Codec != continuationCodec || stored.Size < 0 || stored.Size > maxStoredContinuationBytes {
		return nil, fmt.Errorf("unsupported or oversized transcript continuation container")
	}
	reader, err := zlib.NewReader(bytes.NewReader(stored.Data))
	if err != nil {
		return nil, fmt.Errorf("open transcript continuation: %w", err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(stored.Size)+1))
	if err != nil {
		return nil, fmt.Errorf("read transcript continuation: %w", err)
	}
	if len(decoded) != stored.Size || !json.Valid(decoded) {
		return nil, fmt.Errorf("invalid transcript continuation length or JSON")
	}
	return decoded, nil
}
