package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

var ErrTranscriptRebuildPreempted = errors.New("transcript rebuild preempted")

// TranscriptRebuildHandle belongs to one builder and one process. Its base remains
// visible until SwitchTranscriptRebuild commits; restart abandons this handle.
type TranscriptRebuildHandle struct {
	id, activeID                  int64
	threadID, nativeID            string
	base                          TranscriptCheckpoint
	input                         TranscriptApplyRequest
	search                        []ConversationSearchStatus
	itemsComplete, searchComplete bool
}

func (s *Store) BeginTranscriptRebuild(ctx context.Context, input TranscriptApplyRequest, base TranscriptCheckpoint) (*TranscriptRebuildHandle, error) {
	if input.Result.ReadOffset == 0 {
		input.Result.ReadOffset = input.Result.CompleteOffset
	}
	if err := validateTranscriptApply(input); err != nil {
		return nil, err
	}
	if input.Mode != TranscriptRebuild || input.Result.parsedBatches == nil {
		return nil, errors.New("chunked rebuild requires a parsed rebuild spool")
	}
	state, entries, err := splitTranscriptContinuation(input.Result.ParserState)
	if err != nil {
		return nil, err
	}
	release, err := s.backgroundWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, activeID, err := readActiveTranscriptCheckpointTx(ctx, tx, input.ThreadID, false)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(current, base) || current.SourceID != input.SourceID || current.SourceGeneration != input.SourceGeneration || current.Provider != input.Provider {
		return nil, ErrTranscriptChanged
	}
	var nativeID string
	if err := tx.QueryRowContext(ctx, `SELECT native_thread_id FROM threads WHERE id=?`, input.ThreadID).Scan(&nativeID); err != nil {
		return nil, err
	}
	id, err := createTranscriptProjectionTx(ctx, tx, input, state, entries)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &TranscriptRebuildHandle{id: id, activeID: activeID, threadID: input.ThreadID, nativeID: nativeID, base: base, input: input}, nil
}

// WriteTranscriptRebuild ends commits only between complete spool batches.
// The last chunk also writes Grok's existing session usage view.
func (s *Store) WriteTranscriptRebuild(ctx context.Context, build *TranscriptRebuildHandle, preempt func() bool) error {
	var tx *sql.Tx
	var release func()
	var started time.Time
	finish := func(commit bool) error {
		if tx == nil {
			return nil
		}
		var err error
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		tx = nil
		release()
		release = nil
		return err
	}
	defer func() { _ = finish(false) }()
	open := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if preempt != nil && preempt() {
			return ErrTranscriptRebuildPreempted
		}
		var err error
		release, err = s.backgroundWrite(ctx)
		if err != nil {
			return err
		}
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			release()
			release = nil
			return err
		}
		started = time.Now()
		return nil
	}
	// A transaction-local cache must not survive a rollback or a chunk boundary.
	var chunkCtx context.Context
	err := build.input.Result.parsedBatches(func(batch model.Batch) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if tx != nil && time.Since(started) >= backgroundWriteTarget {
			if err := finish(true); err != nil {
				return err
			}
		}
		if tx == nil {
			if err := open(); err != nil {
				return err
			}
			chunkCtx = context.WithValue(ctx, transcriptValueCacheKey{}, &transcriptValueCache{ids: make(map[string]int64)})
		}
		if err := writeParsedBatchTx(chunkCtx, tx, build.id, build.nativeID, batch, build.input.Result.recordSource); err != nil {
			return err
		}
		if time.Since(started) >= backgroundWriteTarget {
			return finish(true)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if tx == nil {
		if err := open(); err != nil {
			return err
		}
	}
	if err := writeTranscriptSessionUsageTx(ctx, tx, build.id, build.input.Result.sessionUsage); err != nil {
		return err
	}
	if err := finish(true); err != nil {
		return err
	}
	build.itemsComplete = true
	return nil
}

// IndexTranscriptRebuild records the search lifecycle once, before its first
// search chunk. Each candidate uses the shared eligibility rule with S0's kind.
func (s *Store) IndexTranscriptRebuild(ctx context.Context, build *TranscriptRebuildHandle, kind ThreadKind, preempt func() bool) error {
	if !build.itemsComplete {
		return errors.New("transcript rebuild items are incomplete")
	}
	statuses, err := s.ConversationSearchStatuses(ctx)
	if err != nil {
		return err
	}
	build.search = statuses
	modes := maintainedConversationSearchModes(statuses)
	if len(modes) == 0 {
		build.searchComplete = true
		return nil
	}
	var latest int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(projected_seq),0) FROM conversation_items WHERE projection_id=?`, build.id).Scan(&latest)
	if err != nil {
		return err
	}
	for after := int64(0); after < latest; {
		if preempt != nil && preempt() {
			return ErrTranscriptRebuildPreempted
		}
		through := min(after+defaultConversationSearchBackfillBatchSize, latest)
		err := s.indexTranscriptRebuildRange(ctx, build, kind, modes, after, through)
		if err != nil {
			return err
		}
		after = through
	}
	build.searchComplete = true
	return nil
}

func (s *Store) indexTranscriptRebuildRange(ctx context.Context, build *TranscriptRebuildHandle, kind ThreadKind, modes []ConversationSearchMode, after, through int64) error {
	release, err := s.backgroundWrite(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = visitBuildingConversationSearchCandidates(ctx, tx, build.id, after, through, kind, func(_ ConversationSearchCandidate, item EligibleConversationSearchItem, eligible bool) error {
		if !eligible {
			return nil
		}
		_, err := upsertConversationSearchEligibleItemTx(ctx, tx, modes, item)
		return err
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SwitchTranscriptRebuild never indexes or removes whole-thread search rows.
// Those rows are already prepared for B, or become invisible with A.
func (s *Store) SwitchTranscriptRebuild(ctx context.Context, build *TranscriptRebuildHandle) (TranscriptApplyResult, error) {
	if !build.itemsComplete || !build.searchComplete {
		return TranscriptApplyResult{}, errors.New("transcript rebuild is incomplete")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	defer tx.Rollback()
	current, activeID, err := readActiveTranscriptCheckpointTx(ctx, tx, build.threadID, false)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	if activeID != build.activeID || !reflect.DeepEqual(current, build.base) {
		return TranscriptApplyResult{}, fmt.Errorf("active checkpoint changed: %w", ErrTranscriptChanged)
	}
	var thread, state string
	if err := tx.QueryRowContext(ctx, `SELECT thread_id,state FROM conversation_projections WHERE id=?`, build.id).Scan(&thread, &state); err != nil {
		return TranscriptApplyResult{}, err
	}
	if thread != build.threadID || state != "building" {
		return TranscriptApplyResult{}, fmt.Errorf("shadow thread changed: %w", ErrTranscriptChanged)
	}
	statuses, err := conversationSearchStatusesTx(ctx, tx)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	if !sameTranscriptRebuildSearch(build.search, statuses) {
		return TranscriptApplyResult{}, fmt.Errorf("search lifecycle changed: %w", ErrTranscriptChanged)
	}
	count, latest, err := transcriptCounts(ctx, tx, build.id)
	if err != nil {
		return TranscriptApplyResult{}, err
	}
	if err := recordTranscriptItemAliases(ctx, tx, build.threadID, build.activeID, build.id); err != nil {
		return TranscriptApplyResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_projections SET state='superseded' WHERE id=?`, build.activeID); err != nil {
		return TranscriptApplyResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_projections SET state='active',item_count=?,latest_seq=? WHERE id=?`, count, latest, build.id); err != nil {
		return TranscriptApplyResult{}, err
	}
	if err := updateMaintainedConversationSearchStateTx(ctx, tx, statuses, time.Now().UTC()); err != nil {
		return TranscriptApplyResult{}, err
	}
	if build.input.Provider == ProviderGrok {
		if _, err := syncGrokThreadRecency(ctx, tx, build.threadID); err != nil {
			return TranscriptApplyResult{}, err
		}
	}
	if _, err := bumpCatalogVersion(ctx, tx); err != nil {
		return TranscriptApplyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TranscriptApplyResult{}, err
	}
	return TranscriptApplyResult{ProjectionKey: build.input.Result.ProjectionKey, LatestSeq: latest, ItemCount: count, Changed: true}, nil
}

// A building-to-ready change keeps the same maintained mode and its documents.
// Checkpoints, counts and revisions advance independently of this rebuild.
func sameTranscriptRebuildSearch(base, current []ConversationSearchStatus) bool {
	if len(base) != len(current) {
		return false
	}
	for i, previous := range base {
		next := current[i]
		maintained := func(state ConversationSearchBuildState) bool {
			return state == ConversationSearchBuilding || state == ConversationSearchReady
		}
		if previous.Mode != next.Mode || previous.CorpusRuleVersion != next.CorpusRuleVersion || previous.TokenizerVersion != next.TokenizerVersion || !reflect.DeepEqual(previous.StartedAt, next.StartedAt) || maintained(previous.State) != maintained(next.State) {
			return false
		}
	}
	return true
}

func (s *Store) AbandonTranscriptRebuild(ctx context.Context, build *TranscriptRebuildHandle) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE conversation_projections SET state='abandoned' WHERE id=? AND state='building'`, build.id)
	return err
}

// AbandonBuildingTranscripts must commit before either builder or collector runs.
func (s *Store) AbandonBuildingTranscripts(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE conversation_projections SET state='abandoned' WHERE state='building'`)
	return err
}
