package app

import (
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

func (s *completedTurnSource) after(target, cursor string, limit int) ([]completedTurn, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("completed-turn source unavailable")
	}
	return s.store.after(target, cursor, limit)
}
