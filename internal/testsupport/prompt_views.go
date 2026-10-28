package testsupport

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	turns "github.com/dkroshow/nano/internal/core/prompts"
)

func NewQueue(t testing.TB, path string) (*turns.LegacyQueue, error) {
	t.Helper()
	db, path, err := TurnDatabase(t, path)
	if err != nil {
		return nil, err
	}
	return turns.NewLegacyQueue(db, path, PromptStore(t, db))
}

func NewJournal(t testing.TB, path, runID string) (*turns.LegacyJournal, error) {
	t.Helper()
	db, path, err := TurnDatabase(t, path)
	if err != nil {
		return nil, err
	}
	return turns.NewLegacyJournal(db, path, runID, PromptStore(t, db))
}

func MustQueue(t testing.TB) *turns.LegacyQueue {
	t.Helper()
	store, err := NewQueue(t, filepath.Join(t.TempDir(), "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func BindPromptStore(t testing.TB, queue interface {
	Database() *sql.DB
	BindTransactionStore(*turns.Store)
}, attempts interface{ BindTransactionStore(*turns.Store) }) *turns.Store {
	t.Helper()
	if queue == nil || attempts == nil {
		t.Fatal("transactional turn fixture requires queue and journal views")
	}
	store, err := turns.OpenStore(context.Background(), queue.Database())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdoptLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue.BindTransactionStore(store)
	attempts.BindTransactionStore(store)
	return store
}
