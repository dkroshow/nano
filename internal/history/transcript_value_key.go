package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Version 1 is an exact key, not a digest. JSON is the fallback. Recognized
// canonical identifiers store their hexadecimal bytes without changing value.
// Scopes share their base context and reference their two local keys. This
// removes repeated context from both the table and its unique lookup index.
const transcriptValueKeyVersion byte = 1

func encodeTranscriptValueKeyTx(ctx context.Context, tx *sql.Tx, projectionID int64, raw []byte, create bool) ([]byte, error) {
	if len(raw) > maxStoredContinuationBytes {
		return nil, fmt.Errorf("transcript value exceeds storage bound")
	}
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		canonical, _ := json.Marshal(value)
		// Preserve unusual native JSON escapes instead of silently canonicalizing.
		if bytes.Equal(canonical, raw) {
			for kind, prefix := range []string{"local:", "snapshot:"} {
				if strings.HasPrefix(value, prefix) && len(value) >= len(prefix)+64 {
					text := value[len(prefix) : len(prefix)+64]
					digest, err := hex.DecodeString(text)
					if err == nil && hex.EncodeToString(digest) == text {
						key := []byte{transcriptValueKeyVersion, 'L', byte(kind)}
						key = append(key, digest...)
						return append(key, value[len(prefix)+64:]...), nil
					}
				}
			}
			if len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' {
				text := strings.ReplaceAll(value, "-", "")
				decoded, err := hex.DecodeString(text)
				if err == nil && len(decoded) == 16 && hex.EncodeToString(decoded) == text {
					return append([]byte{transcriptValueKeyVersion, 'U'}, decoded...), nil
				}
			}
		}
	}
	if len(raw) > 0 && raw[0] == '{' {
		var scope model.Scope
		if json.Unmarshal(raw, &scope) == nil && (scope.TurnKey != "" || scope.ItemID != "") {
			canonical, _ := json.Marshal(scope)
			// Only the exact known Scope shape can use references. Unknown native
			// objects and fields retain their original JSON through the fallback.
			if bytes.Equal(canonical, raw) {
				turn, item := scope.TurnKey, scope.ItemID
				scope.TurnKey, scope.ItemID = "", ""
				base, err := transcriptValueIDTx(ctx, tx, projectionID, scope, create)
				if err != nil || base == 0 {
					return nil, err
				}
				ids := []int64{base, 0, 0}
				for i, value := range []string{turn, item} {
					if value != "" {
						ids[i+1], err = transcriptValueIDTx(ctx, tx, projectionID, value, create)
						if err != nil || ids[i+1] == 0 {
							return nil, err
						}
					}
				}
				key := []byte{transcriptValueKeyVersion, 'S'}
				for _, id := range ids {
					key = binary.AppendUvarint(key, uint64(id))
				}
				return key, nil
			}
		}
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid transcript value JSON")
	}
	return append([]byte{transcriptValueKeyVersion, 'J'}, raw...), nil
}

// Non-scope keys decode without database access. Scope references are limited
// to one JSON base plus two scalar keys, so corrupt references cannot recurse.
func decodeTranscriptScalarKey(key []byte) ([]byte, error) {
	if len(key) < 2 || key[0] != transcriptValueKeyVersion || len(key) > maxStoredContinuationBytes+2 {
		return nil, fmt.Errorf("invalid transcript value key version or size")
	}
	switch key[1] {
	case 'J':
		if !json.Valid(key[2:]) {
			return nil, fmt.Errorf("invalid stored transcript value JSON")
		}
		return key[2:], nil
	case 'L':
		if len(key) < 35 || key[2] > 1 || !utf8.Valid(key[35:]) {
			return nil, fmt.Errorf("invalid stored local key")
		}
		prefix := []string{"local:", "snapshot:"}[key[2]]
		return json.Marshal(prefix + hex.EncodeToString(key[3:35]) + string(key[35:]))
	case 'U':
		if len(key) != 18 {
			return nil, fmt.Errorf("invalid stored UUID key")
		}
		h := hex.EncodeToString(key[2:])
		return json.Marshal(h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:])
	default:
		return nil, fmt.Errorf("invalid stored scalar key kind")
	}
}

func decodeTranscriptValueKeyTx(ctx context.Context, tx *sql.Tx, projectionID int64, key []byte) ([]byte, error) {
	if len(key) < 2 || key[0] != transcriptValueKeyVersion || key[1] != 'S' {
		return decodeTranscriptScalarKey(key)
	}
	key = key[2:]
	var ids [3]int64
	for i := range ids {
		value, n := binary.Uvarint(key)
		if n <= 0 || value > 1<<63-1 {
			return nil, fmt.Errorf("invalid stored scope reference")
		}
		ids[i], key = int64(value), key[n:]
	}
	if len(key) != 0 || ids[0] == 0 {
		return nil, fmt.Errorf("invalid stored scope key")
	}
	var scope model.Scope
	for i, id := range ids {
		if id == 0 {
			continue
		}
		referenced, err := loadTranscriptValueKeyTx(ctx, tx, projectionID, id)
		if err != nil {
			return nil, err
		}
		if i == 0 && (len(referenced) < 2 || referenced[1] != 'J') {
			return nil, fmt.Errorf("stored scope base must be JSON")
		}
		raw, err := decodeTranscriptScalarKey(referenced)
		if err != nil {
			return nil, err
		}
		switch i {
		case 0:
			if err = json.Unmarshal(raw, &scope); err != nil {
				return nil, err
			}
			if scope.TurnKey != "" || scope.ItemID != "" {
				return nil, fmt.Errorf("stored scope base contains local keys")
			}
		case 1:
			err = json.Unmarshal(raw, &scope.TurnKey)
		case 2:
			err = json.Unmarshal(raw, &scope.ItemID)
		}
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(scope)
}

func unmarshalTranscriptValueKeyTx(ctx context.Context, tx *sql.Tx, projectionID int64, key []byte, value any) error {
	raw, err := decodeTranscriptValueKeyTx(ctx, tx, projectionID, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
