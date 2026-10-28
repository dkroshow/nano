package app

import (
	"database/sql"
	"os"
	"path/filepath"
	"time"
)

func recordLegacyCompletedTurn(store *legacyCompletedStore, target, provider, turnID, finalID string, at time.Time, source string) error {
	stamp := at.UTC().Format(time.RFC3339Nano)
	_, err := store.db.Exec(`INSERT INTO completed_turns(target_hosted_thread_id,provider,turn_id,final_message_id,completed_at,source,created_at) VALUES(?,?,?,?,?,?,?)`, target, provider, turnID, finalID, stamp, source, stamp)
	return err
}

func openCompletedTurnStore(path string) (*legacyCompletedStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode = WAL; PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	s := &legacyCompletedStore{db: db}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS completed_turns (id INTEGER PRIMARY KEY AUTOINCREMENT,target_hosted_thread_id TEXT NOT NULL,provider TEXT NOT NULL,turn_id TEXT NOT NULL,final_message_id TEXT NOT NULL,completed_at TEXT NOT NULL,source TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(target_hosted_thread_id,turn_id)); CREATE INDEX IF NOT EXISTS completed_turns_target_order ON completed_turns(target_hosted_thread_id,id);`); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

type legacyCompletedStore struct{ db *sql.DB }

func (s *legacyCompletedStore) Close() error { return s.db.Close() }
