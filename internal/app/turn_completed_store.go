package app

import (
	"context"
	"database/sql"
	turns "github.com/dkroshow/nano/internal/core/turns"
	"sync"
)

type completedTurnStore struct {
	db    *sql.DB
	store *turns.Store
	mu    sync.Mutex
}

func (s *completedTurnStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
func (s *completedTurnStore) latest(target string) (completedTurn, error) {
	store, err := s.coreStore(context.Background())
	if err != nil {
		return completedTurn{}, err
	}
	return store.Latest(context.Background(), target)
}
func (s *completedTurnStore) after(target, bookmark string, limit int) ([]completedTurn, error) {
	store, err := s.coreStore(context.Background())
	if err != nil {
		return nil, err
	}
	return store.After(context.Background(), target, bookmark, limit)
}

func (s *completedTurnStore) coreStore(ctx context.Context) (*turns.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store, nil
	}
	store, err := turns.OpenStore(ctx, s.db)
	if err != nil {
		return nil, err
	}
	s.store = store
	return store, nil
}
