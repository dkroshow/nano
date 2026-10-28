package app

import (
	"context"
	"database/sql"
	"fmt"

	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	submissionQueued     = string(turns.Queued)
	submissionDelivering = string(turns.Delivering)
	submissionAccepted   = string(turns.Accepted)
	submissionUncertain  = string(turns.Uncertain)
	submissionParked     = string(turns.Parked)
)

// queuedTurnRecord is the schema-20 compatibility shape exposed by the read
// proxy below. Production delivery writes only through internal/core/prompts.Store.
// HostedThreadID is the stable pre-native identity; migrated records may carry
// only NativeThreadID.
type queuedTurnRecord = turns.QueuedTurn

const (
	turnOriginHuman  = string(turns.HumanOrigin)
	turnOriginAgent  = string(turns.AgentOrigin)
	turnOriginSystem = string(turns.SystemOrigin)
)

// queuedTurnStore is the schema-20 compatibility read view of the prompt
// store: every read is answered by the transactional store, so finished
// prompts stay on disk instead of being copied into memory at boot.
type queuedTurnStore struct {
	path      string
	db        *sql.DB
	turns     *turns.Store
	bootParks []turns.BootPark
}

func newQueuedTurnStore(db *sql.DB, path string, store *turns.Store) (*queuedTurnStore, error) {
	if store == nil {
		return nil, fmt.Errorf("threads: queued turn view requires the prompt store")
	}
	return &queuedTurnStore{db: db, path: path, turns: store}, nil
}

// BootParksFor reads prompts parked during boot for the attachment.
func (s *queuedTurnStore) BootParksFor(provider, nativeThreadID, hostedThreadID string) []turns.BootPark {
	if s == nil {
		return nil
	}
	var result []turns.BootPark
	for _, parked := range s.bootParks {
		hostedMatch := hostedThreadID != "" && parked.HostedThreadID == hostedThreadID
		nativeMatch := provider != "" && nativeThreadID != "" && string(parked.Provider) == provider && parked.NativeThreadID == nativeThreadID
		if !hostedMatch && !nativeMatch {
			continue
		}
		copy := parked
		copy.Missing = append([]string(nil), parked.Missing...)
		result = append(result, copy)
	}
	return result
}

// Get reads compatibility evidence for an action.
func (s *queuedTurnStore) Get(actionID string) (queuedTurnRecord, bool) {
	if s == nil {
		return queuedTurnRecord{}, false
	}
	record, _, err := s.turns.Get(context.Background(), actionID)
	return turns.LegacyQueuedTurn(record), err == nil
}

// First reads the next queued prompt for an attachment.
func (s *queuedTurnStore) First(provider, nativeThreadID, hostedThreadID string) (queuedTurnRecord, bool) {
	if s == nil {
		return queuedTurnRecord{}, false
	}
	for _, rec := range s.queued(hostedThreadID) {
		if queuedTurnMatches(rec, provider, nativeThreadID, hostedThreadID) {
			return rec, true
		}
	}
	return queuedTurnRecord{}, false
}

// Position is one-based within the target thread's delivery order: every
// queued record of a higher-priority class, plus earlier records of the same
// class, count ahead of it. It can therefore increase when a human prompt
// arrives behind an agent prompt. Zero means the action is no longer waiting
// in Nano's text-bearing queue.
func (s *queuedTurnStore) Position(actionID string) int {
	if s == nil {
		return 0
	}
	target, ok := s.Get(actionID)
	if !ok || target.State != submissionQueued {
		return 0
	}
	// Queued rows come in delivery order, so the ones ahead come first.
	position := 1
	for _, record := range s.queued(target.HostedThreadID) {
		if record.ClientActionID == actionID {
			return position
		}
		if sameQueuedThread(record, target) {
			position++
		}
	}
	return 0
}

// ByProviderTurn reads a prompt bound to the named provider turn.
func (s *queuedTurnStore) ByProviderTurn(provider, nativeThreadID, hostedThreadID, turnID string) (queuedTurnRecord, bool) {
	if s == nil || turnID == "" {
		return queuedTurnRecord{}, false
	}
	records, err := s.turns.ProviderTurn(context.Background(), hostedThreadID, model.Provider(provider), nativeThreadID, turnID)
	if err != nil || len(records) != 1 {
		return queuedTurnRecord{}, false
	}
	return turns.LegacyQueuedTurn(records[0]), true
}

// ListThread reads compatibility records for a thread.
func (s *queuedTurnStore) ListThread(provider, nativeThreadID, hostedThreadID string) []queuedTurnRecord {
	return s.ListThreadFrom(nil, provider, nativeThreadID, hostedThreadID)
}

// ListThreadFrom is listThread read through journal, or through the store's
// writer connection when journal is nil.
func (s *queuedTurnStore) ListThreadFrom(journal promptJournal, provider, nativeThreadID, hostedThreadID string) []queuedTurnRecord {
	if s == nil {
		return nil
	}
	if journal == nil {
		journal = s.turns
	}
	rows := make([]queuedTurnRecord, 0)
	if hostedThreadID == "" {
		return rows // every transactional prompt has an owning thread
	}
	records, err := journal.List(context.Background(), hostedThreadID)
	if err != nil {
		return rows
	}
	for _, record := range records {
		if rec := turns.LegacyQueuedTurn(record); queuedTurnMatches(rec, provider, nativeThreadID, hostedThreadID) {
			rows = append(rows, rec)
		}
	}
	return rows
}

func queuedTurnMatches(rec queuedTurnRecord, provider, nativeThreadID, hostedThreadID string) bool {
	if rec.Provider != provider {
		return false
	}
	if hostedThreadID != "" && rec.HostedThreadID != "" {
		return rec.HostedThreadID == hostedThreadID
	}
	return nativeThreadID != "" && rec.NativeThreadID == nativeThreadID
}

func sameQueuedThread(left, right queuedTurnRecord) bool {
	if left.Provider != right.Provider {
		return false
	}
	if left.HostedThreadID != "" && right.HostedThreadID != "" {
		return left.HostedThreadID == right.HostedThreadID
	}
	return left.NativeThreadID != "" && left.NativeThreadID == right.NativeThreadID
}

// QueuedCount is the number of records still queued for the target thread.
// Availability turns it into busy/queued_backlog for every listed thread, and
// heartbeat wake gates and idle checks read that, so it reads only the
// thread's own queued prompts.
func (s *queuedTurnStore) QueuedCount(provider, nativeThreadID, hostedThreadID string) int {
	if s == nil {
		return 0
	}
	n := 0
	for _, rec := range s.queued(hostedThreadID) {
		if queuedTurnMatches(rec, provider, nativeThreadID, hostedThreadID) {
			n++
		}
	}
	return n
}

// queued is the thread's queued prompts in delivery order. Every
// transactional prompt has an owning thread, so an empty owner has none.
func (s *queuedTurnStore) queued(hostedThreadID string) []queuedTurnRecord {
	if hostedThreadID == "" {
		return nil
	}
	records, err := s.turns.Queued(context.Background(), hostedThreadID)
	if err != nil {
		return nil
	}
	result := make([]queuedTurnRecord, 0, len(records))
	for _, record := range records {
		result = append(result, turns.LegacyQueuedTurn(record))
	}
	return result
}

// TransactionStore returns the canonical transactional prompt store.
func (s *queuedTurnStore) TransactionStore() *turns.Store {
	if s == nil {
		return nil
	}
	return s.turns
}

// BindTransactionStore sets the canonical transactional prompt store.
func (s *queuedTurnStore) BindTransactionStore(store *turns.Store) {
	if s != nil {
		s.turns = store
	}
}

// Database returns the prompt view’s database connection.
func (s *queuedTurnStore) Database() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}
