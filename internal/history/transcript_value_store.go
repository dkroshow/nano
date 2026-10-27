package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Values are exact scalar identities or source/scope definitions, never native
// payloads. The versioned key is reversible and compared in full, without hashing.
// Integer references keep those values out of every relationship and index.
func internTranscriptValueTx(ctx context.Context, tx *sql.Tx, projectionID int64, value any) (int64, error) {
	return transcriptValueIDTx(ctx, tx, projectionID, value, true)
}

func findTranscriptValueTx(ctx context.Context, tx *sql.Tx, projectionID int64, value any) (int64, error) {
	return transcriptValueIDTx(ctx, tx, projectionID, value, false)
}

func optionalTranscriptStringTx(ctx context.Context, tx *sql.Tx, projectionID int64, value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	return internTranscriptValueTx(ctx, tx, projectionID, value)
}

func transcriptValueIDTx(ctx context.Context, tx *sql.Tx, projectionID int64, value any, create bool) (int64, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	cache, _ := ctx.Value(transcriptValueCacheKey{}).(*transcriptValueCache)
	if cache != nil && cache.projectionID == projectionID {
		if id := cache.ids[string(raw)]; id != 0 {
			return id, nil
		}
	}
	key, err := encodeTranscriptValueKeyTx(ctx, tx, projectionID, raw, create)
	if err != nil || key == nil {
		return 0, err
	}
	if create {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_parse_values(projection_id,value_key) VALUES (?,?)`, projectionID, key); err != nil {
			return 0, fmt.Errorf("intern transcript value: %w", err)
		}
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_parse_values WHERE projection_id=? AND value_key=?`, projectionID, key).Scan(&id); err != nil {
		if err == sql.ErrNoRows && !create {
			return 0, nil
		}
		return 0, err
	}
	if cache != nil {
		if cache.projectionID != projectionID || len(cache.ids) >= 4096 || cache.bytes+len(raw) > 4<<20 {
			cache.projectionID, cache.ids, cache.bytes = projectionID, make(map[string]int64), 0
		}
		if len(raw) <= 4<<20 {
			cache.ids[string(raw)] = id
			cache.bytes += len(raw)
		}
	}
	return id, nil
}

func loadTranscriptValueTx(ctx context.Context, tx *sql.Tx, projectionID, id int64, value any) error {
	raw, err := loadTranscriptValueKeyTx(ctx, tx, projectionID, id)
	if err != nil {
		return err
	}
	return unmarshalTranscriptValueKeyTx(ctx, tx, projectionID, raw, value)
}

func loadTranscriptValueKeyTx(ctx context.Context, tx *sql.Tx, projectionID, id int64) ([]byte, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT value_key FROM conversation_parse_values WHERE projection_id=? AND id=?`, projectionID, id).Scan(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

type transcriptValueCacheKey struct{}

// This cache lives only for one serialized ApplyTranscript transaction. Limits
// bound both unique values and bytes; eviction only causes another SQL lookup.
type transcriptValueCache struct {
	projectionID int64
	ids          map[string]int64
	bytes        int
}
