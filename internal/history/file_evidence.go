package history

import (
	"context"
	"database/sql"
	"strings"
)

// FindFileEvidenceBlobs returns basename-bearing content from one thread's
// active transcript projection. The caller performs path parsing and
// filesystem verification; this method only keeps the SQLite read bounded and
// excludes superseded generations awaiting collection.
func (s *Store) FindFileEvidenceBlobs(ctx context.Context, threadID, basename string, maxRows, maxBytes int) ([]string, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(basename) == "" || maxRows <= 0 || maxBytes <= 0 {
		return nil, nil
	}
	pattern := "%" + escapeFileEvidenceLike(basename) + "%"
	rows, err := s.db.QueryContext(ctx, `
SELECT ci.display_text, ci.detail_json, cib.body_text, cib.body_json
FROM conversation_projections cp
JOIN conversation_items ci ON ci.projection_id = cp.id
LEFT JOIN conversation_item_bodies cib
  ON cib.projection_id = ci.projection_id AND cib.item_id = ci.item_id
WHERE cp.thread_id = ? AND cp.state = 'active' AND (
  ci.display_text LIKE ? ESCAPE '\' OR
  CAST(ci.detail_json AS TEXT) LIKE ? ESCAPE '\' OR
  COALESCE(cib.body_text, '') LIKE ? ESCAPE '\' OR
  CAST(COALESCE(cib.body_json, '') AS TEXT) LIKE ? ESCAPE '\'
)
ORDER BY ci.projected_seq DESC
LIMIT ?`, threadID, pattern, pattern, pattern, pattern, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blobs []string
	total := 0
	for rows.Next() {
		var display string
		var detail []byte
		var bodyText sql.NullString
		var bodyJSON []byte
		if err := rows.Scan(&display, &detail, &bodyText, &bodyJSON); err != nil {
			return nil, err
		}
		parts := []string{display, string(detail), bodyText.String, string(bodyJSON)}
		for _, part := range parts {
			if part == "" || total >= maxBytes {
				continue
			}
			remaining := maxBytes - total
			if len(part) > remaining {
				part = part[:remaining]
			}
			blobs = append(blobs, part)
			total += len(part)
		}
		if total >= maxBytes {
			break
		}
	}
	return blobs, rows.Err()
}

func escapeFileEvidenceLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}
