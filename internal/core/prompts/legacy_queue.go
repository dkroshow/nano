package prompts

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// LegacyQueue is the schema-20 compatibility read view of the prompt
// store: every read is answered by the transactional store, so finished
// prompts stay on disk instead of being copied into memory at boot.
type LegacyQueue struct {
	path      string
	db        *sql.DB
	turns     *Store
	bootParks []BootPark
}

// NewLegacyQueue constructs a read view over the canonical prompt store.
func NewLegacyQueue(db *sql.DB, path string, store *Store) (*LegacyQueue, error) {
	if store == nil {
		return nil, fmt.Errorf("threads: queued turn view requires the prompt store")
	}
	return &LegacyQueue{db: db, path: path, turns: store}, nil
}

// BootParksFor reads prompts parked during boot for the attachment.
func (s *LegacyQueue) BootParksFor(provider, nativeThreadID, hostedThreadID string) []BootPark {
	if s == nil {
		return nil
	}
	var result []BootPark
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
func (s *LegacyQueue) Get(actionID string) (QueuedTurn, bool) {
	if s == nil {
		return QueuedTurn{}, false
	}
	record, _, err := s.turns.Get(context.Background(), actionID)
	return LegacyQueuedTurn(record), err == nil
}

// First reads the next queued prompt for an attachment.
func (s *LegacyQueue) First(provider, nativeThreadID, hostedThreadID string) (QueuedTurn, bool) {
	if s == nil {
		return QueuedTurn{}, false
	}
	for _, rec := range s.queued(hostedThreadID) {
		if queuedTurnMatches(rec, provider, nativeThreadID, hostedThreadID) {
			return rec, true
		}
	}
	return QueuedTurn{}, false
}

// Position is one-based within the target thread's delivery order: every
// queued record of a higher-priority class, plus earlier records of the same
// class, count ahead of it. It can therefore increase when a human prompt
// arrives behind an agent prompt. Zero means the action is no longer waiting
// in Nano's text-bearing queue.
func (s *LegacyQueue) Position(actionID string) int {
	if s == nil {
		return 0
	}
	target, ok := s.Get(actionID)
	if !ok || target.State != string(Queued) {
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
func (s *LegacyQueue) ByProviderTurn(provider, nativeThreadID, hostedThreadID, turnID string) (QueuedTurn, bool) {
	if s == nil || turnID == "" {
		return QueuedTurn{}, false
	}
	records, err := s.turns.ProviderTurn(context.Background(), hostedThreadID, model.Provider(provider), nativeThreadID, turnID)
	if err != nil || len(records) != 1 {
		return QueuedTurn{}, false
	}
	return LegacyQueuedTurn(records[0]), true
}

// ListThread reads compatibility records for a thread.
func (s *LegacyQueue) ListThread(provider, nativeThreadID, hostedThreadID string) []QueuedTurn {
	return s.ListThreadFrom(nil, provider, nativeThreadID, hostedThreadID)
}

// ListThreadFrom is listThread read through journal, or through the store's
// writer connection when journal is nil.
func (s *LegacyQueue) ListThreadFrom(journal JournalReader, provider, nativeThreadID, hostedThreadID string) []QueuedTurn {
	if s == nil {
		return nil
	}
	if journal == nil {
		journal = s.turns
	}
	rows := make([]QueuedTurn, 0)
	if hostedThreadID == "" {
		return rows // every transactional prompt has an owning thread
	}
	records, err := journal.List(context.Background(), hostedThreadID)
	if err != nil {
		return rows
	}
	for _, record := range records {
		if rec := LegacyQueuedTurn(record); queuedTurnMatches(rec, provider, nativeThreadID, hostedThreadID) {
			rows = append(rows, rec)
		}
	}
	return rows
}

func queuedTurnMatches(rec QueuedTurn, provider, nativeThreadID, hostedThreadID string) bool {
	if rec.Provider != provider {
		return false
	}
	if hostedThreadID != "" && rec.HostedThreadID != "" {
		return rec.HostedThreadID == hostedThreadID
	}
	return nativeThreadID != "" && rec.NativeThreadID == nativeThreadID
}

func sameQueuedThread(left, right QueuedTurn) bool {
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
func (s *LegacyQueue) QueuedCount(provider, nativeThreadID, hostedThreadID string) int {
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
func (s *LegacyQueue) queued(hostedThreadID string) []QueuedTurn {
	if hostedThreadID == "" {
		return nil
	}
	records, err := s.turns.Queued(context.Background(), hostedThreadID)
	if err != nil {
		return nil
	}
	result := make([]QueuedTurn, 0, len(records))
	for _, record := range records {
		result = append(result, LegacyQueuedTurn(record))
	}
	return result
}

// TransactionStore returns the canonical transactional prompt store.
func (s *LegacyQueue) TransactionStore() *Store {
	if s == nil {
		return nil
	}
	return s.turns
}

// BindTransactionStore sets the canonical transactional prompt store.
func (s *LegacyQueue) BindTransactionStore(store *Store) {
	if s != nil {
		s.turns = store
	}
}

// Database returns the prompt view’s database connection.
func (s *LegacyQueue) Database() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Path returns the canonical database path retained by this view.
func (s *LegacyQueue) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// SetBootParks supplies the boot migration's parked-prompt evidence.
func (s *LegacyQueue) SetBootParks(parks []BootPark) {
	if s != nil {
		s.bootParks = parks
	}
}
