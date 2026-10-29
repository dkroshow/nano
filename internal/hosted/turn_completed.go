package hosted

import (
	"context"
	"errors"

	turns "github.com/dkroshow/nano/internal/core/turns"
)

// completedTurn is a consumer-independent durable source fact. Consumers keep
// their own cursors; this row deliberately contains no delivery state.
type completedTurn = turns.Record

// CompletedTurnSource adapts the canonical completed-turn store to the hosted feed.
type CompletedTurnSource struct{ store *CompletedTurnStore }

// NewCompletedTurnSource binds the completed-turn feed to its canonical store.
func NewCompletedTurnSource(store *CompletedTurnStore) *CompletedTurnSource {
	return &CompletedTurnSource{store: store}
}

// Open reports a thread with a provider turn that started and has no
// recorded outcome. A store it cannot read counts as open.
func (s *CompletedTurnSource) Open(target string) bool {
	if s == nil || s.store == nil || target == "" {
		return false
	}
	store, err := s.store.CoreStore(context.Background())
	if err != nil {
		return true
	}
	unfinished, err := store.Unfinished(context.Background(), target)
	return unfinished || err != nil
}

// after reads completed turns after the supplied cursor.
func (s *CompletedTurnSource) after(target, cursor string, limit int) ([]completedTurn, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("completed-turn source unavailable")
	}
	return s.store.after(target, cursor, limit)
}

// Store returns the canonical completed-turn adapter, or nil when unavailable.
func (s *CompletedTurnSource) Store() *CompletedTurnStore { return s.store }
