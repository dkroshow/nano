package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	conversationSearchCorpusRuleVersion = "conversation-corpus-v1"
	conversationSearchLiteralTokenizer  = "trigram-v1"
	conversationSearchTermsTokenizer    = "unicode61-v1"
	maxSearchErrorSummaryBytes          = 512
)

// ConversationSearchMode identifies one independently rebuildable derived
// index. It remains internal history plumbing until a later search API slice.
type ConversationSearchMode string

const (
	ConversationSearchLiteral ConversationSearchMode = "literal"
	ConversationSearchTerms   ConversationSearchMode = "terms"
)

// ConversationSearchBuildState describes whether a mode can serve a complete
// corpus. Only the bounded lifecycle transitions in SetConversationSearchState
// may persist a new value.
type ConversationSearchBuildState string

const (
	ConversationSearchNeedsBuild ConversationSearchBuildState = "needs_build"
	ConversationSearchBuilding   ConversationSearchBuildState = "building"
	ConversationSearchReady      ConversationSearchBuildState = "ready"
	ConversationSearchFailed     ConversationSearchBuildState = "failed"
)

// ConversationSearchVersions isolates corpus membership rules from each FTS
// tokenizer. A corpus rule change invalidates both modes; a tokenizer change
// invalidates only its matching mode.
type ConversationSearchVersions struct {
	CorpusRule string
	Literal    string
	Terms      string
}

// ConversationSearchStatus is the durable state for historical search
// maintenance and the internal data returned by the read-only search-status
// HTTP contract.
type ConversationSearchStatus struct {
	Mode                   ConversationSearchMode       `json:"mode"`
	CorpusRuleVersion      string                       `json:"corpusRuleVersion"`
	TokenizerVersion       string                       `json:"tokenizerVersion"`
	State                  ConversationSearchBuildState `json:"state"`
	CheckpointProjectionID *int64                       `json:"checkpointProjectionId"`
	CheckpointItemID       *string                      `json:"checkpointItemId"`
	CheckpointDocumentID   *int64                       `json:"checkpointDocumentId"`
	DocumentCount          int64                        `json:"documentCount"`
	IndexedCount           int64                        `json:"indexedCount"`
	Revision               int64                        `json:"revision"`
	StartedAt              *time.Time                   `json:"startedAt"`
	UpdatedAt              time.Time                    `json:"updatedAt"`
	CompletedAt            *time.Time                   `json:"completedAt"`
	ErrorSummary           *string                      `json:"error"`
}

// ConversationSearchStateUpdate is a validated lifecycle write used by the
// bounded builder and transactional maintenance.
type ConversationSearchStateUpdate struct {
	State                  ConversationSearchBuildState
	CheckpointProjectionID *int64
	CheckpointItemID       *string
	CheckpointDocumentID   *int64
	DocumentCount          int64
	IndexedCount           int64
	ErrorSummary           *string
}

func defaultConversationSearchVersions() ConversationSearchVersions {
	return ConversationSearchVersions{
		CorpusRule: conversationSearchCorpusRuleVersion,
		Literal:    conversationSearchLiteralTokenizer,
		Terms:      conversationSearchTermsTokenizer,
	}
}

func (versions ConversationSearchVersions) validate() error {
	if strings.TrimSpace(versions.CorpusRule) == "" || strings.TrimSpace(versions.Literal) == "" || strings.TrimSpace(versions.Terms) == "" {
		return errors.New("conversation search versions must be non-empty")
	}
	return nil
}

func (versions ConversationSearchVersions) tokenizer(mode ConversationSearchMode) (string, error) {
	switch mode {
	case ConversationSearchLiteral:
		return versions.Literal, nil
	case ConversationSearchTerms:
		return versions.Terms, nil
	default:
		return "", fmt.Errorf("invalid conversation search mode %q", mode)
	}
}

// ConversationSearchStatuses returns the persisted status for both derived
// modes, ordered stably for callers and tests.
func (s *Store) ConversationSearchStatuses(ctx context.Context) ([]ConversationSearchStatus, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin conversation search status read: %w", err)
	}
	defer tx.Rollback()
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit conversation search status read: %w", err)
	}
	return statuses, nil
}

// EnsureConversationSearchVersions persists an invalidation when corpus or
// tokenizer semantics change. It never discovers historical messages or
// writes an FTS row.
func (s *Store) EnsureConversationSearchVersions(ctx context.Context, versions ConversationSearchVersions) ([]ConversationSearchStatus, error) {
	if err := versions.validate(); err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin conversation search version update: %w", err)
	}
	defer tx.Rollback()
	if err := initializeConversationSearchState(ctx, tx, versions); err != nil {
		return nil, err
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit conversation search version update: %w", err)
	}
	return statuses, nil
}

func initializeConversationSearchState(ctx context.Context, tx *sql.Tx, versions ConversationSearchVersions) error {
	if err := versions.validate(); err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	type storedVersion struct{ corpus, tokenizer string }
	stored := make(map[ConversationSearchMode]storedVersion, 2)
	missing := 0
	corpusChanged := false
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		if _, err := versions.tokenizer(mode); err != nil {
			return err
		}
		var corpusRule, storedTokenizer string
		err := tx.QueryRowContext(ctx, `
SELECT corpus_rule_version, tokenizer_version
FROM conversation_search_state WHERE mode = ?`, mode).Scan(&corpusRule, &storedTokenizer)
		if errors.Is(err, sql.ErrNoRows) {
			missing++
			continue
		}
		if err != nil {
			return fmt.Errorf("read conversation search %s state: %w", mode, err)
		}
		stored[mode] = storedVersion{corpusRule, storedTokenizer}
		corpusChanged = corpusChanged || corpusRule != versions.CorpusRule
	}
	if missing == 2 {
		// Fresh schema creation is intentionally empty and fast.
		for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
			tokenizer, _ := versions.tokenizer(mode)
			if err := insertConversationSearchModeTx(ctx, tx, mode, versions.CorpusRule, tokenizer, now); err != nil {
				return err
			}
		}
		return nil
	}
	if missing != 0 || corpusChanged {
		// One missing row is not a safe independent tokenizer state: membership
		// and the surviving mode may describe a different corpus generation.
		if err := clearConversationSearchDerivedRowsTx(ctx, tx); err != nil {
			return err
		}
		for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
			tokenizer, _ := versions.tokenizer(mode)
			if err := resetConversationSearchModeTx(ctx, tx, mode, versions.CorpusRule, tokenizer, now); err != nil {
				return err
			}
		}
		return nil
	}
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		tokenizer, _ := versions.tokenizer(mode)
		if stored[mode].tokenizer == tokenizer {
			continue
		}
		// A tokenizer-only change preserves shared membership and a valid sibling.
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+searchFTSTable(mode)); err != nil {
			return fmt.Errorf("clear invalid %s search rows: %w", mode, err)
		}
		if err := resetConversationSearchModeTx(ctx, tx, mode, versions.CorpusRule, tokenizer, now); err != nil {
			return err
		}
	}
	return nil
}

func clearConversationSearchDerivedRowsTx(ctx context.Context, tx *sql.Tx) error {
	for _, mode := range []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+searchFTSTable(mode)); err != nil {
			return fmt.Errorf("clear invalid %s search rows: %w", mode, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_search_documents`); err != nil {
		return fmt.Errorf("clear invalid conversation search membership: %w", err)
	}
	return nil
}

func insertConversationSearchModeTx(ctx context.Context, tx *sql.Tx, mode ConversationSearchMode, corpusRule, tokenizer, now string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO conversation_search_state(mode, corpus_rule_version, tokenizer_version, state, updated_at)
VALUES (?, ?, ?, ?, ?)`, mode, corpusRule, tokenizer, ConversationSearchNeedsBuild, now); err != nil {
		return fmt.Errorf("insert conversation search %s state: %w", mode, err)
	}
	return nil
}

func resetConversationSearchModeTx(ctx context.Context, tx *sql.Tx, mode ConversationSearchMode, corpusRule, tokenizer, now string) error {
	if _, err := tx.ExecContext(ctx, `
INSERT INTO conversation_search_state(mode, corpus_rule_version, tokenizer_version, state, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(mode) DO UPDATE SET
    corpus_rule_version = excluded.corpus_rule_version,
    tokenizer_version = excluded.tokenizer_version,
    state = excluded.state,
    checkpoint_projection_id = NULL, checkpoint_item_id = NULL,
    checkpoint_document_id = NULL, document_count = 0, indexed_count = 0,
    revision = revision + 1, started_at = NULL, completed_at = NULL,
    updated_at = excluded.updated_at, error_summary = NULL`, mode, corpusRule, tokenizer, ConversationSearchNeedsBuild, now); err != nil {
		return fmt.Errorf("invalidate conversation search %s state: %w", mode, err)
	}
	return nil
}

// SetConversationSearchState changes one persisted mode through its limited
// lifecycle. It does not populate or delete search documents or FTS rows.
func (s *Store) SetConversationSearchState(ctx context.Context, mode ConversationSearchMode, update ConversationSearchStateUpdate) (ConversationSearchStatus, error) {
	if !validConversationSearchMode(mode) || !validConversationSearchBuildState(update.State) {
		return ConversationSearchStatus{}, errors.New("invalid conversation search state update")
	}
	if update.DocumentCount < 0 || update.IndexedCount < 0 || update.IndexedCount > update.DocumentCount {
		return ConversationSearchStatus{}, errors.New("invalid conversation search progress counts")
	}
	if update.State == ConversationSearchReady && update.IndexedCount != update.DocumentCount {
		return ConversationSearchStatus{}, errors.New("ready conversation search state requires complete indexing")
	}
	if update.ErrorSummary != nil && len(*update.ErrorSummary) > maxSearchErrorSummaryBytes {
		return ConversationSearchStatus{}, fmt.Errorf("conversation search error summary exceeds %d bytes", maxSearchErrorSummaryBytes)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationSearchStatus{}, fmt.Errorf("begin conversation search state update: %w", err)
	}
	defer tx.Rollback()
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return ConversationSearchStatus{}, err
	}
	var current ConversationSearchStatus
	found := false
	for _, status := range statuses {
		if status.Mode == mode {
			current, found = status, true
			break
		}
	}
	if !found {
		return ConversationSearchStatus{}, fmt.Errorf("conversation search state missing for %q", mode)
	}
	if !canTransitionConversationSearchState(current.State, update.State) {
		return ConversationSearchStatus{}, fmt.Errorf("invalid conversation search state transition %q to %q", current.State, update.State)
	}
	now := time.Now().UTC()
	startedAt, completedAt := current.StartedAt, current.CompletedAt
	switch update.State {
	case ConversationSearchNeedsBuild:
		// An explicit reset clears all durable progress.
		update.CheckpointProjectionID, update.CheckpointItemID, update.CheckpointDocumentID = nil, nil, nil
		update.DocumentCount, update.IndexedCount, update.ErrorSummary = 0, 0, nil
		startedAt, completedAt = nil, nil
	case ConversationSearchBuilding:
		if startedAt == nil {
			startedAt = &now
		}
		completedAt = nil
	case ConversationSearchReady:
		completedAt = &now
	case ConversationSearchFailed:
		completedAt = &now
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE conversation_search_state
SET state = ?, checkpoint_projection_id = ?, checkpoint_item_id = ?, checkpoint_document_id = ?,
    document_count = ?, indexed_count = ?, started_at = ?, updated_at = ?, completed_at = ?, error_summary = ?
WHERE mode = ?`, update.State, update.CheckpointProjectionID, update.CheckpointItemID, update.CheckpointDocumentID,
		update.DocumentCount, update.IndexedCount, nullableTime(startedAt), formatTime(now), nullableTime(completedAt), nullableString(update.ErrorSummary), mode); err != nil {
		return ConversationSearchStatus{}, fmt.Errorf("update conversation search state: %w", err)
	}
	statuses, err = conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return ConversationSearchStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConversationSearchStatus{}, fmt.Errorf("commit conversation search state update: %w", err)
	}
	for _, status := range statuses {
		if status.Mode == mode {
			return status, nil
		}
	}
	return ConversationSearchStatus{}, fmt.Errorf("conversation search state disappeared for %q", mode)
}

func conversationSearchStatusesTx(ctx context.Context, tx *sql.Tx) ([]ConversationSearchStatus, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT mode, corpus_rule_version, tokenizer_version, state,
       checkpoint_projection_id, checkpoint_item_id, checkpoint_document_id,
       document_count, indexed_count, revision, started_at, updated_at, completed_at, error_summary
FROM conversation_search_state
ORDER BY CASE mode WHEN 'literal' THEN 1 WHEN 'terms' THEN 2 END`)
	if err != nil {
		return nil, fmt.Errorf("read conversation search statuses: %w", err)
	}
	defer rows.Close()
	var statuses []ConversationSearchStatus
	for rows.Next() {
		var status ConversationSearchStatus
		var checkpointProjection, checkpointDocument sql.NullInt64
		var checkpointItem, started, updated, completed, errorSummary sql.NullString
		if err := rows.Scan(&status.Mode, &status.CorpusRuleVersion, &status.TokenizerVersion, &status.State,
			&checkpointProjection, &checkpointItem, &checkpointDocument,
			&status.DocumentCount, &status.IndexedCount, &status.Revision,
			&started, &updated, &completed, &errorSummary); err != nil {
			return nil, fmt.Errorf("scan conversation search status: %w", err)
		}
		status.CheckpointProjectionID = searchNullableInt64Pointer(checkpointProjection)
		status.CheckpointItemID = searchNullableStringPointer(checkpointItem)
		status.CheckpointDocumentID = searchNullableInt64Pointer(checkpointDocument)
		var parseErr error
		if status.StartedAt, parseErr = parseNullTime(started); parseErr != nil {
			return nil, parseErr
		}
		if status.UpdatedAt, parseErr = parseRequiredTime(updated); parseErr != nil {
			return nil, parseErr
		}
		if status.CompletedAt, parseErr = parseNullTime(completed); parseErr != nil {
			return nil, parseErr
		}
		status.ErrorSummary = searchNullableStringPointer(errorSummary)
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conversation search statuses: %w", err)
	}
	return statuses, nil
}

func searchNullableInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	valueCopy := value.Int64
	return &valueCopy
}

func searchNullableStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	valueCopy := value.String
	return &valueCopy
}

func parseRequiredTime(value sql.NullString) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, errors.New("required conversation search timestamp is null")
	}
	parsed, err := time.Parse(sqliteTimeFormat, value.String)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func validConversationSearchMode(mode ConversationSearchMode) bool {
	return mode == ConversationSearchLiteral || mode == ConversationSearchTerms
}

func validConversationSearchBuildState(state ConversationSearchBuildState) bool {
	return state == ConversationSearchNeedsBuild || state == ConversationSearchBuilding ||
		state == ConversationSearchReady || state == ConversationSearchFailed
}

func canTransitionConversationSearchState(from, to ConversationSearchBuildState) bool {
	switch from {
	case ConversationSearchNeedsBuild:
		return to == ConversationSearchNeedsBuild || to == ConversationSearchBuilding
	case ConversationSearchBuilding:
		return to == ConversationSearchNeedsBuild || to == ConversationSearchBuilding || to == ConversationSearchReady || to == ConversationSearchFailed
	case ConversationSearchReady:
		return to == ConversationSearchReady || to == ConversationSearchNeedsBuild
	case ConversationSearchFailed:
		return to == ConversationSearchFailed || to == ConversationSearchNeedsBuild || to == ConversationSearchBuilding
	default:
		return false
	}
}
