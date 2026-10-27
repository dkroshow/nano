package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

var ErrCompletedTranscriptUnavailable = errors.New("complete transcript turn unavailable")

func (s *Store) ReadCompletedTranscriptTurn(ctx context.Context, threadID, nativeTurnID string) (TranscriptPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if threadID == "" || nativeTurnID == "" {
		return TranscriptPage{}, ErrCompletedTranscriptUnavailable
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TranscriptPage{}, err
	}
	defer tx.Rollback()
	var projectionID int64
	page := TranscriptPage{}
	err = tx.QueryRowContext(ctx, `SELECT p.id, p.projection_key, p.latest_seq FROM conversation_projections p JOIN threads t ON t.id=p.thread_id WHERE p.thread_id=? AND p.state='active' AND t.source_status='present' AND p.error_summary IS NULL`, threadID).Scan(&projectionID, &page.Projection.Key, &page.LatestSeq)
	if err != nil {
		return TranscriptPage{}, errors.Join(ErrCompletedTranscriptUnavailable, err)
	}
	var turnKey string
	var start, end int64
	var count int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(turn_key),''), COALESCE(MIN(start_seq),0), COALESCE(MIN(end_seq),0) FROM conversation_turns WHERE projection_id=? AND native_turn_id=?`, projectionID, nativeTurnID).Scan(&count, &turnKey, &start, &end)
	if err != nil {
		return TranscriptPage{}, err
	}
	if count != 1 || start < 1 || end < start {
		return TranscriptPage{}, ErrCompletedTranscriptUnavailable
	}
	turns, err := readTranscriptTurns(ctx, tx, projectionID, start, end, page.LatestSeq)
	if err != nil {
		return TranscriptPage{}, err
	}
	if len(turns) != 1 || turns[0].Key != turnKey || turns[0].NativeTurnID != nativeTurnID || turns[0].Status != TurnCompleted || turns[0].FinalItemID == "" || turns[0].CompletionSeq == 0 {
		return TranscriptPage{}, ErrCompletedTranscriptUnavailable
	}
	var messages, contentBytes int64
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(CAST(display_text AS BLOB))+length(detail_json)),0) FROM conversation_items WHERE projection_id=? AND turn_key=? AND item_type IN ('user_message','agent_message')`, projectionID, turnKey).Scan(&messages, &contentBytes)
	if err != nil {
		return TranscriptPage{}, err
	}
	if messages < 1 || messages > 100 || contentBytes > 256<<10 {
		return TranscriptPage{}, ErrCompletedTranscriptUnavailable
	}
	rows, err := tx.QueryContext(ctx, `SELECT item_id, projected_seq, source_key, native_item_id, turn_key, turn_ordinal,
item_type, item_status, role, display_text, tool_name, call_id, detail_json, created_at, phase,
filter_tags_json, synthetic, sidechain, is_final_answer, content_ref, content_truncated, original_bytes,
'messages' AS category FROM conversation_items WHERE projection_id=? AND turn_key=? AND item_type IN ('user_message','agent_message') ORDER BY projected_seq LIMIT 101`, projectionID, turnKey)
	if err != nil {
		return TranscriptPage{}, err
	}
	seen := make(map[string]bool)
	finals, usedBytes := 0, 0
	for rows.Next() {
		item, scanErr := scanTranscriptItem(rows)
		if scanErr != nil {
			rows.Close()
			return TranscriptPage{}, scanErr
		}
		encoded, encodeErr := json.Marshal(item)
		if encodeErr != nil {
			rows.Close()
			return TranscriptPage{}, encodeErr
		}
		usedBytes += len(encoded)
		if len(page.Items) == 100 || usedBytes > 256<<10 || item.ContentTruncated || (item.NativeItemID != "" && seen[item.NativeItemID]) {
			rows.Close()
			return TranscriptPage{}, ErrCompletedTranscriptUnavailable
		}
		if item.NativeItemID != "" {
			seen[item.NativeItemID] = true
		}
		if item.ID == turns[0].FinalItemID {
			if item.Type != ItemAgentMessage || item.Text == "" {
				rows.Close()
				return TranscriptPage{}, ErrCompletedTranscriptUnavailable
			}
			item.IsFinalAnswer = true
			finals++
		}
		page.Items = append(page.Items, item)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return TranscriptPage{}, errors.Join(err, closeErr)
	}
	if finals != 1 {
		return TranscriptPage{}, ErrCompletedTranscriptUnavailable
	}
	page.Turns = turns
	page.StartSeq, page.EndSeq, page.SnapshotHeadSeq = start, end, page.LatestSeq
	page.Projection.State = TranscriptReady
	page.Projection.SourceStatus = SourcePresent
	page.Projection.EvidenceAvailable = true
	if err := tx.Commit(); err != nil {
		return TranscriptPage{}, err
	}
	var currentKey string
	if err := s.db.QueryRowContext(ctx, `SELECT projection_key FROM conversation_projections WHERE thread_id=? AND state='active'`, threadID).Scan(&currentKey); err != nil {
		return TranscriptPage{}, err
	}
	if currentKey != page.Projection.Key {
		return TranscriptPage{}, ErrTranscriptChanged
	}
	return page, nil
}

func readActiveTranscriptCheckpointTx(ctx context.Context, tx *sql.Tx, threadID string, includeState bool) (TranscriptCheckpoint, int64, error) {
	var checkpoint TranscriptCheckpoint
	var projectionID int64
	var parserState []byte
	var errorSummary sql.NullString
	stateColumn := "NULL"
	if includeState {
		stateColumn = "parser_state_json"
	}
	err := tx.QueryRowContext(ctx, `
SELECT id, projection_key, thread_id, source_file_id, source_generation, provider,
       parser_version, completion_rule_version, complete_offset, complete_lines,
       latest_seq, `+stateColumn+`, error_summary, COALESCE(NULLIF(read_offset,0),complete_offset)
FROM conversation_projections WHERE thread_id = ? AND state = 'active'`, threadID).Scan(
		&projectionID, &checkpoint.ProjectionKey, &checkpoint.ThreadID, &checkpoint.SourceID,
		&checkpoint.SourceGeneration, &checkpoint.Provider, &checkpoint.ParserVersion,
		&checkpoint.CompletionRuleVersion, &checkpoint.CompleteOffset,
		&checkpoint.CompleteLines, &checkpoint.LatestSeq, &parserState, &errorSummary, &checkpoint.ReadOffset)
	checkpoint.ParserState = parserState
	checkpoint.ErrorSummary = pointerFromNullString(errorSummary)
	return checkpoint, projectionID, err
}

func (s *Store) LoadTranscriptCheckpoint(ctx context.Context, threadID string) (TranscriptCheckpoint, bool, error) {
	return s.loadTranscriptCheckpoint(ctx, threadID, true)
}

// LoadTranscriptCheckpointHeader omits the potentially large continuation body.
// Scan planning needs only these guards; eligible appends load the body later.
func (s *Store) LoadTranscriptCheckpointHeader(ctx context.Context, threadID string) (TranscriptCheckpoint, bool, error) {
	return s.loadTranscriptCheckpoint(ctx, threadID, false)
}

func (s *Store) loadTranscriptCheckpoint(ctx context.Context, threadID string, includeState bool) (TranscriptCheckpoint, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TranscriptCheckpoint{}, false, err
	}
	defer tx.Rollback()
	checkpoint, projectionID, err := readActiveTranscriptCheckpointTx(ctx, tx, threadID, includeState)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if threadErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&exists); threadErr != nil {
			return TranscriptCheckpoint{}, false, threadErr
		}
		if exists == 0 {
			return TranscriptCheckpoint{}, false, ErrThreadNotFound
		}
		return TranscriptCheckpoint{}, false, nil
	}
	if err != nil {
		return TranscriptCheckpoint{}, false, err
	}
	if includeState {
		entries, err := loadContinuationEntries(ctx, tx, projectionID)
		if err != nil {
			return TranscriptCheckpoint{}, false, err
		}
		checkpoint.ParserState, err = restoreTranscriptContinuation(checkpoint.ParserState, entries)
		if err != nil {
			return TranscriptCheckpoint{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return TranscriptCheckpoint{}, false, err
	}
	return checkpoint, true, nil
}

// MarkTranscriptError records transcript-specific freshness without changing
// the last good projection, its parser state, or its append checkpoint.
func (s *Store) MarkTranscriptError(ctx context.Context, threadID string, sourceID int64, summary string, observedAt time.Time) error {
	if threadID == "" || sourceID <= 0 || strings.TrimSpace(summary) == "" {
		return errors.New("transcript error requires thread, source, and summary")
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
UPDATE conversation_projections SET error_summary = ?, last_indexed_at = ?
WHERE thread_id = ? AND source_file_id = ? AND state = 'active'`,
		strings.TrimSpace(summary), formatTime(observedAt), threadID, sourceID)
	if err != nil {
		return err
	}
	changed, err := resultChanged(result)
	if err != nil {
		return err
	}
	if !changed {
		_, ok, checkpointErr := s.LoadTranscriptCheckpoint(ctx, threadID)
		if checkpointErr != nil {
			return checkpointErr
		}
		if ok {
			return ErrTranscriptConflict
		}
	}
	return nil
}

func createTranscriptProjectionTx(ctx context.Context, tx *sql.Tx, input TranscriptApplyRequest, parserState []byte, parserStateEntries []continuationEntry) (int64, error) {
	result, err := tx.ExecContext(ctx, `
INSERT INTO conversation_projections(
  projection_key, thread_id, source_file_id, source_generation, provider,
  parser_version, completion_rule_version, schema_version, taxonomy_version,
  state, complete_offset, complete_lines, parser_state_json, item_count,
  latest_seq, last_indexed_at, read_offset)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'building', ?, ?, ?, 0, 0, ?, ?)`,
		input.Result.ProjectionKey, input.ThreadID, input.SourceID, input.SourceGeneration,
		input.Provider, input.ParserVersion, input.CompletionRuleVersion,
		TranscriptSchemaVersion, TranscriptTaxonomyV1, input.Result.CompleteOffset,
		input.Result.CompleteLines, parserState, formatTime(input.IndexedAt), input.Result.ReadOffset)
	if err != nil {
		return 0, fmt.Errorf("create transcript shadow projection: %w", err)
	}
	projectionID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := syncContinuationEntries(ctx, tx, projectionID, parserStateEntries); err != nil {
		return 0, err
	}
	return projectionID, nil
}

func (s *Store) ApplyTranscript(ctx context.Context, input TranscriptApplyRequest) (TranscriptApplyResult, error) {
	if input.Result.ReadOffset == 0 {
		input.Result.ReadOffset = input.Result.CompleteOffset
	}
	if err := validateTranscriptApply(input); err != nil {
		return TranscriptApplyResult{}, err
	}
	parserState, parserStateEntries, err := splitTranscriptContinuation(input.Result.ParserState)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	defer tx.Rollback()

	var generation int64
	ctx = context.WithValue(ctx, transcriptValueCacheKey{}, &transcriptValueCache{ids: make(map[string]int64)})
	var provider Provider
	err = tx.QueryRowContext(ctx, `
SELECT sf.generation, sr.provider
FROM thread_sources ts
JOIN source_files sf ON sf.id = ts.source_file_id
JOIN source_roots sr ON sr.id = sf.source_root_id
WHERE ts.thread_id = ? AND sf.id = ?`, input.ThreadID, input.SourceID).Scan(&generation, &provider)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if checkErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, input.ThreadID).Scan(&exists); checkErr != nil {
			return TranscriptApplyResult{}, checkErr
		}
		if exists == 0 {
			return TranscriptApplyResult{}, ErrThreadNotFound
		}
		return TranscriptApplyResult{}, ErrTranscriptConflict
	}
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	if generation != input.SourceGeneration || provider != input.Provider {
		return TranscriptApplyResult{}, ErrTranscriptChanged
	}
	var canonicalSourceID int64
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(
  (SELECT cp.source_file_id FROM conversation_projections cp
   JOIN source_files active_sf ON active_sf.id = cp.source_file_id
   WHERE cp.thread_id = ? AND cp.state = 'active'
     AND active_sf.status = 'present' LIMIT 1),
  (SELECT MIN(ts.source_file_id) FROM thread_sources ts
   JOIN source_files present_sf ON present_sf.id = ts.source_file_id
   WHERE ts.thread_id = ? AND present_sf.status = 'present'),
  (SELECT source_file_id FROM conversation_projections
   WHERE thread_id = ? AND state = 'active' LIMIT 1),
  (SELECT MIN(source_file_id) FROM thread_sources WHERE thread_id = ?)
)`, input.ThreadID, input.ThreadID, input.ThreadID, input.ThreadID).Scan(&canonicalSourceID); err != nil {
		return TranscriptApplyResult{}, err
	}
	if canonicalSourceID != input.SourceID {
		return TranscriptApplyResult{}, ErrTranscriptConflict
	}
	if input.IndexedAt.IsZero() {
		input.IndexedAt = time.Now().UTC()
	}

	var projectionID int64
	var oldKey, oldParser, oldCompletion string
	var oldGeneration, oldOffset, oldReadOffset, oldLines, oldLatest, oldCount int64
	activeErr := tx.QueryRowContext(ctx, `
SELECT id, projection_key, source_generation, parser_version,
       completion_rule_version, complete_offset, complete_lines, latest_seq, item_count,
       COALESCE(NULLIF(read_offset,0),complete_offset)
FROM conversation_projections WHERE thread_id = ? AND state = 'active'`, input.ThreadID).Scan(
		&projectionID, &oldKey, &oldGeneration, &oldParser, &oldCompletion,
		&oldOffset, &oldLines, &oldLatest, &oldCount, &oldReadOffset)
	if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
		return TranscriptApplyResult{}, activeErr
	}
	if input.ExpectNoProjection && activeErr == nil {
		return TranscriptApplyResult{}, ErrTranscriptChanged
	}
	activeProjectionID := projectionID

	if input.Mode == TranscriptAppend {
		if errors.Is(activeErr, sql.ErrNoRows) {
			return TranscriptApplyResult{}, ErrTranscriptChanged
		}
		if input.ExpectedProjectionKey != "" && input.ExpectedProjectionKey != oldKey {
			return TranscriptApplyResult{}, ErrTranscriptChanged
		}
		if input.Result.ProjectionKey != oldKey || oldGeneration != input.SourceGeneration ||
			oldParser != input.ParserVersion || oldCompletion != input.CompletionRuleVersion {
			return TranscriptApplyResult{}, ErrTranscriptChanged
		}
		if input.Result.ReadOffset <= oldReadOffset {
			return TranscriptApplyResult{ProjectionKey: oldKey, LatestSeq: oldLatest, ItemCount: oldCount}, nil
		}
		if input.Result.CompleteLines < int(oldLines) || input.Result.CompleteOffset < oldOffset {
			return TranscriptApplyResult{}, ErrTranscriptChanged
		}
		if err := validateDenseItems(input.Result.Items, oldLatest); err != nil {
			return TranscriptApplyResult{}, err
		}
		if err := writeTranscriptResultTx(ctx, tx, projectionID, input.Result); err != nil {
			return TranscriptApplyResult{}, err
		}
		count, latest, err := transcriptCounts(ctx, tx, projectionID)
		if err != nil {
			return TranscriptApplyResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_projections SET complete_offset = ?, complete_lines = ?,
  parser_state_json = ?, item_count = ?, latest_seq = ?, last_indexed_at = ?, error_summary = NULL, read_offset = ?
WHERE id = ?`, input.Result.CompleteOffset, input.Result.CompleteLines,
			parserState, count, latest, formatTime(input.IndexedAt), input.Result.ReadOffset, projectionID); err != nil {
			return TranscriptApplyResult{}, err
		}
		if err := syncContinuationEntries(ctx, tx, projectionID, parserStateEntries); err != nil {
			return TranscriptApplyResult{}, err
		}
		if _, err := maintainConversationSearchForProjectionSequenceRangeTx(ctx, tx, projectionID, oldLatest, latest); err != nil {
			return TranscriptApplyResult{}, fmt.Errorf("maintain appended conversation search rows: %w", err)
		}
		if input.Provider == ProviderGrok {
			changed, err := syncGrokThreadRecency(ctx, tx, input.ThreadID)
			if err != nil {
				return TranscriptApplyResult{}, err
			}
			if changed {
				if _, err := bumpCatalogVersion(ctx, tx); err != nil {
					return TranscriptApplyResult{}, err
				}
			}
		}
		if err := tx.Commit(); err != nil {
			return TranscriptApplyResult{}, err
		}
		return TranscriptApplyResult{ProjectionKey: oldKey, LatestSeq: latest, ItemCount: count, Changed: true}, nil
	}

	if input.ExpectedProjectionKey != "" && !errors.Is(activeErr, sql.ErrNoRows) && input.ExpectedProjectionKey != oldKey {
		return TranscriptApplyResult{}, ErrTranscriptChanged
	}
	if input.Result.ProjectionKey == "" || input.Result.CompleteOffset < 0 || input.Result.CompleteLines < 0 {
		return TranscriptApplyResult{}, ErrInvalidTranscriptPage
	}
	if err := validateDenseItems(input.Result.Items, 0); err != nil {
		return TranscriptApplyResult{}, err
	}
	projectionID, err = createTranscriptProjectionTx(ctx, tx, input, parserState, parserStateEntries)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	if err := writeTranscriptResultTx(ctx, tx, projectionID, input.Result); err != nil {
		return TranscriptApplyResult{}, err
	}
	if !errors.Is(activeErr, sql.ErrNoRows) {
		if err := recordTranscriptItemAliases(ctx, tx, input.ThreadID, activeProjectionID, projectionID); err != nil {
			return TranscriptApplyResult{}, err
		}
	}
	count, latest, err := transcriptCounts(ctx, tx, projectionID)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	if !errors.Is(activeErr, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_projections SET state = 'superseded' WHERE id = ?`, activeProjectionID); err != nil {
			return TranscriptApplyResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE conversation_projections SET state = 'active', item_count = ?, latest_seq = ? WHERE id = ?`, count, latest, projectionID); err != nil {
		return TranscriptApplyResult{}, err
	}
	searchChanged := false
	if !errors.Is(activeErr, sql.ErrNoRows) {
		removed, err := removeConversationSearchForProjectionsTx(ctx, tx, []int64{activeProjectionID})
		if err != nil {
			return TranscriptApplyResult{}, fmt.Errorf("remove superseded conversation search rows: %w", err)
		}
		searchChanged = searchChanged || removed
	}
	updated, err := maintainConversationSearchForProjectionsTx(ctx, tx, []int64{projectionID})
	if err != nil {
		return TranscriptApplyResult{}, fmt.Errorf("maintain rebuilt conversation search rows: %w", err)
	}
	searchChanged = searchChanged || updated
	if input.Provider == ProviderGrok {
		changed, err := syncGrokThreadRecency(ctx, tx, input.ThreadID)
		if err != nil {
			return TranscriptApplyResult{}, err
		}
		if changed {
			if _, err := bumpCatalogVersion(ctx, tx); err != nil {
				return TranscriptApplyResult{}, err
			}
		}
	}
	if searchChanged && !updated {
		statuses, err := conversationSearchStatusesTx(ctx, tx)
		if err != nil {
			return TranscriptApplyResult{}, err
		}
		if err := updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC()); err != nil {
			return TranscriptApplyResult{}, fmt.Errorf("update rebuilt conversation search state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return TranscriptApplyResult{}, err
	}
	return TranscriptApplyResult{ProjectionKey: input.Result.ProjectionKey, LatestSeq: latest, ItemCount: count, Changed: true}, nil
}

// syncGrokThreadRecency makes the normalized conversation the authority for
// Grok's sidebar timestamp. Grok rewrites summary.updated_at and last_active_at
// for reconnect hooks and other provider housekeeping, so those fields cannot
// answer the operator-facing question "when did someone last say something?"
// A message-free session keeps the metadata fallback.
func syncGrokThreadRecency(ctx context.Context, tx *sql.Tx, threadID string) (bool, error) {
	latest, err := grokThreadConversationRecency(ctx, tx, threadID)
	if err != nil {
		return false, err
	}
	if latest == nil {
		return false, nil
	}
	formatted := formatTime(*latest)
	result, err := tx.ExecContext(ctx, `
UPDATE threads SET updated_at = ?
WHERE id = ? AND (updated_at IS NULL OR updated_at <> ?)`, formatted, threadID, formatted)
	if err != nil {
		return false, fmt.Errorf("update Grok conversation recency: %w", err)
	}
	return resultChanged(result)
}

func grokThreadConversationRecency(ctx context.Context, tx *sql.Tx, threadID string) (*time.Time, error) {
	var latest sql.NullString
	err := tx.QueryRowContext(ctx, `
SELECT MAX(ci.created_at)
FROM conversation_projections cp
JOIN conversation_items ci ON ci.projection_id = cp.id
WHERE cp.thread_id = ? AND cp.state = 'active'
  AND ci.item_type IN (?, ?) AND ci.sidechain = 0`,
		threadID, ItemUserMessage, ItemAgentMessage).Scan(&latest)
	if err != nil {
		return nil, fmt.Errorf("read Grok conversation recency: %w", err)
	}
	if !latest.Valid || strings.TrimSpace(latest.String) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, latest.String)
	if err != nil {
		return nil, fmt.Errorf("parse Grok conversation recency: %w", err)
	}
	return &parsed, nil
}

const (
	maxSupersededProjectionBatch = 64
	// Provisional; set from the active-thread copy test.
	maxSupersededProjectionBatchItems = 1_000
)

// SupersededProjectionBatch reports one independently committed collection
// unit. Remaining may be conservatively true when the batch ended exactly at
// its limit; the next call then confirms whether work remains.
type SupersededProjectionBatch struct {
	Collected int
	Items     int64
	Progress  []SupersededProjectionProgress
	Remaining bool
}

type SupersededProjectionProgress struct {
	ProjectionID int64
	Tables       []SupersededProjectionTableRows
}

type SupersededProjectionTableRows struct {
	Table string
	Rows  int64
}

func (p SupersededProjectionProgress) RowsSummary() string {
	parts := make([]string, 0, len(p.Tables))
	for _, table := range p.Tables {
		parts = append(parts, fmt.Sprintf("%s:%d", table.Table, table.Rows))
	}
	return strings.Join(parts, ",")
}

var supersededProjectionTables = []string{
	"conversation_parser_state_entries",
	"conversation_item_observations",
	"conversation_turn_observations",
	"conversation_item_bodies",
	"conversation_search_documents",
	"conversation_parse_diagnostics",
	"conversation_relations",
	"conversation_parse_identities",
	"conversation_observation_aliases",
	"conversation_turn_aliases",
	"conversation_observations",
	"conversation_items",
	"conversation_turns",
	"conversation_parse_values",
}

const supersededProjectionEligibility = `
(older.state = 'abandoned' OR (older.state = 'superseded'
AND EXISTS (
  SELECT 1 FROM conversation_projections active
  WHERE active.thread_id = older.thread_id
    AND active.state = 'active'
    AND active.id > older.id
)))`

type supersededProjectionCandidate struct {
	id     int64
	items  int64
	thread string
}

// CollectSupersededProjectionsBatch deletes the oldest eligible generations.
// A normal call is capped by item count. One oversized generation may run by
// itself, with its rows split across short commits that release writeMu.
func (s *Store) CollectSupersededProjectionsBatch(ctx context.Context, limit int) (SupersededProjectionBatch, error) {
	if limit <= 0 || limit > maxSupersededProjectionBatch {
		limit = maxSupersededProjectionBatch
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT older.id, older.thread_id, older.item_count
FROM conversation_projections older
WHERE `+supersededProjectionEligibility+`
ORDER BY older.id ASC
LIMIT ?`, limit)
	if err != nil {
		return SupersededProjectionBatch{}, fmt.Errorf("list superseded projections: %w", err)
	}
	var candidates []supersededProjectionCandidate
	for rows.Next() {
		var row supersededProjectionCandidate
		if err := rows.Scan(&row.id, &row.thread, &row.items); err != nil {
			rows.Close()
			return SupersededProjectionBatch{}, err
		}
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return SupersededProjectionBatch{}, err
	}
	if err := rows.Close(); err != nil {
		return SupersededProjectionBatch{}, err
	}
	selected := make([]supersededProjectionCandidate, 0, len(candidates))
	var selectedItems int64
	for _, candidate := range candidates {
		if len(selected) > 0 && selectedItems+candidate.items > maxSupersededProjectionBatchItems {
			break
		}
		selected = append(selected, candidate)
		selectedItems += candidate.items
		if selectedItems >= maxSupersededProjectionBatchItems {
			break
		}
	}
	var batch SupersededProjectionBatch
	batch.Remaining = len(candidates) > 0
	for _, candidate := range selected {
		items, collected, progress, err := s.collectSupersededProjection(ctx, candidate)
		batch.Items += items
		batch.Progress = append(batch.Progress, progress)
		if err != nil {
			return batch, err
		}
		if collected {
			batch.Collected++
		}
	}
	return batch, nil
}

func (s *Store) collectSupersededProjection(ctx context.Context, candidate supersededProjectionCandidate) (int64, bool, SupersededProjectionProgress, error) {
	progress := SupersededProjectionProgress{ProjectionID: candidate.id}
	var items int64
	for _, table := range supersededProjectionTables {
		var tableRows int64
		for {
			removed, eligible, err := s.collectSupersededProjectionTableBatch(ctx, candidate, table)
			tableRows += removed
			if table == "conversation_items" {
				items += removed
			}
			if err != nil || !eligible {
				progress.Tables = append(progress.Tables, SupersededProjectionTableRows{Table: table, Rows: tableRows})
				return items, false, progress, err
			}
			if removed == 0 {
				break
			}
		}
		progress.Tables = append(progress.Tables, SupersededProjectionTableRows{Table: table, Rows: tableRows})
	}

	removed, eligible, err := s.supersededTx(ctx, candidate.id, func(tx *sql.Tx) (int64, error) {
		result, err := tx.ExecContext(ctx, `DELETE FROM conversation_projections WHERE id=?`, candidate.id)
		if err != nil {
			return 0, fmt.Errorf("collect superseded projection %d: %w", candidate.id, err)
		}
		return result.RowsAffected()
	})
	progress.Tables = append(progress.Tables, SupersededProjectionTableRows{Table: "conversation_projections", Rows: removed})
	return items, eligible && removed > 0, progress, err
}

func (s *Store) collectSupersededProjectionItemBatch(ctx context.Context, candidate supersededProjectionCandidate) (int64, bool, error) {
	return s.supersededTx(ctx, candidate.id, func(tx *sql.Tx) (int64, error) {
		rows, err := tx.QueryContext(ctx, `SELECT item_id FROM conversation_items WHERE projection_id=? ORDER BY projected_seq LIMIT ?`, candidate.id, maxSupersededProjectionBatchItems)
		if err != nil {
			return 0, err
		}
		var itemIDs []string
		for rows.Next() {
			var itemID string
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				return 0, err
			}
			itemIDs = append(itemIDs, itemID)
		}
		if err := rows.Close(); err != nil || len(itemIDs) == 0 {
			return 0, err
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")
		// Canonical item ids are stable across generations, so the same row
		// usually lives on in another generation of the thread and its
		// aliases still resolve. Collect only aliases whose
		// canonical row exists in no other generation.
		others, err := otherThreadProjectionIDs(ctx, tx, candidate.thread, candidate.id)
		if err != nil {
			return 0, err
		}
		aliasQuery := `DELETE FROM conversation_item_aliases WHERE thread_id=? AND canonical_item_id IN (` + marks + `)`
		aliasArgs := append([]any{candidate.thread}, stringsToAny(itemIDs)...)
		if len(others) > 0 {
			otherMarks := strings.TrimSuffix(strings.Repeat("?,", len(others)), ",")
			aliasQuery += ` AND NOT EXISTS (SELECT 1 FROM conversation_items other
WHERE other.projection_id IN (` + otherMarks + `) AND other.item_id=conversation_item_aliases.canonical_item_id)`
			aliasArgs = append(aliasArgs, others...)
		}
		if _, err := tx.ExecContext(ctx, aliasQuery, aliasArgs...); err != nil {
			return 0, err
		}
		itemArgs := append([]any{candidate.id}, stringsToAny(itemIDs)...)
		result, err := tx.ExecContext(ctx, `DELETE FROM conversation_items WHERE projection_id=? AND item_id IN (`+marks+`)`, itemArgs...)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	})
}

// otherThreadProjectionIDs lists the thread's generations other than the one
// being collected.
func otherThreadProjectionIDs(ctx context.Context, tx *sql.Tx, threadID string, collecting int64) ([]any, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM conversation_projections WHERE thread_id=? AND id!=?`, threadID, collecting)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) collectSupersededProjectionTableBatch(ctx context.Context, candidate supersededProjectionCandidate, table string) (int64, bool, error) {
	if table == "conversation_items" {
		return s.collectSupersededProjectionItemBatch(ctx, candidate)
	}
	if table == "conversation_search_documents" {
		return s.collectSupersededProjectionSearchBatch(ctx, candidate)
	}
	return s.supersededTx(ctx, candidate.id, func(tx *sql.Tx) (int64, error) {
		statement := fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE projection_id=? LIMIT ?)`, table, table)
		result, err := tx.ExecContext(ctx, statement, candidate.id, maxSupersededProjectionBatchItems)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	})
}

func (s *Store) collectSupersededProjectionSearchBatch(ctx context.Context, candidate supersededProjectionCandidate) (int64, bool, error) {
	return s.supersededTx(ctx, candidate.id, func(tx *sql.Tx) (int64, error) {
		rows, err := tx.QueryContext(ctx, `SELECT item_id FROM conversation_search_documents WHERE projection_id=? LIMIT ?`, candidate.id, maxSupersededProjectionBatchItems)
		if err != nil {
			return 0, err
		}
		var itemIDs []string
		for rows.Next() {
			var itemID string
			if err := rows.Scan(&itemID); err != nil {
				rows.Close()
				return 0, err
			}
			itemIDs = append(itemIDs, itemID)
		}
		if err := rows.Close(); err != nil || len(itemIDs) == 0 {
			return 0, err
		}
		changed, err := removeConversationSearchForProjectionItemsTx(ctx, tx, candidate.id, itemIDs)
		if err != nil {
			return 0, err
		}
		if err := updateCollectedConversationSearchStateTx(ctx, tx, changed); err != nil {
			return 0, err
		}
		return int64(len(itemIDs)), nil
	})
}

func (s *Store) supersededTx(ctx context.Context, projectionID int64, work func(*sql.Tx) (int64, error)) (removed int64, eligible bool, err error) {
	release, err := s.backgroundWrite(ctx)
	if err != nil {
		return 0, false, err
	}
	defer release()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Every Store opener enables foreign keys, so restore that shared invariant
		// before this connection returns to its pool.
		if _, restoreErr := conn.ExecContext(restoreCtx, `PRAGMA foreign_keys=ON`); err == nil && restoreErr != nil {
			err = fmt.Errorf("restore collection foreign keys: %w", restoreErr)
		}
		if closeErr := conn.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return 0, false, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	eligible, err = supersededProjectionEligibleTx(ctx, tx, projectionID)
	if err != nil || !eligible {
		return 0, eligible, err
	}
	removed, err = work(tx)
	if err != nil {
		return 0, eligible, err
	}
	if err := tx.Commit(); err != nil {
		return 0, eligible, err
	}
	return removed, eligible, nil
}

func supersededProjectionEligibleTx(ctx context.Context, tx *sql.Tx, projectionID int64) (bool, error) {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM conversation_projections older
WHERE older.id=? AND `+supersededProjectionEligibility+`)`, projectionID).Scan(&eligible)
	return eligible, err
}

func validateTranscriptApply(input TranscriptApplyRequest) error {
	if input.Result.ReadOffset < input.Result.CompleteOffset || input.Result.CompleteOffset < 0 || input.Result.CompleteLines < 0 {
		return ErrInvalidTranscriptPage
	}
	if input.Result.parsedBatches != nil && (len(input.Result.Items) != 0 || len(input.Result.Turns) != 0 || len(input.Result.Bodies) != 0 || len(input.Result.ItemPatches) != 0) {
		return errors.New("transcript apply cannot mix parsed batches with legacy rows")
	}
	if input.Mode != TranscriptAppend && input.Mode != TranscriptRebuild {
		return errors.New("invalid transcript projection mode")
	}
	if input.ThreadID == "" || input.SourceID <= 0 || input.SourceGeneration <= 0 {
		return errors.New("transcript projection requires thread, source, and generation")
	}
	if input.Provider != ProviderCodex && input.Provider != ProviderClaude && input.Provider != ProviderGrok {
		return errors.New("invalid transcript provider")
	}
	if strings.TrimSpace(input.ParserVersion) == "" || strings.TrimSpace(input.CompletionRuleVersion) == "" {
		return errors.New("transcript projection requires parser versions")
	}
	return nil
}

func validateDenseItems(items []TranscriptItem, prior int64) error {
	expected := prior + 1
	for _, item := range items {
		if item.ProjectedSeq != expected || item.ID == "" || item.SourceKey == "" || item.TurnKey == "" || len(item.SourceLineSHA256) != sha256.Size*2 {
			return fmt.Errorf("transcript items are not dense at sequence %d", expected)
		}
		expected++
	}
	return nil
}

func writeTranscriptBatch(ctx context.Context, tx *sql.Tx, projectionID int64, result TranscriptProjectionResult) error {
	for _, turn := range result.Turns {
		config := turn.Config
		if len(config) == 0 {
			config = json.RawMessage(`{}`)
		}
		usageInput, usageCached, usageOutput, usageReasoning, usageTotal, contextWindow, usageCacheRead, usageCacheWrite, usageNative, usageContract := turnUsageColumns(turn.Usage)
		_, err := tx.ExecContext(ctx, `
INSERT INTO conversation_turns(
  projection_id, turn_key, native_turn_id, ordinal, start_seq, end_seq,
  completion_seq, status, completion_evidence, final_item_id, finality_source,
  finality_confidence, started_at, completed_at, duration_ms, config_json,
  usage_input, usage_cached, usage_output, usage_reasoning, usage_total, context_window,
  usage_cache_read, usage_cache_write, usage_native_json, usage_contract, usage_model, usage_effort, usage_service_tier)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(projection_id, turn_key) DO UPDATE SET
  native_turn_id = excluded.native_turn_id, ordinal = excluded.ordinal,
  start_seq = excluded.start_seq, end_seq = excluded.end_seq,
  completion_seq = excluded.completion_seq, status = excluded.status,
  completion_evidence = excluded.completion_evidence,
  final_item_id = excluded.final_item_id, finality_source = excluded.finality_source,
  finality_confidence = excluded.finality_confidence, started_at = excluded.started_at,
  completed_at = excluded.completed_at, duration_ms = excluded.duration_ms,
  config_json = excluded.config_json, usage_input = excluded.usage_input,
  usage_cached = excluded.usage_cached, usage_output = excluded.usage_output,
  usage_reasoning = excluded.usage_reasoning, usage_total = excluded.usage_total,
  context_window = excluded.context_window, usage_cache_read = excluded.usage_cache_read,
  usage_cache_write = excluded.usage_cache_write, usage_native_json = excluded.usage_native_json,
	usage_contract = excluded.usage_contract, usage_model = excluded.usage_model,
	usage_effort = excluded.usage_effort, usage_service_tier = excluded.usage_service_tier`, projectionID, turn.Key,
			nullableText(turn.NativeTurnID), turn.Ordinal, turn.StartSeq, turn.EndSeq,
			nullablePositive(turn.CompletionSeq), turn.Status, turn.CompletionEvidence,
			nullableText(turn.FinalItemID), nullableText(turn.FinalitySource),
			nullableText(turn.FinalityConfidence), nullableTime(turn.StartedAt),
			nullableTime(turn.CompletedAt), nullablePositive(turn.DurationMS), []byte(config),
			usageInput, usageCached, usageOutput, usageReasoning, usageTotal, contextWindow,
			usageCacheRead, usageCacheWrite, usageNative, usageContract,
			nullableText(turn.Model), nullableText(turn.Effort), nullableText(turn.ServiceTier))
		if err != nil {
			return fmt.Errorf("write transcript turn: %w", err)
		}
	}
	for _, item := range result.Items {
		detail := item.Detail
		if len(detail) == 0 {
			detail = json.RawMessage(`{}`)
		}
		tags, _ := json.Marshal(item.FilterTags)
		_, err := tx.ExecContext(ctx, `
INSERT INTO conversation_items(
  projection_id, item_id, projected_seq, source_key, native_item_id, turn_key,
  turn_ordinal, item_type, item_status, role, display_text, tool_name, call_id,
  detail_json, created_at, phase, filter_tags_json, synthetic, sidechain,
  is_final_answer, content_ref, content_truncated, original_bytes, source_line,
  source_row_type, source_payload_type, source_line_sha256, emission_index)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			projectionID, item.ID, item.ProjectedSeq, item.SourceKey,
			nullableText(item.NativeItemID), item.TurnKey, item.TurnOrdinal,
			item.Type, item.Status, nullableText(string(item.Role)), item.Text,
			nullableText(item.ToolName), nullableText(item.CallID), []byte(detail),
			nullableTime(item.CreatedAt), nullableText(item.Phase), tags,
			item.Synthetic, item.Sidechain, item.IsFinalAnswer,
			nullableText(item.ContentRef), item.ContentTruncated, item.OriginalBytes,
			item.SourceLine, item.SourceRowType, nullableText(item.SourcePayloadType),
			item.SourceLineSHA256, item.EmissionIndex)
		if err != nil {
			return fmt.Errorf("write transcript item: %w", err)
		}
	}
	for _, body := range result.Bodies {
		_, err := tx.ExecContext(ctx, `
INSERT INTO conversation_item_bodies(
  projection_id, item_id, content_ref, body_text, body_json, original_bytes,
  stored_bytes, truncated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, projectionID,
			body.ItemID, body.ContentRef, nullableText(body.Text), nullableBytes(body.JSON),
			body.OriginalBytes, body.StoredBytes, body.Truncated)
		if err != nil {
			return fmt.Errorf("write transcript body: %w", err)
		}
	}
	for _, patch := range result.ItemPatches {
		if patch.ItemID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_items SET
  native_item_id = COALESCE(?, native_item_id),
  phase = COALESCE(?, phase)
WHERE projection_id = ? AND item_id = ?`, nullableText(patch.NativeItemID),
			nullableText(patch.Phase), projectionID, patch.ItemID); err != nil {
			return fmt.Errorf("promote canonical transcript item: %w", err)
		}
	}
	for _, turn := range result.Turns {
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_items SET is_final_answer = CASE WHEN item_id = ? THEN 1 ELSE 0 END
WHERE projection_id = ? AND turn_key = ?`, nullableText(turn.FinalItemID), projectionID, turn.Key); err != nil {
			return err
		}
	}
	return nil
}

func transcriptCounts(ctx context.Context, tx *sql.Tx, projectionID int64) (count, latest int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(projected_seq), 0) FROM conversation_items WHERE projection_id = ?`, projectionID).Scan(&count, &latest)
	return
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

// turnUsageColumns flattens the optional per-turn usage record. A nil field
// persists as SQL NULL. New parser writes always stamp usage_contract = 1 so
// the view can hide leftover v7 cumulative/folded numbers.
func turnUsageColumns(usage *TranscriptTokenUsage) (input, cached, output, reasoning, total, contextWindow, cacheRead, cacheWrite, native, contract any) {
	contract = UsageContractPerTurnV1
	if usage == nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, []byte("{}"), contract
	}
	nativeJSON := usage.Native
	if len(nativeJSON) == 0 {
		nativeJSON = json.RawMessage(`{}`)
	}
	cachedValue := usage.Cached
	if cachedValue == nil {
		cachedValue = usage.CacheRead
	}
	return nullableInt64Pointer(usage.Input), nullableInt64Pointer(cachedValue),
		nullableInt64Pointer(usage.Output), nullableInt64Pointer(usage.Reasoning),
		nullableInt64Pointer(usage.Total), nullableInt64Pointer(usage.ContextWindow),
		nullableInt64Pointer(usage.CacheRead), nullableInt64Pointer(usage.CacheWrite),
		[]byte(nativeJSON), contract
}

func nullableInt64Pointer(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func (s *Store) ReadTranscript(ctx context.Context, request TranscriptReadRequest) (TranscriptPage, error) {
	pages, _, err := s.ReadTranscripts(ctx, request.ThreadID, []TranscriptReadRequest{request}, nil)
	if err != nil {
		return TranscriptPage{}, err
	}
	return pages[0], nil
}

// ReadTranscripts reads several pages of one thread, and which saved rows
// carry the given exact keys, in one read transaction: every answer comes
// from the same snapshot.
func (s *Store) ReadTranscripts(ctx context.Context, threadID string, requests []TranscriptReadRequest, match []TranscriptMatch) ([]TranscriptPage, map[string]string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	pages := make([]TranscriptPage, 0, len(requests))
	for _, request := range requests {
		if request.ThreadID != threadID {
			return nil, nil, ErrInvalidTranscriptPage
		}
		page, err := readTranscriptTx(ctx, tx, request)
		if err != nil {
			return nil, nil, err
		}
		pages = append(pages, page)
	}
	matched := map[string]string{}
	if len(match) > 0 {
		var projectionID, headSeq int64
		err := tx.QueryRowContext(ctx, `SELECT id, latest_seq FROM conversation_projections WHERE thread_id = ? AND state = 'active'`, threadID).Scan(&projectionID, &headSeq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM threads WHERE id = ?`, threadID).Scan(new(int)); errors.Is(err, sql.ErrNoRows) {
				return nil, nil, ErrThreadNotFound
			} else if err != nil {
				return nil, nil, err
			}
		case err != nil:
			return nil, nil, err
		default:
			if matched, err = readMatchedItemsTx(ctx, tx, projectionID, match, headSeq); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return pages, matched, nil
}

func readTranscriptTx(ctx context.Context, tx *sql.Tx, request TranscriptReadRequest) (TranscriptPage, error) {
	if request.ThreadID == "" {
		return TranscriptPage{}, ErrInvalidTranscriptPage
	}
	if request.Limit == 0 {
		request.Limit = DefaultTranscriptPageLimit
	}
	if request.Limit < 1 || request.Limit > MaxTranscriptPageLimit {
		return TranscriptPage{}, ErrInvalidTranscriptPage
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = MaxTranscriptPageBytes
	}
	if request.MaxBytes < 1024 || request.MaxBytes > MaxTranscriptPageBytes {
		return TranscriptPage{}, ErrInvalidTranscriptPage
	}
	if request.TurnLimit < 0 || request.TurnLimit > 20 ||
		(request.TurnLimit > 0 && request.BeforeNativeTurnID == "" && request.BeforeTurnOrdinal < 1) {
		return TranscriptPage{}, ErrInvalidTranscriptPage
	}
	hidden, err := validateHiddenTranscriptCategories(request.HiddenCategories)
	if err != nil {
		return TranscriptPage{}, err
	}
	if request.MessagesOnly && len(hidden) > 0 {
		return TranscriptPage{}, ErrInvalidTranscriptPage
	}
	categoryWhere, categoryArgs := transcriptCategoryWhere(hidden)
	if request.MessagesOnly {
		// The literal predicate, so SQLite reads the page, its turn extension
		// and the older-rows check through conversation_items_messages_page.
		categoryWhere = " AND (" + transcriptMessageSQL + ")"
	}
	var threadStatus SourceStatus
	if err := tx.QueryRowContext(ctx, `SELECT source_status FROM threads WHERE id = ?`, request.ThreadID).Scan(&threadStatus); errors.Is(err, sql.ErrNoRows) {
		return TranscriptPage{}, ErrThreadNotFound
	} else if err != nil {
		return TranscriptPage{}, err
	}

	page := TranscriptPage{Items: []TranscriptItem{}, Turns: []TranscriptTurn{}}
	var projectionID int64
	var indexed, transcriptError sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT id, projection_key, parser_version, schema_version, item_count, latest_seq,
       last_indexed_at, error_summary
FROM conversation_projections WHERE thread_id = ? AND state = 'active'`, request.ThreadID).Scan(
		&projectionID, &page.Projection.Key, &page.Projection.ParserVersion,
		&page.Projection.SchemaVersion, &page.Projection.ItemCount, &page.LatestSeq, &indexed, &transcriptError)
	if errors.Is(err, sql.ErrNoRows) {
		page.Projection = TranscriptProjectionInfo{State: TranscriptPending, SourceStatus: threadStatus, SchemaVersion: TranscriptSchemaVersion}
		return page, nil
	}
	if err != nil {
		return TranscriptPage{}, err
	}
	page.Projection.SourceStatus = threadStatus
	page.Projection.EvidenceAvailable = threadStatus == SourcePresent
	page.Projection.LastIndexedAt, err = parseNullTime(indexed)
	if err != nil {
		return TranscriptPage{}, err
	}
	page.Projection.State = transcriptAvailability(threadStatus, page.Projection.ItemCount, transcriptError.Valid)
	page.Projection.ErrorSummary = pointerFromNullString(transcriptError)
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM conversation_item_aliases WHERE thread_id = ?`, request.ThreadID).Scan(&page.Projection.AliasCount); err != nil {
		return TranscriptPage{}, err
	}
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM conversation_item_alias_conflicts WHERE thread_id = ?`, request.ThreadID).Scan(&page.Projection.AliasConflictCount); err != nil {
		return TranscriptPage{}, err
	}
	if request.ExpectedProjectionKey != "" && request.ExpectedProjectionKey != page.Projection.Key {
		return TranscriptPage{}, ErrTranscriptChanged
	}
	var turnWindowStart int64
	if request.TurnLimit > 0 {
		beforeOrdinal := request.BeforeTurnOrdinal
		if request.BeforeNativeTurnID != "" {
			if err := tx.QueryRowContext(ctx, `
SELECT ordinal FROM conversation_turns
WHERE projection_id = ? AND native_turn_id = ?`, projectionID, request.BeforeNativeTurnID).Scan(&beforeOrdinal); errors.Is(err, sql.ErrNoRows) {
				return TranscriptPage{}, ErrTranscriptChanged
			} else if err != nil {
				return TranscriptPage{}, err
			}
		}
		var boundaryStart int64
		if err := tx.QueryRowContext(ctx, `
SELECT start_seq FROM conversation_turns
WHERE projection_id = ? AND ordinal = ?`, projectionID, beforeOrdinal).Scan(&boundaryStart); errors.Is(err, sql.ErrNoRows) {
			return TranscriptPage{}, ErrTranscriptChanged
		} else if err != nil {
			return TranscriptPage{}, err
		}
		if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MIN(start_seq), 0) FROM (
  SELECT start_seq FROM conversation_turns
  WHERE projection_id = ? AND ordinal < ?
  ORDER BY ordinal DESC LIMIT ?
)`, projectionID, beforeOrdinal, request.TurnLimit).Scan(&turnWindowStart); err != nil {
			return TranscriptPage{}, err
		}
		request.SnapshotHeadSeq = boundaryStart - 1
		request.BeforeSeq = boundaryStart
	}
	page.SnapshotHeadSeq = request.SnapshotHeadSeq
	if page.SnapshotHeadSeq == 0 && request.TurnLimit == 0 {
		page.SnapshotHeadSeq = page.LatestSeq
	}
	if page.SnapshotHeadSeq < 0 || page.SnapshotHeadSeq > page.LatestSeq {
		return TranscriptPage{}, ErrTranscriptChanged
	}
	before := request.BeforeSeq
	if before == 0 {
		before = page.SnapshotHeadSeq + 1
	}
	if before < 1 || before > page.SnapshotHeadSeq+1 {
		return TranscriptPage{}, ErrInvalidTranscriptPage
	}

	itemCategoryWhere := categoryWhere
	itemCategoryArgs := append([]any(nil), categoryArgs...)
	if turnWindowStart > 0 {
		itemCategoryWhere += ` AND projected_seq >= ?`
		itemCategoryArgs = append(itemCategoryArgs, turnWindowStart)
	}
	itemQuery := `
SELECT item_id, projected_seq, source_key, native_item_id, turn_key, turn_ordinal,
       item_type, item_status, role, display_text, tool_name, call_id,
       detail_json, created_at, phase, filter_tags_json, synthetic, sidechain,
       is_final_answer, content_ref, content_truncated, original_bytes,
       (` + transcriptCategorySQL + `) AS category
FROM conversation_items
WHERE projection_id = ? AND projected_seq < ? AND projected_seq <= ?` + itemCategoryWhere + `
ORDER BY projected_seq DESC LIMIT ?`
	itemArgs := []any{projectionID, before, page.SnapshotHeadSeq}
	itemArgs = append(itemArgs, itemCategoryArgs...)
	itemArgs = append(itemArgs, request.Limit)
	rows, err := tx.QueryContext(ctx, itemQuery, itemArgs...)
	if err != nil {
		return TranscriptPage{}, err
	}
	defer rows.Close()
	var descending []TranscriptItem
	usedBytes := 512
	for rows.Next() {
		item, scanErr := scanTranscriptItem(rows)
		if scanErr != nil {
			return TranscriptPage{}, scanErr
		}
		cost := 256 + len(item.Text) + len(item.Detail) + len(item.ToolName) + len(item.CallID)
		if len(descending) > 0 && usedBytes+cost > request.MaxBytes {
			break
		}
		usedBytes += cost
		descending = append(descending, item)
	}
	if err := rows.Err(); err != nil {
		return TranscriptPage{}, err
	}
	if err := rows.Close(); err != nil {
		return TranscriptPage{}, err
	}
	for index := len(descending) - 1; index >= 0; index-- {
		page.Items = append(page.Items, descending[index])
	}
	// Prefer a whole-turn page start when the requested tail boundary cuts a
	// turn and the extension still fits both hard bounds. Otherwise the
	// response keeps the exact bounded window and reports StartsMidTurn.
	if len(page.Items) > 0 && len(page.Items) < MaxTranscriptPageLimit {
		var turnStart int64
		if err := tx.QueryRowContext(ctx, `
SELECT start_seq FROM conversation_turns WHERE projection_id = ? AND turn_key = ?`,
			projectionID, page.Items[0].TurnKey).Scan(&turnStart); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return TranscriptPage{}, err
		}
		if turnStart > 0 && turnStart < page.Items[0].ProjectedSeq {
			var extensionCount int
			if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM conversation_items
WHERE projection_id = ? AND projected_seq >= ? AND projected_seq < ?`+categoryWhere,
				append([]any{projectionID, turnStart, page.Items[0].ProjectedSeq}, categoryArgs...)...).Scan(&extensionCount); err != nil {
				return TranscriptPage{}, err
			}
			if extensionCount <= MaxTranscriptPageLimit-len(page.Items) {
				extended, extensionBytes, err := readTranscriptItemRangeWhere(ctx, tx, projectionID, turnStart, page.Items[0].ProjectedSeq, categoryWhere, categoryArgs)
				if err != nil {
					return TranscriptPage{}, err
				}
				if usedBytes+extensionBytes <= request.MaxBytes {
					page.Items = append(extended, page.Items...)
					usedBytes += extensionBytes
				}
			}
		}
	}
	if len(page.Items) > 0 {
		page.StartSeq = page.Items[0].ProjectedSeq
		page.EndSeq = page.Items[len(page.Items)-1].ProjectedSeq
		turns, err := readTranscriptTurns(ctx, tx, projectionID, page.StartSeq, page.EndSeq, page.SnapshotHeadSeq)
		if err != nil {
			return TranscriptPage{}, err
		}
		page.Turns = turns
		finalByID := make(map[string]bool)
		for _, turn := range turns {
			if turn.FinalItemID != "" {
				finalByID[turn.FinalItemID] = true
			}
			if turn.StartSeq < page.StartSeq && turn.EndSeq >= page.StartSeq {
				page.StartsMidTurn = true
			}
		}
		for index := range page.Items {
			page.Items[index].IsFinalAnswer = finalByID[page.Items[index].ID]
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
  SELECT 1 FROM conversation_items WHERE projection_id = ? AND projected_seq < ?`+categoryWhere+`
)`, append([]any{projectionID, page.StartSeq}, categoryArgs...)...).Scan(&page.HasMoreBefore); err != nil {
			return TranscriptPage{}, err
		}
	}
	return page, nil
}

// readMatchedItemsTx finds the saved row that carries each exact native key,
// through the indexed alias lookup ReadSavedUserRows uses, qualified as
// TranscriptMatch describes.
func readMatchedItemsTx(ctx context.Context, tx *sql.Tx, projectionID int64, matches []TranscriptMatch, headSeq int64) (map[string]string, error) {
	found := make(map[string]string)
	for _, match := range matches {
		if match.Namespace == MatchTurnItem || match.Namespace == MatchTurnNativeItem || match.Namespace == MatchTurnEvent {
			if err := readMatchedTurnsTx(ctx, tx, projectionID, match, headSeq, found); err != nil {
				return nil, err
			}
			continue
		}
		for _, key := range match.Keys {
			raw, _ := json.Marshal(key)
			scalar, ok := transcriptNativeScalar(raw)
			if !ok {
				continue
			}
			value, err := findTranscriptValueTx(ctx, tx, projectionID, scalar)
			if err != nil {
				return nil, err
			}
			if value == 0 {
				continue
			}
			rows, err := tx.QueryContext(ctx, `SELECT i.item_id, a.scope_id, i.source_line FROM conversation_observation_aliases a
CROSS JOIN conversation_observations o ON o.projection_id=a.projection_id AND o.key_id=a.observation_value_id
CROSS JOIN conversation_items i ON i.projection_id=o.projection_id AND i.source_line=o.source_line
WHERE a.projection_id=? AND a.namespace=? AND a.value_id=? AND i.item_type=? AND i.sidechain=0 AND i.projected_seq<=?
ORDER BY i.projected_seq`,
				projectionID, match.Namespace, value, match.ItemType, headSeq)
			if err != nil {
				return nil, err
			}
			scopes := make(map[string]int64)
			lines := make(map[string]int64)
			var order []string
			for rows.Next() {
				var itemID string
				var scopeID, line int64
				if err := rows.Scan(&itemID, &scopeID, &line); err != nil {
					rows.Close()
					return nil, err
				}
				if _, seen := scopes[itemID]; !seen {
					order = append(order, itemID)
				}
				scopes[itemID], lines[itemID] = scopeID, line
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			// The key's row is one saved row, which holds one item per content
			// block; rows on two lines leave the key unmatched.
			first, rowLines := "", make(map[int64]bool)
			for _, itemID := range order {
				var scope model.Scope
				if err := loadTranscriptValueTx(ctx, tx, projectionID, scopes[itemID], &scope); err != nil {
					return nil, err
				}
				if scope.ParentNativeID == nil && scope.NativeParentCallID == nil {
					if first == "" {
						first = itemID
					}
					rowLines[lines[itemID]] = true
				}
			}
			if len(rowLines) == 1 {
				found[key] = first
			}
		}
	}
	return found, nil
}

// readMatchedTurnsTx finds, by native turn id, the one row of the match's
// type in that turn. A turn id with more than one such row matches nothing.
func readMatchedTurnsTx(ctx context.Context, tx *sql.Tx, projectionID int64, match TranscriptMatch, headSeq int64, found map[string]string) error {
	for _, key := range match.Keys {
		turn, native := key, ""
		switch match.Namespace {
		case MatchTurnNativeItem:
			turn, native = match.Turns[key], key
		case MatchTurnEvent:
			turn = match.Turns[key]
		}
		if match.Namespace == MatchTurnEvent || len(match.Exclude[key]) > 0 {
			carry, events := false, match.Exclude[key]
			if match.Namespace == MatchTurnEvent {
				carry, events = true, []string{key}
			}
			id, err := readTurnRowByEventsTx(ctx, tx, projectionID, turn, match.ItemType, headSeq, carry, events)
			if err != nil {
				return err
			}
			if id != "" {
				found[key] = id
			}
			continue
		}
		rows, err := tx.QueryContext(ctx, turnItemQuery, projectionID, turn, match.ItemType, headSeq, native, native)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(ids) == 1 {
			found[key] = ids[0]
		}
	}
	return nil
}

// readTurnRowByEventsTx picks one turn's row by the native event ids it is
// proven to carry: the one row claimed by one of events when carry, otherwise
// the one row no event claims. A claim needs a genuine user row and one of its
// own observations scoped to this projection's provider and native thread,
// this native turn, and the top level. Several prompts can share the turn, so
// every row of that one turn is read before choosing; no limit comes first.
func readTurnRowByEventsTx(ctx context.Context, tx *sql.Tx, projectionID int64, turn, itemType string, headSeq int64, carry bool, events []string) (string, error) {
	var provider, nativeThread string
	if err := tx.QueryRowContext(ctx, `SELECT t.provider, t.native_thread_id FROM conversation_projections cp JOIN threads t ON t.id=cp.thread_id WHERE cp.id=?`,
		projectionID).Scan(&provider, &nativeThread); err != nil {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, turnItemsQuery, projectionID, turn, itemType, headSeq, "", "")
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || len(ids) == 0 {
		return "", err
	}
	args := []any{projectionID}
	for _, id := range ids {
		args = append(args, id)
	}
	members := `(?` + strings.Repeat(",?", len(ids)-1) + `)`
	observations, err := loadItemObservationMap(ctx, tx, projectionID, `WHERE m.projection_id=? AND m.item_id IN `+members, args...)
	if err != nil {
		return "", err
	}
	genuine := map[string]bool{}
	rows, err = tx.QueryContext(ctx, `SELECT item_id FROM conversation_items WHERE projection_id=? AND item_id IN `+members+` AND role='user' AND synthetic=0`, args...)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		genuine[id] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", err
	}
	claimed := func(id string) bool {
		if !genuine[id] {
			return false
		}
		for _, observation := range observations[id] {
			scope := observation.Scope
			if observation.NativeEventID != "" && slices.Contains(events, observation.NativeEventID) &&
				string(scope.Provider) == provider && scope.NativeThreadID != nil && *scope.NativeThreadID == nativeThread &&
				scope.NativeTurnID != nil && *scope.NativeTurnID == turn && scope.ParentNativeID == nil && scope.NativeParentCallID == nil {
				return true
			}
		}
		return false
	}
	match := ""
	for _, id := range ids {
		if claimed(id) != carry {
			continue
		}
		if match != "" {
			return "", nil
		}
		match = id
	}
	return match, nil
}

// CROSS JOIN keeps the turn first, and the unary + keeps the head bound off the
// page index, so a lookup reads one turn's rows through conversation_items_turn
// instead of every row of the projection (about 90 ms to under 1 ms on a
// 60,000-row thread).
const turnItemsQuery = `SELECT i.item_id FROM conversation_turns t
CROSS JOIN conversation_items i ON i.projection_id=t.projection_id AND i.turn_key=t.turn_key
WHERE t.projection_id=? AND t.native_turn_id=? AND i.item_type=? AND i.sidechain=0 AND +i.projected_seq<=?
  AND (?='' OR i.native_item_id=?)`

// turnItemQuery stops at two rows: a turn with more than one matches nothing.
const turnItemQuery = turnItemsQuery + `
LIMIT 2`

func readTranscriptItemRange(ctx context.Context, tx *sql.Tx, projectionID, startSeq, beforeSeq int64, hidden []TranscriptCategory) ([]TranscriptItem, int, error) {
	categoryWhere, categoryArgs := transcriptCategoryWhere(hidden)
	return readTranscriptItemRangeWhere(ctx, tx, projectionID, startSeq, beforeSeq, categoryWhere, categoryArgs)
}

func readTranscriptItemRangeWhere(ctx context.Context, tx *sql.Tx, projectionID, startSeq, beforeSeq int64, categoryWhere string, categoryArgs []any) ([]TranscriptItem, int, error) {
	query := `
SELECT item_id, projected_seq, source_key, native_item_id, turn_key, turn_ordinal,
       item_type, item_status, role, display_text, tool_name, call_id,
       detail_json, created_at, phase, filter_tags_json, synthetic, sidechain,
       is_final_answer, content_ref, content_truncated, original_bytes,
       (` + transcriptCategorySQL + `) AS category
FROM conversation_items
WHERE projection_id = ? AND projected_seq >= ? AND projected_seq < ?` + categoryWhere + `
ORDER BY projected_seq`
	args := append([]any{projectionID, startSeq, beforeSeq}, categoryArgs...)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var items []TranscriptItem
	bytes := 0
	for rows.Next() {
		item, err := scanTranscriptItem(rows)
		if err != nil {
			return nil, 0, err
		}
		bytes += 256 + len(item.Text) + len(item.Detail) + len(item.ToolName) + len(item.CallID)
		items = append(items, item)
	}
	return items, bytes, rows.Err()
}

func transcriptAvailability(status SourceStatus, count int64, transcriptFailed bool) TranscriptProjectionState {
	switch status {
	case SourceMissing:
		return TranscriptRetained
	case SourceError:
		return TranscriptDegraded
	default:
		if transcriptFailed {
			return TranscriptDegraded
		}
		if count == 0 {
			return TranscriptEmpty
		}
		return TranscriptReady
	}
}

type transcriptRowScanner interface{ Scan(...any) error }

func scanTranscriptItem(row transcriptRowScanner) (TranscriptItem, error) {
	var item TranscriptItem
	var nativeID, role, toolName, callID, createdAt, phase, contentRef, category sql.NullString
	var detail, tags []byte
	if err := row.Scan(&item.ID, &item.ProjectedSeq, &item.SourceKey, &nativeID, &item.TurnKey,
		&item.TurnOrdinal, &item.Type, &item.Status, &role, &item.Text, &toolName,
		&callID, &detail, &createdAt, &phase, &tags, &item.Synthetic,
		&item.Sidechain, &item.IsFinalAnswer, &contentRef, &item.ContentTruncated,
		&item.OriginalBytes, &category); err != nil {
		return TranscriptItem{}, err
	}
	item.NativeItemID = stringFromNull(nativeID)
	item.Role = TranscriptRole(stringFromNull(role))
	item.ToolName = stringFromNull(toolName)
	item.CallID = stringFromNull(callID)
	item.Phase = stringFromNull(phase)
	item.ContentRef = stringFromNull(contentRef)
	item.Category = TranscriptCategory(stringFromNull(category))
	item.Detail = append(json.RawMessage(nil), detail...)
	if err := json.Unmarshal(tags, &item.FilterTags); err != nil {
		return TranscriptItem{}, err
	}
	if item.Category == "" {
		item.Category = transcriptItemCategory(item)
	}
	var err error
	item.CreatedAt, err = parseNullTime(createdAt)
	return item, err
}

const transcriptCategorySQL = `CASE
	WHEN item_type = 'interaction' AND sidechain = 0 THEN 'messages'
  WHEN item_type = 'tool_result' AND item_status IN ('failed', 'interrupted')
    AND EXISTS (
      SELECT 1 FROM conversation_turns AS resolved_turn
      WHERE resolved_turn.projection_id = conversation_items.projection_id
        AND resolved_turn.turn_key = conversation_items.turn_key
        -- A turn with no completion is still running, as readTranscriptTurns reads it.
        AND (COALESCE(resolved_turn.completion_seq, 0) = 0 OR (resolved_turn.status = 'completed'
          AND resolved_turn.final_item_id IS NOT NULL
          AND resolved_turn.final_item_id != ''))
    ) THEN 'toolResult'
  -- Enhanced hides every shell command and its result, failed or not.
  WHEN item_type = 'command' THEN 'toolCall'
  WHEN item_type = 'tool_result' AND instr(filter_tags_json, '"command"') > 0 THEN 'toolResult'
  WHEN item_status IN ('failed', 'interrupted') THEN 'error'
  WHEN sidechain = 1 THEN 'subagent'
	  WHEN item_type IN ('user_message', 'agent_message', 'interaction') THEN 'messages'
  WHEN item_type = 'reasoning' THEN 'reasoning'
  WHEN item_type IN ('tool_call', 'file_change', 'plan') THEN 'toolCall'
  WHEN item_type = 'tool_result' THEN 'toolResult'
  WHEN instr(filter_tags_json, '"collaboration"') > 0 THEN 'subagent'
  WHEN instr(filter_tags_json, '"unclassified"') > 0 THEN 'unclassified'
  WHEN item_type IN ('compaction', 'notice', 'provider_event') OR role = 'system' THEN 'system'
  ELSE 'unclassified'
END`

func validateHiddenTranscriptCategories(values []TranscriptCategory) ([]TranscriptCategory, error) {
	known := make(map[TranscriptCategory]bool, len(TranscriptCategories))
	for _, category := range TranscriptCategories {
		known[category] = true
	}
	seen := make(map[TranscriptCategory]bool, len(values))
	for _, category := range values {
		if !known[category] {
			return nil, ErrInvalidTranscriptPage
		}
		seen[category] = true
	}
	result := make([]TranscriptCategory, 0, len(seen))
	for _, category := range TranscriptCategories {
		if seen[category] {
			result = append(result, category)
		}
	}
	return result, nil
}

func transcriptCategoryWhere(hidden []TranscriptCategory) (string, []any) {
	if len(hidden) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(hidden))
	args := make([]any, len(hidden))
	for index, category := range hidden {
		placeholders[index] = "?"
		args[index] = string(category)
	}
	return " AND (" + transcriptCategorySQL + ") NOT IN (" + strings.Join(placeholders, ",") + ")", args
}

func transcriptItemCategory(item TranscriptItem) TranscriptCategory {
	if item.Type == ItemInteraction && !item.Sidechain {
		return CategoryMessages
	}
	if item.Type == ItemCommand {
		return CategoryToolCall
	}
	if item.Type == ItemToolResult && slices.Contains(item.FilterTags, "command") {
		return CategoryToolResult
	}
	if item.Status == ItemStatusFailed || item.Status == ItemStatusInterrupted {
		return CategoryError
	}
	if item.Sidechain {
		return CategorySubagent
	}
	switch item.Type {
	case ItemUserMessage, ItemAgentMessage, ItemInteraction:
		return CategoryMessages
	case ItemReasoning:
		return CategoryReasoning
	case ItemToolCall, ItemFileChange, ItemPlan:
		return CategoryToolCall
	case ItemToolResult:
		return CategoryToolResult
	}
	for _, tag := range item.FilterTags {
		if tag == "collaboration" {
			return CategorySubagent
		}
		if tag == "unclassified" {
			return CategoryUnclassified
		}
	}
	if item.Type == ItemCompaction || item.Type == ItemNotice || item.Type == ItemProviderEvent || item.Role == RoleSystem {
		return CategorySystem
	}
	return CategoryUnclassified
}

// LoadEligibleConversationItem applies the shared search-corpus boundary to
// one persisted item. It is internal store plumbing for future backfill and
// incremental maintenance; it does not create an index or expose an API.
func (s *Store) LoadEligibleConversationItem(ctx context.Context, projectionKey, itemID string) (EligibleConversationSearchItem, bool, error) {
	if strings.TrimSpace(projectionKey) == "" || strings.TrimSpace(itemID) == "" {
		return EligibleConversationSearchItem{}, false, ErrInvalidTranscriptPage
	}
	row := s.db.QueryRowContext(ctx, `
SELECT cp.id, cp.projection_key, cp.thread_id, cp.state, t.kind,
       ci.item_id, ci.item_type, ci.role, ci.synthetic, ci.display_text,
       CASE WHEN cib.item_id IS NULL THEN 0 ELSE 1 END, cib.body_text
FROM conversation_projections cp
JOIN threads t ON t.id = cp.thread_id
JOIN conversation_items ci ON ci.projection_id = cp.id
LEFT JOIN conversation_item_bodies cib
  ON cib.projection_id = ci.projection_id AND cib.item_id = ci.item_id
WHERE cp.projection_key = ? AND ci.item_id = ?`, projectionKey, itemID)
	candidate, err := scanPersistedConversationSearchCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EligibleConversationSearchItem{}, false, nil
	}
	if err != nil {
		return EligibleConversationSearchItem{}, false, err
	}
	eligible, ok := EligibleConversationItem(candidate)
	return eligible, ok, nil
}

// LoadTranscriptBody reads Nano's retained normalized body without consulting
// the provider-owned source file. Item IDs are projection-scoped, so a stale
// disclosure cannot cross a projection rebuild.
func (s *Store) LoadTranscriptBody(ctx context.Context, threadID, itemID string) (TranscriptRetainedBody, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		return TranscriptRetainedBody{}, ErrInvalidTranscriptPage
	}
	canonicalItemID, err := s.ResolveTranscriptItemID(ctx, threadID, itemID)
	if err != nil {
		return TranscriptRetainedBody{}, err
	}
	var body TranscriptRetainedBody
	var text sql.NullString
	var raw []byte
	err = s.db.QueryRowContext(ctx, `
SELECT cp.thread_id, cib.item_id, cp.projection_key, cib.content_ref,
       cib.body_text, cib.body_json, cib.original_bytes, cib.stored_bytes,
       cib.truncated
FROM conversation_projections cp
JOIN conversation_item_bodies cib ON cib.projection_id = cp.id
WHERE cp.thread_id = ? AND cp.state = 'active' AND cib.item_id = ?`, threadID, canonicalItemID).Scan(
		&body.ThreadID, &body.ItemID, &body.ProjectionKey, &body.ContentRef,
		&text, &raw, &body.OriginalBytes, &body.StoredBytes, &body.Truncated)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if checkErr := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&exists); checkErr != nil {
			return TranscriptRetainedBody{}, checkErr
		}
		if exists == 0 {
			return TranscriptRetainedBody{}, ErrThreadNotFound
		}
		return TranscriptRetainedBody{}, ErrTranscriptBodyUnavailable
	}
	if err != nil {
		return TranscriptRetainedBody{}, err
	}
	body.Text = stringFromNull(text)
	body.JSON = append(json.RawMessage(nil), raw...)
	return body, nil
}

// ResolveTranscriptItemID accepts historical projection-scoped ids during the
// compatibility window but always returns the canonical source-lineage id.
func (s *Store) ResolveTranscriptItemID(ctx context.Context, threadID, itemID string) (string, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		return "", ErrInvalidTranscriptPage
	}
	current := itemID
	for hops := 0; hops < 4; hops++ {
		var next string
		err := s.db.QueryRowContext(ctx, `SELECT canonical_item_id FROM conversation_item_aliases WHERE thread_id = ? AND old_item_id = ?`, threadID, current).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return current, nil
		}
		if err != nil {
			return "", err
		}
		if next == "" || next == current {
			return current, nil
		}
		// A later parser can restore a row an earlier rebuild absorbed. An id
		// that names a row of the active projection is current, whatever an
		// older alias says.
		var active int
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM conversation_items i JOIN conversation_projections p ON p.id = i.projection_id
WHERE p.thread_id = ? AND p.state = 'active' AND i.item_id = ?`, threadID, current).Scan(&active)
		if err == nil {
			return current, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		current = next
	}
	return "", ErrTranscriptConflict
}

func recordTranscriptItemAliases(ctx context.Context, tx *sql.Tx, threadID string, oldProjectionID, newProjectionID int64) error {
	rows, err := tx.QueryContext(ctx, `
SELECT old.item_id, old.source_key, next.item_id
FROM conversation_items old
JOIN conversation_items next ON next.projection_id = ? AND next.source_key = old.source_key
WHERE old.projection_id = ? AND old.item_id != next.item_id`, newProjectionID, oldProjectionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oldID, sourceKey, canonicalID string
		if err := rows.Scan(&oldID, &sourceKey, &canonicalID); err != nil {
			return err
		}
		if err := recordTranscriptItemAlias(ctx, tx, threadID, oldID, sourceKey, canonicalID); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return recordAbsorbedTranscriptItemAliases(ctx, tx, threadID, oldProjectionID, newProjectionID)
}

// recordAbsorbedTranscriptItemAliases keeps an old public id resolvable when
// the parser now joins its source row into another item, such as the second
// copy of a Codex serialization twin (reader v36).
func recordAbsorbedTranscriptItemAliases(ctx context.Context, tx *sql.Tx, threadID string, oldProjectionID, newProjectionID int64) error {
	aliases, err := absorbedTranscriptItemAliases(ctx, tx, oldProjectionID, newProjectionID)
	if err != nil {
		return err
	}
	for _, alias := range aliases {
		if err := recordTranscriptItemAlias(ctx, tx, threadID, alias.OldID, alias.SourceKey, alias.CanonicalID); err != nil {
			return err
		}
	}
	return nil
}

type absorbedTranscriptItemAlias struct{ OldID, SourceKey, CanonicalID string }

// absorbedTranscriptItemAliases finds each old row with no same-source-key
// successor and pairs it with the one new item whose observations include its
// source line with the same bytes, from the same source file. A changed line,
// or zero or several such items, gives no pair: the old row's evidence is gone
// or its owner is a guess.
func absorbedTranscriptItemAliases(ctx context.Context, tx *sql.Tx, oldProjectionID, newProjectionID int64) ([]absorbedTranscriptItemAlias, error) {
	rows, err := tx.QueryContext(ctx, `
WITH retired AS (
  SELECT old.item_id, old.source_key, old.source_line, old.source_line_sha256
  FROM conversation_items old
  WHERE old.projection_id = ? AND old.source_line_sha256 != '' AND NOT EXISTS (
    SELECT 1 FROM conversation_items same WHERE same.projection_id = ? AND same.source_key = old.source_key)
    AND (SELECT source_file_id FROM conversation_projections WHERE id = ?) =
        (SELECT source_file_id FROM conversation_projections WHERE id = ?)
), owners AS (
  SELECT obs.source_line, lower(hex(obs.source_sha256)) AS source_line_sha256,
         MIN(link.item_id) AS item_id, COUNT(DISTINCT link.item_id) AS owners
  FROM conversation_item_observations link
  JOIN conversation_observations obs ON obs.id = link.observation_id
  WHERE link.projection_id = ? AND obs.source_line IN (SELECT source_line FROM retired)
  GROUP BY obs.source_line, obs.source_sha256
)
SELECT retired.item_id, retired.source_key, owners.item_id
FROM retired JOIN owners
  ON owners.source_line = retired.source_line AND owners.source_line_sha256 = retired.source_line_sha256
WHERE owners.owners = 1 AND owners.item_id != retired.item_id`,
		oldProjectionID, newProjectionID, oldProjectionID, newProjectionID, newProjectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var aliases []absorbedTranscriptItemAlias
	for rows.Next() {
		var alias absorbedTranscriptItemAlias
		if err := rows.Scan(&alias.OldID, &alias.SourceKey, &alias.CanonicalID); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	return aliases, rows.Err()
}

// recordTranscriptItemAlias adds one old-to-canonical alias. It never rewrites
// an existing alias; a different existing target is recorded as a conflict.
func recordTranscriptItemAlias(ctx context.Context, tx *sql.Tx, threadID, oldID, sourceKey, canonicalID string) error {
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT canonical_item_id FROM conversation_item_aliases WHERE thread_id = ? AND old_item_id = ?`, threadID, oldID).Scan(&existing)
	if err == nil && existing != canonicalID {
		_, err := tx.ExecContext(ctx, `
INSERT INTO conversation_item_alias_conflicts(
  thread_id, old_item_id, existing_canonical_item_id, candidate_canonical_item_id, source_key, detected_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(thread_id, old_item_id, candidate_canonical_item_id) DO UPDATE SET
  existing_canonical_item_id = excluded.existing_canonical_item_id,
  source_key = excluded.source_key,
  detected_at = excluded.detected_at`,
			threadID, oldID, existing, canonicalID, sourceKey, formatTime(time.Now().UTC()))
		return err
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_item_aliases(thread_id, old_item_id, canonical_item_id, source_key, created_at) VALUES (?, ?, ?, ?, ?)`, threadID, oldID, canonicalID, sourceKey, formatTime(time.Now().UTC()))
	return err
}

func (s *Store) LoadTranscriptEvidenceRef(ctx context.Context, threadID, itemID string) (TranscriptEvidenceRef, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(itemID) == "" {
		return TranscriptEvidenceRef{}, ErrInvalidTranscriptPage
	}
	canonicalItemID, err := s.ResolveTranscriptItemID(ctx, threadID, itemID)
	if err != nil {
		return TranscriptEvidenceRef{}, err
	}
	var ref TranscriptEvidenceRef
	var parsed bool
	var payload sql.NullString
	var device, inode sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
SELECT cp.thread_id, ci.item_id, cp.projection_key, cp.provider,
       sf.canonical_path, ci.source_line, ci.source_row_type, ci.source_payload_type, ci.source_line_sha256,
       cp.source_generation, sf.generation, cp.complete_offset, sf.device, sf.inode, sf.status,
       o.id IS NOT NULL, COALESCE(o.byte_start,0),COALESCE(o.byte_length,0),ci.parsed_json!='{}'
FROM conversation_projections cp
JOIN conversation_items ci ON ci.projection_id = cp.id
JOIN source_files sf ON sf.id = cp.source_file_id
LEFT JOIN conversation_observations o ON o.id=(
 SELECT MIN(ob.id) FROM conversation_item_observations m
 JOIN conversation_observations ob ON ob.id=m.observation_id AND ob.projection_id=m.projection_id
 JOIN conversation_parse_values src ON src.id=ob.source_id AND src.projection_id=ob.projection_id
 WHERE m.projection_id=cp.id AND m.item_id=ci.item_id
   AND ob.source_line=ci.source_line AND lower(hex(ob.source_sha256))=ci.source_line_sha256
   AND json_extract(CASE WHEN substr(src.value_key,1,2)=X'014A' THEN substr(src.value_key,3) ELSE '{}' END,'$.kind')='file'
   AND json_extract(CASE WHEN substr(src.value_key,1,2)=X'014A' THEN substr(src.value_key,3) ELSE '{}' END,'$.label')=sf.canonical_path
   AND json_extract(CASE WHEN substr(src.value_key,1,2)=X'014A' THEN substr(src.value_key,3) ELSE '{}' END,'$.incarnation')=cp.projection_key
)
WHERE cp.thread_id = ? AND cp.state = 'active' AND ci.item_id = ?`, threadID, canonicalItemID).Scan(
		&ref.ThreadID, &ref.ItemID, &ref.ProjectionKey, &ref.Provider,
		&ref.SourcePath, &ref.SourceLine, &ref.SourceRowType, &payload, &ref.SourceLineSHA256,
		&ref.SourceGeneration, &ref.CurrentGeneration, &ref.CompleteOffset,
		&device, &inode, &ref.SourceStatus, &ref.HasByteRange, &ref.ByteStart, &ref.ByteLength, &parsed)
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if checkErr := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&exists); checkErr != nil {
			return TranscriptEvidenceRef{}, checkErr
		}
		if exists == 0 {
			return TranscriptEvidenceRef{}, ErrThreadNotFound
		}
		return TranscriptEvidenceRef{}, ErrTranscriptEvidenceUnavailable
	}
	if err != nil {
		return TranscriptEvidenceRef{}, err
	}
	ref.SourcePayloadType = stringFromNull(payload)
	ref.Device = int64FromNull(device)
	ref.Inode = int64FromNull(inode)
	if ref.SourceStatus != SourcePresent || ref.SourceGeneration != ref.CurrentGeneration || len(ref.SourceLineSHA256) != sha256.Size*2 {
		return TranscriptEvidenceRef{}, ErrTranscriptChanged
	}
	if parsed && !ref.HasByteRange {
		return TranscriptEvidenceRef{}, ErrTranscriptEvidenceUnavailable
	}
	if ref.HasByteRange && (ref.ByteStart < 0 || ref.ByteLength <= 0 || ref.ByteStart > ref.CompleteOffset || ref.ByteLength > ref.CompleteOffset-ref.ByteStart) {
		return TranscriptEvidenceRef{}, ErrTranscriptChanged
	}
	return ref, nil
}

func readTranscriptTurns(ctx context.Context, tx *sql.Tx, projectionID, startSeq, endSeq, snapshot int64) ([]TranscriptTurn, error) {
	// Bound the semantic check to each turn so a source-address index cannot
	// turn a page read into repeated scans of the entire projection.
	rows, err := tx.QueryContext(ctx, `
SELECT turn_key, native_turn_id, ordinal, start_seq, end_seq, completion_seq,
       status, completion_evidence, final_item_id, finality_source,
       finality_confidence, started_at, completed_at, duration_ms, config_json,
       usage_input, usage_cached, usage_output, usage_reasoning, usage_total, context_window,
       usage_cache_read, usage_cache_write, usage_native_json, usage_contract
FROM conversation_turns
WHERE projection_id = ? AND start_seq <= ? AND end_seq >= ?
  AND EXISTS (
    SELECT 1 FROM conversation_items semantic
    WHERE semantic.projection_id = conversation_turns.projection_id
      AND semantic.turn_key = conversation_turns.turn_key
      AND semantic.projected_seq >= conversation_turns.start_seq
      AND semantic.projected_seq <= conversation_turns.end_seq
      AND semantic.item_type NOT IN ('provider_event', 'notice', 'compaction')
  )
ORDER BY ordinal`, projectionID, endSeq, startSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := []TranscriptTurn{}
	for rows.Next() {
		var turn TranscriptTurn
		var nativeID, finalID, finalSource, confidence, started, completed sql.NullString
		var completionSeq, duration sql.NullInt64
		var usageInput, usageCached, usageOutput, usageReasoning, usageTotal, contextWindow sql.NullInt64
		var usageCacheRead, usageCacheWrite, usageContract sql.NullInt64
		var config, usageNative []byte
		if err := rows.Scan(&turn.Key, &nativeID, &turn.Ordinal, &turn.StartSeq,
			&turn.EndSeq, &completionSeq, &turn.Status, &turn.CompletionEvidence,
			&finalID, &finalSource, &confidence, &started, &completed, &duration,
			&config, &usageInput, &usageCached, &usageOutput, &usageReasoning,
			&usageTotal, &contextWindow, &usageCacheRead, &usageCacheWrite, &usageNative,
			&usageContract); err != nil {
			return nil, err
		}
		turn.NativeTurnID = stringFromNull(nativeID)
		turn.CompletionSeq = int64FromNull(completionSeq)
		turn.DurationMS = int64FromNull(duration)
		turn.Usage = scanTurnUsage(usageInput, usageCached, usageOutput, usageReasoning, usageTotal, contextWindow, usageCacheRead, usageCacheWrite, usageNative, usageContract)
		turn.Config = append(json.RawMessage(nil), config...)
		var parseErr error
		turn.StartedAt, parseErr = parseNullTime(started)
		if parseErr != nil {
			return nil, parseErr
		}
		turn.CompletedAt, parseErr = parseNullTime(completed)
		if parseErr != nil {
			return nil, parseErr
		}
		if turn.EndSeq > snapshot {
			turn.EndSeq = snapshot
		}
		if turn.CompletionSeq == 0 || turn.CompletionSeq > snapshot {
			turn.Status = TurnInProgress
			turn.CompletionEvidence = CompletionNone
			turn.CompletionSeq = 0
			turn.CompletedAt = nil
			turn.FinalItemID = ""
			turn.FinalitySource = ""
			turn.FinalityConfidence = ""
			turn.DurationMS = 0
		} else {
			turn.FinalItemID = stringFromNull(finalID)
			turn.FinalitySource = stringFromNull(finalSource)
			turn.FinalityConfidence = stringFromNull(confidence)
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// scanTurnUsage rebuilds the optional per-turn usage record. An all-NULL
// envelope round-trips to a nil usage record.
func scanTurnUsage(input, cached, output, reasoning, total, contextWindow, cacheRead, cacheWrite sql.NullInt64, native []byte, contract sql.NullInt64) *TranscriptTokenUsage {
	columns := []sql.NullInt64{input, cached, output, reasoning, total, contextWindow, cacheRead, cacheWrite}
	any := false
	for _, column := range columns {
		if column.Valid {
			any = true
			break
		}
	}
	if !any && len(bytes.TrimSpace(native)) <= 2 {
		return nil
	}
	usage := &TranscriptTokenUsage{
		Input:         int64PointerFromNull(input),
		Output:        int64PointerFromNull(output),
		Reasoning:     int64PointerFromNull(reasoning),
		CacheRead:     int64PointerFromNull(cacheRead),
		CacheWrite:    int64PointerFromNull(cacheWrite),
		Cached:        int64PointerFromNull(cached),
		Total:         int64PointerFromNull(total),
		ContextWindow: int64PointerFromNull(contextWindow),
	}
	if usage.CacheRead == nil && usage.Cached != nil {
		usage.CacheRead = int64Pointer(*usage.Cached)
	}
	if len(native) > 0 && string(native) != "{}" {
		usage.Native = append(json.RawMessage(nil), native...)
	}
	if contract.Valid {
		usage.Contract = int(contract.Int64)
	}
	return usage
}

func int64PointerFromNull(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return int64Pointer(value.Int64)
}

func stringFromNull(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
func int64FromNull(value sql.NullInt64) int64 {
	if value.Valid {
		return value.Int64
	}
	return 0
}

// ThreadTurnTotals is the evidence behind a thread's "took": the summed native
// duration of its turns, and how many turns that sum actually covers.
//
// Coverage is reported rather than assumed because partial coverage is the
// COMMON case, not an edge (measured across the local corpus 2026-07-31: of the
// interactive main threads a reader actually opens, roughly a fifth have no
// duration at all and another 40% have it for only some turns — claude records
// `turn_duration` for about half its completed turns, and codex's
// `task_complete` carries `duration_ms` for ~90% of its own). A caller renders
// "12m 40s · 9 turns" so the number is legible as a partial sum, and drops the
// field entirely at zero coverage.
type ThreadTurnTotals struct {
	DurationMS int64 `json:"durationMs"`
	Turns      int64 `json:"turns"`
}

// ThreadTurnTotals sums a thread's per-turn native durations. Three rules, each
// one a bug that would otherwise ship:
//
//   - **Active generation only.** A superseded projection can remain while the
//     background collector catches up. An unscoped SUM over conversation_turns
//     would double-count those rows.
//   - **Never wall clock.** First-to-last timestamps would silently bill the
//     hours a thread sat idle between turns as work. Only summed native
//     durations, and only over turns that HAVE one.
//   - **The in-flight guard, mirrored from the read path** (readTranscriptTurns
//     zeroes a duration whose completion_seq exceeds the snapshot): a turn that
//     completed past the projection's own head is not finished as far as this
//     snapshot is concerned, so its duration is not yet countable.
func (s *Store) ThreadTurnTotals(ctx context.Context, threadID string) (ThreadTurnTotals, error) {
	var totals ThreadTurnTotals
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(t.duration_ms), 0), COUNT(*)
FROM conversation_turns t
JOIN conversation_projections cp ON cp.id = t.projection_id
WHERE cp.thread_id = ? AND cp.state = 'active'
  AND t.duration_ms > 0
  AND t.completion_seq > 0 AND t.completion_seq <= cp.latest_seq`, threadID).
		Scan(&totals.DurationMS, &totals.Turns)
	if err != nil {
		return ThreadTurnTotals{}, fmt.Errorf("sum turn durations for %s: %w", threadID, err)
	}
	return totals, nil
}
