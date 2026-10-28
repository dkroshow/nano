package prompts

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// LegacyJournal is the schema-20 compatibility read view of the journal;
// like queuedTurnStore it reads through the transactional store.
type LegacyJournal struct {
	path  string
	db    *sql.DB
	runID string
	turns *Store
}

// NewLegacyJournal constructs a read view over the canonical delivery journal.
func NewLegacyJournal(db *sql.DB, path, runID string, store *Store) (*LegacyJournal, error) {
	if store == nil {
		return nil, fmt.Errorf("threads: send attempt view requires the prompt store")
	}
	return &LegacyJournal{db: db, path: path, runID: runID, turns: store}, nil
}

// Get reads compatibility evidence for an action.
func (s *LegacyJournal) Get(actionID string) (SendAttempt, bool) {
	if s == nil {
		return SendAttempt{}, false
	}
	record, attempt, err := s.turns.Get(context.Background(), actionID)
	if err == nil {
		return LegacySendAttempt(record, attempt), true
	}
	attempt, err = s.turns.GetAttempt(context.Background(), actionID)
	return LegacySendAttempt(Record{}, attempt), err == nil
}

// ListThread reads compatibility records for a thread.
func (s *LegacyJournal) ListThread(provider, nativeThreadID string) []SendAttempt {
	if s == nil || nativeThreadID == "" {
		return nil
	}
	rows := make([]SendAttempt, 0)
	attempts, err := s.turns.ListThreadAttempts(context.Background(), model.Provider(provider), nativeThreadID)
	if err != nil {
		return nil
	}
	for _, entry := range attempts {
		if entry.OutboxState == Expired {
			continue // hidden for good; its journal stays in the store
		}
		record := Record{Outbox: OutboxEntry{State: entry.OutboxState}}
		rows = append(rows, LegacySendAttempt(record, entry.Attempt))
	}
	sort.Slice(rows, func(i, j int) bool { return threadvalues.TimestampBefore(rows[j].UpdatedAt, rows[i].UpdatedAt) })
	return rows
}

// TransactionStore returns the canonical transactional prompt store.
func (s *LegacyJournal) TransactionStore() *Store {
	if s == nil {
		return nil
	}
	return s.turns
}

// BindTransactionStore sets the canonical transactional prompt store.
func (s *LegacyJournal) BindTransactionStore(store *Store) {
	if s != nil {
		s.turns = store
	}
}

// RunIdentity returns the process identity attached to delivery evidence.
func (s *LegacyJournal) RunIdentity() string {
	if s == nil {
		return ""
	}
	return s.runID
}

// Database returns the journal view's canonical database connection.
func (s *LegacyJournal) Database() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Path returns the canonical database path retained by this view.
func (s *LegacyJournal) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}
