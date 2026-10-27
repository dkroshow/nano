// Package feedback keeps the operator's feedback notes. Each note carries a
// snapshot of the content-free browser evidence and pane state of the moment it
// was saved, so a later restart cannot erase what the operator saw. Notes are
// only stored; saving one notifies no agent.
package feedback

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

var (
	ErrNotFound           = errors.New("feedback note not found")
	ErrClientActionReused = errors.New("clientActionId was already used for a different feedback note")
)

type Thread struct {
	HostedThreadID string `json:"hostedThreadId,omitempty"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	Provider       string `json:"provider,omitempty"`
}

// Build names both sides: the binary and bundle Nano was serving, and the
// bundle the tab had actually loaded. A stale tab shows as a difference.
type Build struct {
	Stamp     string `json:"stamp"`
	Bundle    string `json:"bundle"`
	TabBundle string `json:"tabBundle,omitempty"`
}

type Pane struct {
	Working        bool   `json:"working"`
	PendingPrompts int    `json:"pendingPrompts"`
	LastSavedRowID string `json:"lastSavedRowId,omitempty"`
}

type Browser struct {
	TabID  string `json:"tabId,omitempty"`
	PageID string `json:"pageId,omitempty"`
	PaneID string `json:"paneId,omitempty"`
}

type Note struct {
	ID         string  `json:"id"`
	CreatedAt  string  `json:"createdAt"`
	Text       string  `json:"note"`
	Thread     Thread  `json:"thread"`
	Build      Build   `json:"build"`
	View       string  `json:"view,omitempty"`
	Pane       Pane    `json:"pane"`
	Browser    Browser `json:"browser"`
	EventCount int     `json:"eventCount"`
	// Events is the tab's diagnostic events for this thread, exactly as the
	// browser diagnostics store held them at save time. Lists omit it.
	Events json.RawMessage `json:"events,omitempty"`
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("feedback database path is required")
	}
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS feedback_notes (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  feedback_id TEXT NOT NULL UNIQUE,
  client_action_id TEXT NOT NULL UNIQUE,
  request_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  note TEXT NOT NULL,
  hosted_thread_id TEXT NOT NULL DEFAULT '',
  native_thread_id TEXT NOT NULL DEFAULT '',
  display_name TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  build_stamp TEXT NOT NULL DEFAULT '',
  build_bundle TEXT NOT NULL DEFAULT '',
  tab_bundle TEXT NOT NULL DEFAULT '',
  view TEXT NOT NULL DEFAULT '',
  pane_working INTEGER NOT NULL DEFAULT 0,
  pane_pending_prompts INTEGER NOT NULL DEFAULT 0,
  pane_last_saved_row_id TEXT NOT NULL DEFAULT '',
  tab_id TEXT NOT NULL DEFAULT '',
  page_id TEXT NOT NULL DEFAULT '',
  pane_id TEXT NOT NULL DEFAULT '',
  event_count INTEGER NOT NULL DEFAULT 0,
  events_json TEXT NOT NULL DEFAULT '[]',
  -- The triage columns belong to a retired prompt relay. They keep the delivery
  -- history of notes saved while it ran; new notes record 'none'.
  triage_thread_ref TEXT NOT NULL DEFAULT '',
  triage_state TEXT NOT NULL CHECK(triage_state IN ('pending','sent','failed','none')),
  triage_turn_id TEXT NOT NULL DEFAULT '',
  triage_attempts INTEGER NOT NULL DEFAULT 0,
  triage_error TEXT NOT NULL DEFAULT ''
)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate feedback database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func NewID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return "fb_" + hex.EncodeToString(raw[:])
}

// Create stores a new note. A retried clientActionId with the same request
// hash returns the original note unchanged; with another hash it is refused.
func (s *Store) Create(note Note, clientActionID, requestHash string) (Note, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var existingID, existingHash string
	err := s.db.QueryRow(`SELECT feedback_id, request_hash FROM feedback_notes WHERE client_action_id = ?`, clientActionID).Scan(&existingID, &existingHash)
	if err == nil {
		if existingHash != requestHash {
			return Note{}, false, ErrClientActionReused
		}
		existing, err := s.Get(existingID)
		return existing, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Note{}, false, err
	}
	if len(note.Events) == 0 {
		note.Events = json.RawMessage(`[]`)
	}
	_, err = s.db.Exec(`INSERT INTO feedback_notes(feedback_id, client_action_id, request_hash, created_at, note,
hosted_thread_id, native_thread_id, display_name, provider, build_stamp, build_bundle, tab_bundle, view,
pane_working, pane_pending_prompts, pane_last_saved_row_id, tab_id, page_id, pane_id, event_count, events_json,
triage_state) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'none')`,
		note.ID, clientActionID, requestHash, note.CreatedAt, note.Text,
		note.Thread.HostedThreadID, note.Thread.NativeThreadID, note.Thread.DisplayName, note.Thread.Provider,
		note.Build.Stamp, note.Build.Bundle, note.Build.TabBundle, note.View,
		note.Pane.Working, note.Pane.PendingPrompts, note.Pane.LastSavedRowID,
		note.Browser.TabID, note.Browser.PageID, note.Browser.PaneID, note.EventCount, string(note.Events))
	if err != nil {
		return Note{}, false, err
	}
	return note, true, nil
}

const noteColumns = `feedback_id, created_at, note, hosted_thread_id, native_thread_id, display_name, provider,
build_stamp, build_bundle, tab_bundle, view, pane_working, pane_pending_prompts, pane_last_saved_row_id,
tab_id, page_id, pane_id, event_count`

func scanNote(row interface{ Scan(...any) error }, extra ...any) (Note, error) {
	var n Note
	err := row.Scan(append([]any{&n.ID, &n.CreatedAt, &n.Text, &n.Thread.HostedThreadID, &n.Thread.NativeThreadID,
		&n.Thread.DisplayName, &n.Thread.Provider, &n.Build.Stamp, &n.Build.Bundle, &n.Build.TabBundle, &n.View,
		&n.Pane.Working, &n.Pane.PendingPrompts, &n.Pane.LastSavedRowID, &n.Browser.TabID, &n.Browser.PageID,
		&n.Browser.PaneID, &n.EventCount}, extra...)...)
	return n, err
}

// Get returns one note with its evidence.
func (s *Store) Get(id string) (Note, error) {
	var events string
	note, err := scanNote(s.db.QueryRow(`SELECT `+noteColumns+`, events_json FROM feedback_notes WHERE feedback_id = ?`, id), &events)
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	note.Events = json.RawMessage(events)
	return note, err
}

// List returns notes newest first without their evidence. threadID matches
// either the hosted or the native thread id.
func (s *Store) List(threadID string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	where, args := "1=1", []any{}
	if threadID != "" {
		where, args = "(hosted_thread_id = ? OR native_thread_id = ?)", []any{threadID, threadID}
	}
	rows, err := s.db.Query(`SELECT `+noteColumns+` FROM feedback_notes WHERE `+where+` ORDER BY seq DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []Note{}
	for rows.Next() {
		note, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}
