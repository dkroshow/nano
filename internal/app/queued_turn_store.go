package app

import (
	"context"
	"database/sql"
	"fmt"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	submissionQueued     = "queued"
	submissionDelivering = "delivering"
	submissionAccepted   = "accepted"
	submissionUncertain  = "uncertain"
	submissionParked     = "parked"
)

// queuedTurnRecord is the schema-20 compatibility shape exposed by the read
// proxy below. Production delivery writes only through internal/core/prompts.Store.
// HostedThreadID is the stable pre-native identity; migrated records may carry
// only NativeThreadID.
type queuedTurnRecord = turns.QueuedTurn

const (
	turnOriginHuman  = "human"
	turnOriginAgent  = "agent"
	turnOriginSystem = "system"
)

func normalizeTurnOrigin(origin string) string {
	switch origin {
	case turnOriginAgent, turnOriginSystem:
		return origin
	default:
		return turnOriginHuman
	}
}

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

func (s *queuedTurnStore) bootParksFor(provider, nativeThreadID, hostedThreadID string) []turns.BootPark {
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

func (s *queuedTurnStore) get(actionID string) (queuedTurnRecord, bool) {
	if s == nil {
		return queuedTurnRecord{}, false
	}
	record, _, err := s.turns.Get(context.Background(), actionID)
	return queuedTurnRecordFromTransactional(record), err == nil
}

func (s *queuedTurnStore) first(provider, nativeThreadID, hostedThreadID string) (queuedTurnRecord, bool) {
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

// position is one-based within the target thread's delivery order: every
// queued record of a higher-priority class, plus earlier records of the same
// class, count ahead of it. It can therefore increase when a human prompt
// arrives behind an agent prompt. Zero means the action is no longer waiting
// in Nano's text-bearing queue.
func (s *queuedTurnStore) position(actionID string) int {
	if s == nil {
		return 0
	}
	target, ok := s.get(actionID)
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

func (s *queuedTurnStore) byProviderTurn(provider, nativeThreadID, hostedThreadID, turnID string) (queuedTurnRecord, bool) {
	if s == nil || turnID == "" {
		return queuedTurnRecord{}, false
	}
	records, err := s.turns.ProviderTurn(context.Background(), hostedThreadID, model.Provider(provider), nativeThreadID, turnID)
	if err != nil || len(records) != 1 {
		return queuedTurnRecord{}, false
	}
	return queuedTurnRecordFromTransactional(records[0]), true
}

func (s *queuedTurnStore) listThread(provider, nativeThreadID, hostedThreadID string) []queuedTurnRecord {
	return s.listThreadFrom(nil, provider, nativeThreadID, hostedThreadID)
}

// listThreadFrom is listThread read through journal, or through the store's
// writer connection when journal is nil.
func (s *queuedTurnStore) listThreadFrom(journal promptJournal, provider, nativeThreadID, hostedThreadID string) []queuedTurnRecord {
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
		if rec := queuedTurnRecordFromTransactional(record); queuedTurnMatches(rec, provider, nativeThreadID, hostedThreadID) {
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

// queuedCount is the number of records still queued for the target thread.
// Availability turns it into busy/queued_backlog for every listed thread, and
// heartbeat wake gates and idle checks read that, so it reads only the
// thread's own queued prompts.
func (s *queuedTurnStore) queuedCount(provider, nativeThreadID, hostedThreadID string) int {
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
		result = append(result, queuedTurnRecordFromTransactional(record))
	}
	return result
}

func queuedTurnRecordFromTransactional(record turns.Record) queuedTurnRecord {
	providerTurnID := record.ProviderTurnID
	if providerTurnID == "" && record.Outbox.Binding != nil && record.Outbox.Binding.Scope.NativeTurnID != nil {
		providerTurnID = *record.Outbox.Binding.Scope.NativeTurnID
	}
	return queuedTurnRecord{
		ClientActionID: record.Outbox.ActionID, Provider: string(record.Provider), HostedThreadID: record.Outbox.HostedThreadID,
		NativeThreadID: record.NativeThreadID, Text: record.ProviderText, PresentationText: record.Outbox.PresentationText,
		TextHash: record.TextHash, RequestHash: record.RequestHash, State: string(record.Outbox.State), Origin: string(record.Origin),
		CreatedAt: record.CreatedAt, ProviderTurnID: providerTurnID,
		ProviderTerminalStatus: record.ProviderTerminal, ProviderTerminalAt: record.ProviderTerminalAt,
	}
}

func (s *queuedTurnStore) transactionStore() *turns.Store {
	if s == nil {
		return nil
	}
	return s.turns
}
func (s *queuedTurnStore) bindTransactionStore(store *turns.Store) {
	if s != nil {
		s.turns = store
	}
}
func (s *queuedTurnStore) database() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}
