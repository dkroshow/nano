package hosted

import (
	"context"
	"database/sql"
	"sync"

	turns "github.com/dkroshow/nano/internal/core/turns"
)

// CompletedTurnStore adapts hosted completed-turn reads to the canonical turn owner.
type CompletedTurnStore struct {
	db    *sql.DB
	store *turns.Store
	mu    sync.Mutex
}

// NewCompletedTurnStore opens completed-turn views on the canonical database.
func NewCompletedTurnStore(db *sql.DB) *CompletedTurnStore { return &CompletedTurnStore{db: db} }

func (s *CompletedTurnStore) after(target, bookmark string, limit int) ([]turns.Record, error) {
	store, err := s.CoreStore(context.Background())
	if err != nil {
		return nil, err
	}
	return store.After(context.Background(), target, bookmark, limit)
}

// CoreStore opens the canonical completed-turn repository on the shared database.
func (s *CompletedTurnStore) CoreStore(ctx context.Context) (*turns.Store, error) {
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
