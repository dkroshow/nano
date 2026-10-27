package history

import (
	"context"
	"fmt"
)

func (s *Store) SupersededProjectionSummary(ctx context.Context, largestLimit int, visit func(int64, string, int64) error) (int64, int64, error) {
	var candidates, items int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(older.item_count), 0)
FROM conversation_projections older
WHERE `+supersededProjectionEligibility).Scan(&candidates, &items); err != nil {
		return 0, 0, fmt.Errorf("summarize superseded projections: %w", err)
	}
	if largestLimit <= 0 || visit == nil {
		return candidates, items, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT older.id, older.thread_id, older.item_count
FROM conversation_projections older
WHERE `+supersededProjectionEligibility+`
ORDER BY older.item_count DESC, older.id ASC
LIMIT ?`, largestLimit)
	if err != nil {
		return 0, 0, fmt.Errorf("list largest superseded projections: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var projectionID, projectionItems int64
		var threadID string
		if err := rows.Scan(&projectionID, &threadID, &projectionItems); err != nil {
			return 0, 0, err
		}
		if err := visit(projectionID, threadID, projectionItems); err != nil {
			return 0, 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	return candidates, items, nil
}

// Aliases predate projection ownership. Maintenance removes old dangling rows
// without putting their 4.4-million-row scan in Nano's periodic server pass.
func (s *Store) CollectDanglingItemAliasesBatch(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 || limit > maxSupersededProjectionBatchItems {
		limit = maxSupersededProjectionBatchItems
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `DELETE FROM conversation_item_aliases WHERE rowid IN (
SELECT alias.rowid
FROM conversation_item_aliases alias
WHERE NOT EXISTS (
  SELECT 1 FROM conversation_items item
  JOIN conversation_projections projection ON projection.id=item.projection_id
  WHERE projection.thread_id=alias.thread_id AND item.item_id=alias.canonical_item_id
)
ORDER BY alias.rowid
LIMIT ?
)`, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
