package history

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// maintainConversationSearchForProjectionSequenceRangeTx applies the shared corpus
// boundary to newly written normalized items. It is deliberately called inside
// the source transaction: an accepted transcript change cannot commit without
// its ready/building derived rows.
func maintainConversationSearchForProjectionSequenceRangeTx(ctx context.Context, tx *sql.Tx, projectionID, afterSequence, throughSequence int64) (bool, error) {
	if projectionID <= 0 || throughSequence <= afterSequence {
		return false, nil
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return false, err
	}
	modes := maintainedConversationSearchModes(statuses)
	if len(modes) == 0 {
		return false, nil
	}
	changed := false
	err = VisitPersistedConversationSearchCandidatesForProjectionSequenceRange(ctx, tx, projectionID, afterSequence, throughSequence, func(_ ConversationSearchCandidate, item EligibleConversationSearchItem, eligible bool) error {
		if !eligible {
			return nil
		}
		updated, err := upsertConversationSearchEligibleItemTx(ctx, tx, modes, item)
		changed = changed || updated
		return err
	})
	if err != nil || !changed {
		return changed, err
	}
	return true, updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC())
}

// maintainConversationSearchForProjectionsTx reconciles only affected
// projections. It still delegates every inclusion decision to
// EligibleConversationItem; its SQL merely scopes the source rows whose state
// changed (for example a Guardian reclassification or projection reassignment).
func maintainConversationSearchForProjectionsTx(ctx context.Context, tx *sql.Tx, projectionIDs []int64) (bool, error) {
	projectionIDs = uniqueInt64s(projectionIDs)
	if len(projectionIDs) == 0 {
		return false, nil
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return false, err
	}
	modes := maintainedConversationSearchModes(statuses)
	if len(modes) == 0 {
		return false, nil
	}
	changed := false
	err = VisitPersistedConversationSearchCandidatesForProjections(ctx, tx, projectionIDs, func(candidate ConversationSearchCandidate, item EligibleConversationSearchItem, eligible bool) error {
		if !eligible {
			removed, err := removeConversationSearchDocumentTx(ctx, tx, candidate.Identity.ProjectionID, candidate.Identity.ItemID)
			changed = changed || removed
			return err
		}
		updated, err := upsertConversationSearchEligibleItemTx(ctx, tx, modes, item)
		changed = changed || updated
		return err
	})
	if err != nil || !changed {
		return changed, err
	}
	return true, updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC())
}

func maintainConversationSearchForThreadsTx(ctx context.Context, tx *sql.Tx, threadIDs []string) (bool, error) {
	threadIDs = uniqueStrings(threadIDs...)
	if len(threadIDs) == 0 {
		return false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(threadIDs)), ",")
	rows, err := tx.QueryContext(ctx, `SELECT id FROM conversation_projections WHERE state = 'active' AND thread_id IN (`+marks+`)`, stringsToAny(threadIDs)...)
	if err != nil {
		return false, err
	}
	var projections []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		projections = append(projections, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed, err := maintainConversationSearchForProjectionsTx(ctx, tx, projections)
	if err != nil || changed {
		return changed, err
	}
	// Kind and thread ownership participate in grouped search results even when
	// the projection/item membership identity stays the same. Publish one
	// revision for such a metadata-only visible change, without double-counting
	// operations that already inserted or removed derived rows above.
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return false, err
	}
	if len(maintainedConversationSearchModes(statuses)) == 0 {
		return false, nil
	}
	return true, updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC())
}

// removeConversationSearchForProjectionsTx removes FTS rowids before their
// membership rows. Contentless FTS has no foreign-key cascade, so this is the
// required deletion path for supersession and garbage collection.
func removeConversationSearchForProjectionsTx(ctx context.Context, tx *sql.Tx, projectionIDs []int64) (bool, error) {
	projectionIDs = uniqueInt64s(projectionIDs)
	changed := false
	for _, projectionID := range projectionIDs {
		rows, err := tx.QueryContext(ctx, `SELECT item_id FROM conversation_search_documents WHERE projection_id = ?`, projectionID)
		if err != nil {
			return false, err
		}
		var itemIDs []string
		for rows.Next() {
			var itemID string
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				return false, err
			}
			itemIDs = append(itemIDs, itemID)
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		for _, itemID := range itemIDs {
			removed, err := removeConversationSearchDocumentTx(ctx, tx, projectionID, itemID)
			if err != nil {
				return false, err
			}
			changed = changed || removed
		}
	}
	return changed, nil
}

func removeConversationSearchForProjectionItemsTx(ctx context.Context, tx *sql.Tx, projectionID int64, itemIDs []string) (bool, error) {
	if projectionID <= 0 || len(itemIDs) == 0 {
		return false, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")
	args := append([]any{projectionID}, stringsToAny(itemIDs)...)
	rows, err := tx.QueryContext(ctx, `SELECT item_id FROM conversation_search_documents WHERE projection_id=? AND item_id IN (`+marks+`)`, args...)
	if err != nil {
		return false, err
	}
	var indexed []string
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			rows.Close()
			return false, err
		}
		indexed = append(indexed, itemID)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, itemID := range indexed {
		if _, err := removeConversationSearchDocumentTx(ctx, tx, projectionID, itemID); err != nil {
			return false, err
		}
	}
	return len(indexed) > 0, nil
}

func updateCollectedConversationSearchStateTx(ctx context.Context, tx *sql.Tx, changed bool) error {
	if !changed {
		return nil
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return err
	}
	if err := updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC()); err != nil {
		return fmt.Errorf("update collected conversation search state: %w", err)
	}
	return nil
}

func upsertConversationSearchEligibleItemTx(ctx context.Context, tx *sql.Tx, modes []ConversationSearchMode, item EligibleConversationSearchItem) (bool, error) {
	var documentID int64
	err := tx.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE projection_id = ? AND item_id = ?`, item.Identity.ProjectionID, item.Identity.ItemID).Scan(&documentID)
	changed := false
	if err == sql.ErrNoRows {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO conversation_search_documents(projection_id, item_id) VALUES (?, ?)`, item.Identity.ProjectionID, item.Identity.ItemID)
		if insertErr != nil {
			return false, insertErr
		}
		documentID, insertErr = result.LastInsertId()
		if insertErr != nil {
			return false, insertErr
		}
		changed = true
	} else if err != nil {
		return false, err
	}
	for _, mode := range modes {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+searchFTSTable(mode)+` WHERE rowid = ?)`, documentID).Scan(&exists); err != nil {
			return false, err
		}
		if exists != 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+searchFTSTable(mode)+`(rowid, text) VALUES (?, ?)`, documentID, item.Text); err != nil {
			return false, fmt.Errorf("insert %s FTS row: %w", mode, err)
		}
		changed = true
	}
	return changed, nil
}

func removeConversationSearchDocumentTx(ctx context.Context, tx *sql.Tx, projectionID int64, itemID string) (bool, error) {
	var documentID int64
	err := tx.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE projection_id = ? AND item_id = ?`, projectionID, itemID).Scan(&documentID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+searchFTSTable(mode)+` WHERE rowid = ?`, documentID); err != nil {
			return false, fmt.Errorf("delete %s FTS row: %w", mode, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_search_documents WHERE document_id = ?`, documentID); err != nil {
		return false, err
	}
	return true, nil
}

func maintainedConversationSearchModes(statuses []ConversationSearchStatus) []ConversationSearchMode {
	var modes []ConversationSearchMode
	for _, status := range statuses {
		if status.State == ConversationSearchBuilding || status.State == ConversationSearchReady {
			modes = append(modes, status.Mode)
		}
	}
	return modes
}

func updateMaintainedConversationSearchStateTx(ctx context.Context, tx *sql.Tx, statuses []ConversationSearchStatus, now time.Time) error {
	var documents int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_search_documents`).Scan(&documents); err != nil {
		return err
	}
	for _, status := range statuses {
		if status.State != ConversationSearchBuilding && status.State != ConversationSearchReady {
			continue
		}
		var indexed int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+searchFTSTable(status.Mode)).Scan(&indexed); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_search_state
SET document_count = ?, indexed_count = ?, revision = revision + 1, updated_at = ?
WHERE mode = ?`, documents, indexed, formatTime(now), status.Mode); err != nil {
			return err
		}
	}
	return nil
}

func uniqueInt64s(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if value > 0 {
			if _, ok := seen[value]; !ok {
				seen[value] = struct{}{}
				result = append(result, value)
			}
		}
	}
	return result
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}
