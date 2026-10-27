package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ParserStateCompactionBatch reports one pass of CompactParserStateBatch.
// parser_retention_checked_at marks a checkpoint the pass has visited: rewritten
// slim, or confirmed as one no current reader can decode.
type ParserStateCompactionBatch struct {
	Projections int
	// Skipped counts checkpoints no current reader can decode; an append
	// rebuilds them, so they are marked visited and left as they are.
	Skipped     int
	BytesBefore int64
	BytesAfter  int64
	Remaining   bool
}

const maxParserCompactionBatch = 64

// CompactParserStateBatch rewrites the next few active checkpoints under the
// reader's current retention rule and marks them checked. A checkpoint that
// an append changed meanwhile is left for a later pass. Nothing but the
// reader's own eviction decides what goes.
func (s *Store) CompactParserStateBatch(ctx context.Context, limit int) (ParserStateCompactionBatch, error) {
	if limit <= 0 || limit > maxParserCompactionBatch {
		limit = maxParserCompactionBatch
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM conversation_projections WHERE state='active' AND parser_retention_checked_at IS NULL ORDER BY id LIMIT ?`, limit+1)
	if err != nil {
		return ParserStateCompactionBatch{}, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ParserStateCompactionBatch{}, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return ParserStateCompactionBatch{}, err
	}
	var batch ParserStateCompactionBatch
	batch.Remaining = len(ids) > limit
	if batch.Remaining {
		ids = ids[:limit]
	}
	for _, id := range ids {
		before, after, err := s.compactParserState(ctx, id)
		if errors.Is(err, ErrTranscriptChanged) {
			if err := s.markParserStateVisited(ctx, id); err != nil {
				return batch, err
			}
			batch.Skipped++
			continue
		}
		if err != nil {
			return batch, fmt.Errorf("compact parser state of projection %d: %w", id, err)
		}
		batch.Projections++
		batch.BytesBefore += before
		batch.BytesAfter += after
	}
	return batch, nil
}

func (s *Store) markParserStateVisited(ctx context.Context, projectionID int64) error {
	release, err := s.backgroundWrite(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = s.db.ExecContext(ctx, `UPDATE conversation_projections SET parser_retention_checked_at=? WHERE id=? AND parser_retention_checked_at IS NULL`, formatTime(time.Now().UTC()), projectionID)
	return err
}

func (s *Store) compactParserState(ctx context.Context, projectionID int64) (before, after int64, err error) {
	var provider, indexedAt string
	var readOffset int64
	var stored []byte
	if err := s.db.QueryRowContext(ctx, `SELECT provider, last_indexed_at, read_offset, parser_state_json FROM conversation_projections WHERE id=? AND state='active'`, projectionID).Scan(&provider, &indexedAt, &readOffset, &stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil // superseded since it was listed
		}
		return 0, 0, err
	}
	entries, err := loadContinuationEntries(ctx, s.db, projectionID)
	if err != nil {
		return 0, 0, err
	}
	before = int64(len(stored))
	for _, entry := range entries {
		before += int64(len(entry.value))
	}
	raw, err := restoreTranscriptContinuation(stored, entries)
	if err != nil {
		return 0, 0, err
	}
	var checkpoint parsedFileCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil || len(checkpoint.Reader) == 0 {
		return 0, 0, fmt.Errorf("decode file continuation: %w", ErrTranscriptChanged)
	}
	reader, err := newModuleFileReader(Provider(provider), checkpoint.Reader)
	if err != nil {
		return 0, 0, fmt.Errorf("restore file reader: %w", ErrTranscriptChanged)
	}
	if err := reader.compact(); err != nil {
		// An older reader version, like an undecodable one: an append rebuilds it.
		return 0, 0, fmt.Errorf("%v: %w", err, ErrTranscriptChanged)
	}
	if checkpoint.Reader, _, _, _, err = reader.save(); err != nil {
		return 0, 0, err
	}
	if raw, err = json.Marshal(checkpoint); err != nil {
		return 0, 0, err
	}
	base, compacted, err := splitTranscriptContinuation(raw)
	if err != nil {
		return 0, 0, err
	}
	after = int64(len(base))
	for _, entry := range compacted {
		after += int64(len(entry.value))
	}
	release, err := s.backgroundWrite(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	// An append since the read wrote its own, already slim, state.
	result, err := tx.ExecContext(ctx, `UPDATE conversation_projections SET parser_state_json=?, parser_retention_checked_at=? WHERE id=? AND state='active' AND last_indexed_at=? AND read_offset=?`,
		base, formatTime(time.Now().UTC()), projectionID, indexedAt, readOffset)
	if err != nil {
		return 0, 0, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return 0, 0, nil
	}
	if err := syncContinuationEntries(ctx, tx, projectionID, compacted); err != nil {
		return 0, 0, err
	}
	return before, after, tx.Commit()
}
