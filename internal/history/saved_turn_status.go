package history

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SavedTurnStatus is the settlement evidence of one saved turn, found by its
// exact native turn id.
type SavedTurnStatus struct {
	Turn TranscriptTurn
	// Newer reports a later saved conversation turn in the same projection.
	Newer bool
}

// ReadSavedTurnStatus reads one turn of a thread's active projection by exact
// native turn id, without reading items or pages. ok is false unless exactly
// one conversation turn has that id; duplicate ids give no evidence. A
// conversation turn holds at least one row other than a provider event, notice
// or compaction, the same rule readTranscriptTurns applies to pages.
func (s *Store) ReadSavedTurnStatus(ctx context.Context, threadID, nativeTurnID string) (SavedTurnStatus, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if threadID == "" || nativeTurnID == "" {
		return SavedTurnStatus{}, false, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SavedTurnStatus{}, false, err
	}
	defer tx.Rollback()
	var projectionID, latestSeq int64
	err = tx.QueryRowContext(ctx, `SELECT p.id, p.latest_seq FROM conversation_projections p JOIN threads t ON t.id=p.thread_id
WHERE p.thread_id=? AND p.state='active' AND t.source_status='present' AND p.error_summary IS NULL`, threadID).Scan(&projectionID, &latestSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedTurnStatus{}, false, nil
	}
	if err != nil {
		return SavedTurnStatus{}, false, err
	}
	rows, err := tx.QueryContext(ctx, savedTurnByNativeIDQuery, projectionID, nativeTurnID)
	if err != nil {
		return SavedTurnStatus{}, false, err
	}
	var found []TranscriptTurn
	for rows.Next() {
		var turn TranscriptTurn
		var started, completed sql.NullString
		var completionSeq, duration sql.NullInt64
		if err := rows.Scan(&turn.NativeTurnID, &turn.Ordinal, &completionSeq, &turn.Status, &started, &completed, &duration); err != nil {
			rows.Close()
			return SavedTurnStatus{}, false, err
		}
		if turn.StartedAt, err = parseNullTime(started); err == nil {
			turn.CompletedAt, err = parseNullTime(completed)
		}
		if err != nil {
			rows.Close()
			return SavedTurnStatus{}, false, err
		}
		turn.CompletionSeq, turn.DurationMS = int64FromNull(completionSeq), int64FromNull(duration)
		// As readTranscriptTurns: a completion beyond the committed prefix is
		// not yet evidence.
		if turn.CompletionSeq == 0 || turn.CompletionSeq > latestSeq {
			turn.Status, turn.CompletionSeq, turn.CompletedAt, turn.DurationMS = TurnInProgress, 0, nil, 0
		}
		found = append(found, turn)
	}
	if err := rows.Close(); err != nil {
		return SavedTurnStatus{}, false, err
	}
	if err := rows.Err(); err != nil {
		return SavedTurnStatus{}, false, err
	}
	if len(found) != 1 {
		return SavedTurnStatus{}, false, nil
	}
	status := SavedTurnStatus{Turn: found[0]}
	err = tx.QueryRowContext(ctx, newerSavedTurnQuery, projectionID, status.Turn.Ordinal).Scan(&status.Newer)
	if err != nil {
		return SavedTurnStatus{}, false, err
	}
	return status, true, nil
}

var (
	savedTurnByNativeIDQuery = `SELECT turn.native_turn_id, turn.ordinal, turn.completion_seq,
       turn.status, turn.started_at, turn.completed_at, turn.duration_ms
FROM conversation_turns turn
WHERE turn.projection_id=? AND turn.native_turn_id=? AND ` + conversationTurnFilter("turn") + `
LIMIT 2`
	// A history-only group (no native id, no parsed turn) holds rows the parser
	// could not place in a turn, such as a Grok background task. It can follow
	// a turn that is still running, so it is not a newer turn. A Grok prompt
	// saved before its id binds is a parsed turn and still counts.
	newerSavedTurnQuery = newerTurnQuery("")
	// newerRootTurnQuery is newerSavedTurnQuery for a Claude root turn. A
	// worker's turns are saved in the root file, as sidechain rows, while the
	// root turn that started them still runs, so only a later turn whose
	// conversation row is itself a root row follows it. A worker's
	// housekeeping rows are stored without its sidechain flag and prove
	// nothing.
	newerRootTurnQuery = newerTurnQuery(" AND semantic.sidechain=0")
)

func newerTurnQuery(rowFilter string) string {
	return `SELECT EXISTS (SELECT 1 FROM conversation_turns later
WHERE later.projection_id=? AND later.ordinal>? AND (later.native_turn_id IS NOT NULL OR later.parsed_json<>'{}') AND ` + conversationRowFilter("later", rowFilter) + `)`
}

func conversationTurnFilter(turn string) string {
	return conversationRowFilter(turn, "")
}

// conversationRowFilter requires a conversation row in turn that also meets
// rowFilter, a condition on that same row.
func conversationRowFilter(turn, rowFilter string) string {
	return `EXISTS (SELECT 1 FROM conversation_items semantic
  WHERE semantic.projection_id=` + turn + `.projection_id AND semantic.turn_key=` + turn + `.turn_key
    AND semantic.projected_seq>=` + turn + `.start_seq AND semantic.projected_seq<=` + turn + `.end_seq
    AND semantic.item_type NOT IN ('provider_event', 'notice', 'compaction')` + rowFilter + `)`
}
