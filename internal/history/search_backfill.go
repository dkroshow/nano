package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultConversationSearchBackfillBatchSize = 500

// ConversationSearchBackfillStart explicitly clears and begins the selected
// derived modes. CorpusReset is reserved for rebuilding both modes together;
// it recreates shared membership only after both modes become non-serving.
// Document IDs stay stable while a build resumes, but have no identity contract
// across a deliberate CorpusReset: membership is recreated and SQLite may reuse
// or allocate different integer IDs.
type ConversationSearchBackfillStart struct {
	Modes       []ConversationSearchMode
	CorpusReset bool
}

// ConversationSearchBackfillBatch examines at most BatchSize normalized source
// rows, including rows the shared eligibility boundary rejects. A zero
// BatchSize uses the conservative production default; tests may choose a
// smaller bound to exercise checkpointing.
type ConversationSearchBackfillBatch struct {
	Modes     []ConversationSearchMode
	BatchSize int
}

// ConversationSearchBackfillProgress is an internal, read-only completion
// snapshot. It does not expose a public search/status API.
type ConversationSearchBackfillProgress struct {
	Modes     []ConversationSearchStatus
	Examined  int
	Processed int
	Complete  bool
}

// ErrConversationSearchReconciliationStale means a selected build lifecycle
// was reset, failed, or otherwise replaced after a consistent reconciliation
// snapshot. Ordinary transcript maintenance may advance revision and counts
// without making the proof stale because it maintains the same derived rows in
// its source transaction. The mode remains building and the caller may safely
// run another batch.
var ErrConversationSearchReconciliationStale = errors.New("conversation search reconciliation snapshot became stale")

var beforeConversationSearchReadinessForTest func()

// StartConversationSearchBackfill resets selected derived structures and marks
// them building. It never discovers historical messages; Run...Batch does that
// in bounded transactions.
func (s *Store) StartConversationSearchBackfill(ctx context.Context, start ConversationSearchBackfillStart) ([]ConversationSearchStatus, error) {
	modes, err := normalizeConversationSearchModes(start.Modes)
	if err != nil {
		return nil, err
	}
	if start.CorpusReset && !hasBothConversationSearchModes(modes) {
		return nil, errors.New("corpus search reset requires literal and terms modes")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin conversation search backfill start: %w", err)
	}
	defer tx.Rollback()
	if err := initializeConversationSearchState(ctx, tx, defaultConversationSearchVersions()); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if start.CorpusReset {
		for _, mode := range modes {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+searchFTSTable(mode)); err != nil {
				return nil, fmt.Errorf("clear %s search rows: %w", mode, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_search_documents`); err != nil {
			return nil, fmt.Errorf("clear conversation search membership: %w", err)
		}
		if err := setConversationSearchBuildStartTx(ctx, tx, modes, now, true); err != nil {
			return nil, err
		}
	} else {
		for _, mode := range modes {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+searchFTSTable(mode)); err != nil {
				return nil, fmt.Errorf("clear %s search rows: %w", mode, err)
			}
		}
		if err := setConversationSearchBuildStartTx(ctx, tx, modes, now, false); err != nil {
			return nil, err
		}
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit conversation search backfill start: %w", err)
	}
	return statuses, nil
}

// FailConversationSearchBackfill records one terminal scheduler failure for
// the selected building modes. It is intentionally separate from a normal
// batch so a background owner can stop retrying without leaving a partial mode
// looking authoritative.
func (s *Store) FailConversationSearchBackfill(ctx context.Context, modes []ConversationSearchMode, reason error) ([]ConversationSearchStatus, error) {
	selectedModes, err := normalizeConversationSearchModes(modes)
	if err != nil {
		return nil, err
	}
	if reason == nil {
		return nil, errors.New("conversation search failure requires a reason")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin conversation search failure: %w", err)
	}
	defer tx.Rollback()
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	selected := selectConversationSearchStatuses(statuses, selectedModes)
	if len(selected) != len(selectedModes) {
		return nil, errors.New("conversation search state rows are missing")
	}
	building := make([]ConversationSearchMode, 0, len(selected))
	for _, status := range selected {
		if status.State == ConversationSearchBuilding {
			building = append(building, status.Mode)
		}
	}
	if err := failConversationSearchModesTx(ctx, tx, building, reason.Error()); err != nil {
		return nil, err
	}
	statuses, err = conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit conversation search failure: %w", err)
	}
	return selectConversationSearchStatuses(statuses, selectedModes), nil
}

// RunConversationSearchBackfillBatch applies one bounded, resumable batch. It
// atomically persists membership, FTS rows, counts, and the durable checkpoint.
func (s *Store) RunConversationSearchBackfillBatch(ctx context.Context, batch ConversationSearchBackfillBatch) (ConversationSearchBackfillProgress, error) {
	modes, err := normalizeConversationSearchModes(batch.Modes)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	batchSize := batch.BatchSize
	if batchSize == 0 {
		batchSize = defaultConversationSearchBackfillBatchSize
	}
	if batchSize < 1 {
		return ConversationSearchBackfillProgress{}, errors.New("conversation search batch size must be positive")
	}
	release, err := s.backgroundWrite(ctx)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationSearchBackfillProgress{}, fmt.Errorf("begin conversation search batch: %w", err)
	}
	defer tx.Rollback()
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	selected, checkpointProjection, checkpointItem, err := selectedBuildingSearchStatuses(statuses, modes)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	examined, processed, changed, lastProjection, lastItem, lastDocument, err := applyConversationSearchBatch(ctx, tx, modes, checkpointProjection, checkpointItem, batchSize)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	if examined == 0 {
		// Release both the batch transaction and write lock before the full
		// reconciliation snapshot, so transcript ingestion is never stalled by it.
		tx.Rollback()
		release()
		locked = false
		return s.finalizeConversationSearchBackfill(ctx, modes, selected)
	}
	if err := updateConversationSearchBatchProgressTx(ctx, tx, selected, lastProjection, lastItem, lastDocument, changed, time.Now().UTC()); err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	statuses, err = conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationSearchBackfillProgress{}, fmt.Errorf("commit conversation search batch: %w", err)
	}
	return ConversationSearchBackfillProgress{Modes: selectConversationSearchStatuses(statuses, modes), Examined: examined, Processed: processed}, nil
}

func setConversationSearchBuildStartTx(ctx context.Context, tx *sql.Tx, modes []ConversationSearchMode, now time.Time, corpusReset bool) error {
	for _, mode := range modes {
		revision := "revision"
		if corpusReset {
			revision = "revision + 1"
		}
		statement := `
UPDATE conversation_search_state
SET state = ?, checkpoint_projection_id = NULL, checkpoint_item_id = NULL,
    checkpoint_document_id = NULL, document_count = 0, indexed_count = 0,
    revision = ` + revision + `, started_at = ?, updated_at = ?, completed_at = NULL, error_summary = NULL
WHERE mode = ?`
		if _, err := tx.ExecContext(ctx, statement, ConversationSearchBuilding, formatTime(now), formatTime(now), mode); err != nil {
			return fmt.Errorf("start %s conversation search build: %w", mode, err)
		}
	}
	return nil
}

func applyConversationSearchBatch(ctx context.Context, tx *sql.Tx, modes []ConversationSearchMode, checkpointProjection *int64, checkpointItem *string, batchSize int) (int, int, bool, *int64, *string, *int64, error) {
	examined, processed := 0, 0
	changed := false
	var lastProjection *int64
	var lastItem *string
	var lastDocument *int64
	err := VisitPersistedConversationSearchCandidatesAfterLimit(ctx, tx, checkpointProjection, checkpointItem, batchSize, func(candidate ConversationSearchCandidate, item EligibleConversationSearchItem, eligible bool) error {
		projection := candidate.Identity.ProjectionID
		itemID := candidate.Identity.ItemID
		lastProjection, lastItem, lastDocument = &projection, &itemID, nil
		examined++
		if !eligible {
			return nil
		}
		updated, err := upsertConversationSearchEligibleItemTx(ctx, tx, modes, item)
		if err != nil {
			return err
		}
		changed = changed || updated
		var documentID int64
		if err := tx.QueryRowContext(ctx, `SELECT document_id FROM conversation_search_documents WHERE projection_id = ? AND item_id = ?`, item.Identity.ProjectionID, item.Identity.ItemID).Scan(&documentID); err != nil {
			return err
		}
		document := documentID
		lastDocument = &document
		processed++
		return nil
	})
	return examined, processed, changed, lastProjection, lastItem, lastDocument, err
}

func updateConversationSearchBatchProgressTx(ctx context.Context, tx *sql.Tx, selected []ConversationSearchStatus, projectionID *int64, itemID *string, documentID *int64, changed bool, now time.Time) error {
	var membershipCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_search_documents`).Scan(&membershipCount); err != nil {
		return err
	}
	for _, status := range selected {
		var indexedCount int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+searchFTSTable(status.Mode)).Scan(&indexedCount); err != nil {
			return err
		}
		revision := "revision"
		if changed {
			revision = "revision + 1"
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_search_state
SET checkpoint_projection_id = ?, checkpoint_item_id = ?, checkpoint_document_id = ?,
    document_count = ?, indexed_count = ?, revision = `+revision+`, updated_at = ?
WHERE mode = ?`, projectionID, itemID, documentID, membershipCount, indexedCount, formatTime(now), status.Mode); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) finalizeConversationSearchBackfill(ctx context.Context, modes []ConversationSearchMode, selected []ConversationSearchStatus) (ConversationSearchBackfillProgress, error) {
	// SQLite's default deferred transaction establishes one consistent read
	// snapshot without a Store write lock. End that snapshot before taking the
	// short promotion lock: transcript maintenance that commits during a long
	// reconciliation already maintains membership and both FTS modes, so it may
	// safely advance revisions and counts without invalidating a successful proof.
	readTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationSearchBackfillProgress{}, fmt.Errorf("begin conversation search reconciliation snapshot: %w", err)
	}
	defer readTx.Rollback()
	selectedGeneration, err := conversationSearchBuildGenerationForStatuses(selected, modes)
	if err != nil {
		return ConversationSearchBackfillProgress{}, ErrConversationSearchReconciliationStale
	}
	statuses, err := conversationSearchStatusesTx(ctx, readTx)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	snapshotSelected := selectConversationSearchStatuses(statuses, modes)
	snapshotGeneration, err := conversationSearchBuildGenerationForStatuses(snapshotSelected, modes)
	if err != nil || !sameConversationSearchBuildGeneration(selectedGeneration, snapshotGeneration) {
		return ConversationSearchBackfillProgress{Modes: selectConversationSearchStatuses(statuses, modes)}, ErrConversationSearchReconciliationStale
	}
	applicable := applicableConversationSearchModes(statuses, modes)
	snapshotApplicable := selectConversationSearchStatuses(statuses, applicable)
	reconcileErr := reconcileConversationSearch(ctx, readTx, applicable)
	if err := readTx.Commit(); err != nil {
		return ConversationSearchBackfillProgress{}, fmt.Errorf("commit conversation search reconciliation snapshot: %w", err)
	}
	if ctx.Err() != nil {
		return ConversationSearchBackfillProgress{}, ctx.Err()
	}
	if beforeConversationSearchReadinessForTest != nil {
		beforeConversationSearchReadinessForTest()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	promotionTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationSearchBackfillProgress{}, fmt.Errorf("begin conversation search reconciliation promotion: %w", err)
	}
	defer promotionTx.Rollback()
	statuses, err = conversationSearchStatusesTx(ctx, promotionTx)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	promotionSelected := selectConversationSearchStatuses(statuses, modes)
	promotionGeneration, generationErr := conversationSearchBuildGenerationForStatuses(promotionSelected, modes)
	if generationErr != nil || !sameConversationSearchBuildGeneration(snapshotGeneration, promotionGeneration) {
		return ConversationSearchBackfillProgress{Modes: promotionSelected}, ErrConversationSearchReconciliationStale
	}
	if reconcileErr != nil {
		// A failed snapshot may have been repaired by maintained work after it
		// ended. Unlike a successful proof, do not persist that old failure if
		// any selected mode's derived revision/counts changed.
		promotionApplicable := selectConversationSearchStatuses(statuses, applicable)
		if !sameConversationSearchFailureSnapshot(snapshotApplicable, promotionApplicable) {
			return ConversationSearchBackfillProgress{Modes: promotionSelected}, ErrConversationSearchReconciliationStale
		}
		if err := failConversationSearchModesTx(ctx, promotionTx, modes, reconcileErr.Error()); err != nil {
			return ConversationSearchBackfillProgress{}, err
		}
		statuses, err = conversationSearchStatusesTx(ctx, promotionTx)
		if err != nil {
			return ConversationSearchBackfillProgress{}, err
		}
		if err := promotionTx.Commit(); err != nil {
			return ConversationSearchBackfillProgress{}, fmt.Errorf("commit failed conversation search reconciliation: %w", err)
		}
		return ConversationSearchBackfillProgress{}, fmt.Errorf("conversation search reconciliation failed: %w", reconcileErr)
	}
	if err := markConversationSearchModesReadyTx(ctx, promotionTx, promotionSelected, time.Now().UTC()); err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	statuses, err = conversationSearchStatusesTx(ctx, promotionTx)
	if err != nil {
		return ConversationSearchBackfillProgress{}, err
	}
	if err := promotionTx.Commit(); err != nil {
		return ConversationSearchBackfillProgress{}, fmt.Errorf("commit completed conversation search batch: %w", err)
	}
	return ConversationSearchBackfillProgress{Modes: selectConversationSearchStatuses(statuses, modes), Complete: true}, nil
}

// conversationSearchBuildGeneration identifies one durable build lifecycle.
// Revision and counts are intentionally absent: transactional transcript
// maintenance advances those fields while preserving the index invariant.
type conversationSearchBuildGeneration struct {
	Mode                   ConversationSearchMode
	CorpusRuleVersion      string
	TokenizerVersion       string
	State                  ConversationSearchBuildState
	CheckpointProjectionID *int64
	CheckpointItemID       *string
	CheckpointDocumentID   *int64
	StartedAt              *time.Time
}

func conversationSearchBuildGenerationForStatuses(statuses []ConversationSearchStatus, modes []ConversationSearchMode) ([]conversationSearchBuildGeneration, error) {
	selected := selectConversationSearchStatuses(statuses, modes)
	if len(selected) != len(modes) {
		return nil, errors.New("conversation search state rows are missing")
	}
	generation := make([]conversationSearchBuildGeneration, 0, len(selected))
	for _, status := range selected {
		if status.State != ConversationSearchBuilding || status.StartedAt == nil {
			return nil, fmt.Errorf("conversation search mode %q is not an active build", status.Mode)
		}
		generation = append(generation, conversationSearchBuildGeneration{
			Mode:                   status.Mode,
			CorpusRuleVersion:      status.CorpusRuleVersion,
			TokenizerVersion:       status.TokenizerVersion,
			State:                  status.State,
			CheckpointProjectionID: status.CheckpointProjectionID,
			CheckpointItemID:       status.CheckpointItemID,
			CheckpointDocumentID:   status.CheckpointDocumentID,
			StartedAt:              status.StartedAt,
		})
	}
	return generation, nil
}

func sameConversationSearchBuildGeneration(left, right []conversationSearchBuildGeneration) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Mode != right[index].Mode || left[index].CorpusRuleVersion != right[index].CorpusRuleVersion ||
			left[index].TokenizerVersion != right[index].TokenizerVersion || left[index].State != right[index].State ||
			!equalInt64Pointers(left[index].CheckpointProjectionID, right[index].CheckpointProjectionID) ||
			!equalStringPointers(left[index].CheckpointItemID, right[index].CheckpointItemID) ||
			!equalInt64Pointers(left[index].CheckpointDocumentID, right[index].CheckpointDocumentID) ||
			!equalTimePointers(left[index].StartedAt, right[index].StartedAt) {
			return false
		}
	}
	return true
}

func sameConversationSearchFailureSnapshot(left, right []ConversationSearchStatus) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Mode != right[index].Mode || left[index].CorpusRuleVersion != right[index].CorpusRuleVersion ||
			left[index].TokenizerVersion != right[index].TokenizerVersion || left[index].State != right[index].State ||
			left[index].Revision != right[index].Revision || left[index].DocumentCount != right[index].DocumentCount ||
			left[index].IndexedCount != right[index].IndexedCount ||
			!equalInt64Pointers(left[index].CheckpointProjectionID, right[index].CheckpointProjectionID) ||
			!equalStringPointers(left[index].CheckpointItemID, right[index].CheckpointItemID) ||
			!equalInt64Pointers(left[index].CheckpointDocumentID, right[index].CheckpointDocumentID) ||
			!equalTimePointers(left[index].StartedAt, right[index].StartedAt) ||
			!equalTimePointers(left[index].CompletedAt, right[index].CompletedAt) ||
			!equalStringPointers(left[index].ErrorSummary, right[index].ErrorSummary) {
			return false
		}
	}
	return true
}

type conversationSearchReadQueryer interface {
	conversationSearchCandidateQueryer
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func reconcileConversationSearch(ctx context.Context, queryer conversationSearchReadQueryer, modes []ConversationSearchMode) error {
	eligible := make(map[searchDocumentIdentity]struct{})
	if err := VisitPersistedEligibleConversationItemIdentities(ctx, queryer, func(identity ConversationSearchIdentity) error {
		key := searchDocumentIdentity{ProjectionID: identity.ProjectionID, ItemID: identity.ItemID}
		if _, exists := eligible[key]; exists {
			return fmt.Errorf("duplicate eligible identity %#v", key)
		}
		eligible[key] = struct{}{}
		return nil
	}); err != nil {
		return err
	}
	documents := make(map[searchDocumentIdentity]int64, len(eligible))
	rows, err := queryer.QueryContext(ctx, `SELECT d.document_id, d.projection_id, d.item_id FROM conversation_search_documents d JOIN conversation_projections p ON p.id=d.projection_id WHERE p.state='active'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var documentID, projectionID int64
		var itemID string
		if err := rows.Scan(&documentID, &projectionID, &itemID); err != nil {
			rows.Close()
			return err
		}
		key := searchDocumentIdentity{ProjectionID: projectionID, ItemID: itemID}
		if _, exists := documents[key]; exists {
			rows.Close()
			return fmt.Errorf("duplicate search membership identity %#v", key)
		}
		documents[key] = documentID
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for key := range eligible {
		if _, exists := documents[key]; !exists {
			return fmt.Errorf("missing search membership identity %#v", key)
		}
	}
	for key := range documents {
		if _, exists := eligible[key]; !exists {
			return fmt.Errorf("extra search membership identity %#v", key)
		}
	}
	if len(documents) != len(eligible) {
		return fmt.Errorf("search membership count %d does not match eligible count %d", len(documents), len(eligible))
	}
	wantRows := make(map[int64]struct{}, len(documents))
	for _, documentID := range documents {
		wantRows[documentID] = struct{}{}
	}
	for _, mode := range modes {
		if err := reconcileConversationSearchFTSRows(ctx, queryer, mode, wantRows); err != nil {
			return err
		}
	}
	return nil
}

type searchDocumentIdentity struct {
	ProjectionID int64
	ItemID       string
}

func reconcileConversationSearchFTSRows(ctx context.Context, queryer conversationSearchReadQueryer, mode ConversationSearchMode, want map[int64]struct{}) error {
	rows, err := queryer.QueryContext(ctx, `SELECT f.rowid FROM `+searchFTSTable(mode)+` f LEFT JOIN conversation_search_documents d ON d.document_id=f.rowid LEFT JOIN conversation_projections p ON p.id=d.projection_id WHERE d.document_id IS NULL OR p.state='active'`)
	if err != nil {
		return err
	}
	got := make(map[int64]struct{}, len(want))
	for rows.Next() {
		var documentID int64
		if err := rows.Scan(&documentID); err != nil {
			rows.Close()
			return err
		}
		if _, exists := got[documentID]; exists {
			rows.Close()
			return fmt.Errorf("duplicate %s FTS rowid %d", mode, documentID)
		}
		got[documentID] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for documentID := range want {
		if _, exists := got[documentID]; !exists {
			return fmt.Errorf("missing %s FTS rowid %d", mode, documentID)
		}
	}
	for documentID := range got {
		if _, exists := want[documentID]; !exists {
			return fmt.Errorf("extra %s FTS rowid %d", mode, documentID)
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s FTS row count %d does not match membership count %d", mode, len(got), len(want))
	}
	return nil
}

func markConversationSearchModesReadyTx(ctx context.Context, tx *sql.Tx, selected []ConversationSearchStatus, now time.Time) error {
	var membershipCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_search_documents`).Scan(&membershipCount); err != nil {
		return err
	}
	for _, status := range selected {
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_search_state
SET state = ?, document_count = ?, indexed_count = ?, completed_at = ?, updated_at = ?, error_summary = NULL
WHERE mode = ?`, ConversationSearchReady, membershipCount, membershipCount, formatTime(now), formatTime(now), status.Mode); err != nil {
			return err
		}
	}
	return nil
}

func failConversationSearchModesTx(ctx context.Context, tx *sql.Tx, modes []ConversationSearchMode, reason string) error {
	now := formatTime(time.Now().UTC())
	for _, mode := range modes {
		if _, err := tx.ExecContext(ctx, `
UPDATE conversation_search_state
SET state = ?, completed_at = ?, updated_at = ?, error_summary = ?
WHERE mode = ?`, ConversationSearchFailed, now, now, boundedConversationSearchError(reason), mode); err != nil {
			return err
		}
	}
	return nil
}

func boundedConversationSearchError(value string) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= maxSearchErrorSummaryBytes {
		return value
	}
	for len(value) > maxSearchErrorSummaryBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func selectedBuildingSearchStatuses(statuses []ConversationSearchStatus, modes []ConversationSearchMode) ([]ConversationSearchStatus, *int64, *string, error) {
	selected := selectConversationSearchStatuses(statuses, modes)
	if len(selected) != len(modes) {
		return nil, nil, nil, errors.New("conversation search state rows are missing")
	}
	for _, status := range selected {
		if status.State != ConversationSearchBuilding {
			return nil, nil, nil, fmt.Errorf("conversation search mode %q is %q, not building", status.Mode, status.State)
		}
	}
	projection, item, document := selected[0].CheckpointProjectionID, selected[0].CheckpointItemID, selected[0].CheckpointDocumentID
	for _, status := range selected[1:] {
		if !equalInt64Pointers(projection, status.CheckpointProjectionID) || !equalStringPointers(item, status.CheckpointItemID) || !equalInt64Pointers(document, status.CheckpointDocumentID) {
			return nil, nil, nil, errors.New("selected conversation search modes have different checkpoints")
		}
	}
	return selected, projection, item, nil
}

func applicableConversationSearchModes(statuses []ConversationSearchStatus, selected []ConversationSearchMode) []ConversationSearchMode {
	selectedSet := make(map[ConversationSearchMode]struct{}, len(selected))
	for _, mode := range selected {
		selectedSet[mode] = struct{}{}
	}
	var modes []ConversationSearchMode
	for _, status := range statuses {
		if status.State == ConversationSearchReady {
			modes = append(modes, status.Mode)
			continue
		}
		if _, building := selectedSet[status.Mode]; building {
			modes = append(modes, status.Mode)
		}
	}
	return modes
}

func selectConversationSearchStatuses(statuses []ConversationSearchStatus, modes []ConversationSearchMode) []ConversationSearchStatus {
	wanted := make(map[ConversationSearchMode]struct{}, len(modes))
	for _, mode := range modes {
		wanted[mode] = struct{}{}
	}
	var selected []ConversationSearchStatus
	for _, status := range statuses {
		if _, ok := wanted[status.Mode]; ok {
			selected = append(selected, status)
		}
	}
	return selected
}

func normalizeConversationSearchModes(modes []ConversationSearchMode) ([]ConversationSearchMode, error) {
	if len(modes) == 0 {
		return nil, errors.New("at least one conversation search mode is required")
	}
	set := make(map[ConversationSearchMode]struct{}, len(modes))
	for _, mode := range modes {
		if !validConversationSearchMode(mode) {
			return nil, fmt.Errorf("invalid conversation search mode %q", mode)
		}
		if _, exists := set[mode]; exists {
			return nil, fmt.Errorf("duplicate conversation search mode %q", mode)
		}
		set[mode] = struct{}{}
	}
	sorted := make([]ConversationSearchMode, 0, len(set))
	for mode := range set {
		sorted = append(sorted, mode)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted, nil
}

func hasBothConversationSearchModes(modes []ConversationSearchMode) bool {
	return len(modes) == 2 && modes[0] == ConversationSearchLiteral && modes[1] == ConversationSearchTerms
}

func searchFTSTable(mode ConversationSearchMode) string {
	if mode == ConversationSearchLiteral {
		return "conversation_search_literal_fts"
	}
	return "conversation_search_terms_fts"
}

func equalInt64Pointers(left, right *int64) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalStringPointers(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalTimePointers(left, right *time.Time) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && left.Equal(*right))
}
