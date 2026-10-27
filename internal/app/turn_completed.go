package app

import (
	"context"
	"errors"
	turns "github.com/dkroshow/nano/internal/core/turns"
)

// completedTurn is a consumer-independent durable source fact. Consumers keep
// their own cursors; this row deliberately contains no delivery state.
type completedTurn = turns.Record

type completedTurnSource struct{ store *completedTurnStore }

func newCompletedTurnSource(store *completedTurnStore) *completedTurnSource {
	return &completedTurnSource{store: store}
}

// open reports a thread with a provider turn that started and has no
// recorded outcome. A store it cannot read counts as open.
func (s *completedTurnSource) open(target string) bool {
	if s == nil || s.store == nil || target == "" {
		return false
	}
	store, err := s.store.coreStore(context.Background())
	if err != nil {
		return true
	}
	unfinished, err := store.Unfinished(context.Background(), target)
	return unfinished || err != nil
}

func (s *completedTurnSource) after(target, cursor string, limit int) ([]completedTurn, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("completed-turn source unavailable")
	}
	return s.store.after(target, cursor, limit)
}
