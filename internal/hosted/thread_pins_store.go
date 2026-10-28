package hosted

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	nanofiles "github.com/dkroshow/nano/internal/files"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

// PinStore owns append-only hosted transcript pins and their read counters.
type PinStore struct {
	db *sql.DB
	mu sync.Mutex
}

// OpenPinStore opens the canonical pin file and its observation tables.
func OpenPinStore(path string) (*PinStore, error) {
	path = strings.TrimSpace(path)
	absolute, err := nanofiles.CanonicalProspective(path)
	if err != nil {
		return nil, fmt.Errorf("resolve thread pins database: %w", err)
	}
	db, err := statedb.Open(context.Background(), absolute)
	if err != nil {
		return nil, err
	}
	// The observation tables belong to the monitor. They count reads; they
	// never reference pins by foreign key and never change a pin row.
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS thread_pins (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  pin_id TEXT NOT NULL UNIQUE,
  hosted_thread_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  native_thread_id TEXT NOT NULL,
  anchor_item_id TEXT NOT NULL,
  anchor_turn_id TEXT NOT NULL,
  anchor_type TEXT NOT NULL,
  anchor_at TEXT,
  anchor_preview TEXT NOT NULL,
  text TEXT NOT NULL,
  commentary TEXT NOT NULL DEFAULT '',
  creator TEXT NOT NULL DEFAULT '',
  creator_hosted_thread_id TEXT NOT NULL DEFAULT '',
  creator_native_thread_id TEXT NOT NULL DEFAULT '',
  creator_display_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  client_action_id TEXT,
  request_hash TEXT NOT NULL DEFAULT '',
  UNIQUE(hosted_thread_id, client_action_id)
);
CREATE INDEX IF NOT EXISTS thread_pins_hosted ON thread_pins(hosted_thread_id, seq DESC);
CREATE INDEX IF NOT EXISTS thread_pins_native ON thread_pins(provider, native_thread_id, seq DESC);
CREATE TABLE IF NOT EXISTS thread_pin_context_reads (
  pin_id TEXT PRIMARY KEY,
  reads INTEGER NOT NULL,
  broken_reads INTEGER NOT NULL,
  rank_total INTEGER NOT NULL,
  max_rank INTEGER NOT NULL,
  first_read_at TEXT NOT NULL,
  last_read_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS thread_pin_list_reads (
  hosted_thread_id TEXT PRIMARY KEY,
  calls INTEGER NOT NULL,
  continued_calls INTEGER NOT NULL,
  max_depth INTEGER NOT NULL,
  last_read_at TEXT NOT NULL
)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate thread pins database: %w", err)
	}
	return &PinStore{db: db}, nil
}

// Close releases the canonical pin file.
func (s *PinStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

type threadPinRecord struct {
	Seq                   int64
	PinID                 string
	HostedThreadID        string
	Provider              string
	NativeThreadID        string
	AnchorItemID          string
	AnchorTurnID          string
	AnchorType            string
	AnchorAt              string
	AnchorPreview         string
	Text                  string
	Commentary            string
	Creator               string
	CreatorHostedThreadID string
	CreatorNativeThreadID string
	CreatorDisplayName    string
	CreatedAt             string
	ClientActionID        string
	RequestHash           string
}

const threadPinColumns = `seq, pin_id, hosted_thread_id, provider, native_thread_id, anchor_item_id,
anchor_turn_id, anchor_type, COALESCE(anchor_at, ''), anchor_preview, text, commentary, creator,
creator_hosted_thread_id, creator_native_thread_id, creator_display_name, created_at,
COALESCE(client_action_id, ''), request_hash`

func scanThreadPin(row interface{ Scan(...any) error }) (threadPinRecord, error) {
	var p threadPinRecord
	err := row.Scan(&p.Seq, &p.PinID, &p.HostedThreadID, &p.Provider, &p.NativeThreadID, &p.AnchorItemID,
		&p.AnchorTurnID, &p.AnchorType, &p.AnchorAt, &p.AnchorPreview, &p.Text, &p.Commentary, &p.Creator,
		&p.CreatorHostedThreadID, &p.CreatorNativeThreadID, &p.CreatorDisplayName, &p.CreatedAt,
		&p.ClientActionID, &p.RequestHash)
	return p, err
}

// append inserts a new pin. A retried clientActionId with the same request
// returns the original pin; with a different request it is refused.
func (s *PinStore) append(pin threadPinRecord) (threadPinRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pin.ClientActionID != "" {
		existing, err := s.byClientActionLocked(pin.HostedThreadID, pin.ClientActionID)
		if err == nil {
			if existing.RequestHash != pin.RequestHash {
				return threadPinRecord{}, false, errPinIdempotencyReused
			}
			return existing, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return threadPinRecord{}, false, err
		}
	}
	var clientActionID any
	if pin.ClientActionID != "" {
		clientActionID = pin.ClientActionID
	}
	result, err := s.db.Exec(`INSERT INTO thread_pins(pin_id, hosted_thread_id, provider, native_thread_id,
anchor_item_id, anchor_turn_id, anchor_type, anchor_at, anchor_preview, text, commentary, creator,
creator_hosted_thread_id, creator_native_thread_id, creator_display_name, created_at, client_action_id, request_hash)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		pin.PinID, pin.HostedThreadID, pin.Provider, pin.NativeThreadID, pin.AnchorItemID, pin.AnchorTurnID,
		pin.AnchorType, pinNullable(pin.AnchorAt), pin.AnchorPreview, pin.Text, pin.Commentary, pin.Creator,
		pin.CreatorHostedThreadID, pin.CreatorNativeThreadID, pin.CreatorDisplayName, pin.CreatedAt,
		clientActionID, pin.RequestHash)
	if err != nil {
		return threadPinRecord{}, false, err
	}
	pin.Seq, _ = result.LastInsertId()
	return pin, true, nil
}

func pinNullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// threadPinScope is the set of pins that belong to one hosted thread: pins
// written under its hosted id, plus pins anchored in any native id it has ever
// claimed.
type threadPinScope struct {
	HostedThreadID string
	Provider       string
	NativeIDs      []string
}

func (scope threadPinScope) where() (string, []any) {
	clause := `(hosted_thread_id = ?`
	args := []any{scope.HostedThreadID}
	if len(scope.NativeIDs) > 0 {
		clause += ` OR (provider = ? AND native_thread_id IN (?` + strings.Repeat(`,?`, len(scope.NativeIDs)-1) + `))`
		args = append(args, scope.Provider)
		for _, id := range scope.NativeIDs {
			args = append(args, id)
		}
	}
	return clause + `)`, args
}

// list returns up to limit pins older than beforeSeq (0 = newest), newest
// first, and whether more exist.
func (s *PinStore) list(scope threadPinScope, beforeSeq int64, limit int) ([]threadPinRecord, bool, error) {
	where, args := scope.where()
	if beforeSeq > 0 {
		where += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`SELECT `+threadPinColumns+` FROM thread_pins WHERE `+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	pins := make([]threadPinRecord, 0, limit)
	for rows.Next() {
		pin, err := scanThreadPin(rows)
		if err != nil {
			return nil, false, err
		}
		pins = append(pins, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(pins) > limit
	if more {
		pins = pins[:limit]
	}
	return pins, more, nil
}

func (s *PinStore) get(scope threadPinScope, pinID string) (threadPinRecord, error) {
	where, args := scope.where()
	args = append(args, pinID)
	pin, err := scanThreadPin(s.db.QueryRow(`SELECT `+threadPinColumns+` FROM thread_pins WHERE `+where+` AND pin_id = ?`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return threadPinRecord{}, errPinNotFound
	}
	return pin, err
}

// rank is the pin's 1-based newest-first position within its scope.
func (s *PinStore) rank(scope threadPinScope, seq int64) (int, error) {
	where, args := scope.where()
	args = append(args, seq)
	var rank int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_pins WHERE `+where+` AND seq >= ?`, args...).Scan(&rank)
	return rank, err
}

// Observation writes are best-effort: a failed count must never fail the read
// that an agent is relying on.
func (s *PinStore) observeList(hostedThreadID string, continued bool, depth int) {
	cont := 0
	if continued {
		cont = 1
	}
	_, _ = s.db.Exec(`INSERT INTO thread_pin_list_reads(hosted_thread_id, calls, continued_calls, max_depth, last_read_at)
VALUES (?, 1, ?, ?, ?)
ON CONFLICT(hosted_thread_id) DO UPDATE SET calls = calls + 1, continued_calls = continued_calls + excluded.continued_calls,
  max_depth = MAX(max_depth, excluded.max_depth), last_read_at = excluded.last_read_at`,
		hostedThreadID, cont, depth, time.Now().UTC().Format(time.RFC3339Nano))
}

func (s *PinStore) observeContext(pinID string, rank int, broken bool) {
	brokenCount := 0
	if broken {
		brokenCount = 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.Exec(`INSERT INTO thread_pin_context_reads(pin_id, reads, broken_reads, rank_total, max_rank, first_read_at, last_read_at)
VALUES (?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(pin_id) DO UPDATE SET reads = reads + 1, broken_reads = broken_reads + excluded.broken_reads,
  rank_total = rank_total + excluded.rank_total, max_rank = MAX(max_rank, excluded.max_rank),
  last_read_at = excluded.last_read_at`, pinID, brokenCount, rank, rank, now, now)
}

// clientActionId is scoped to one hosted thread: two threads may reuse a key.
func (s *PinStore) byClientAction(hostedThreadID, clientActionID string) (threadPinRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byClientActionLocked(hostedThreadID, clientActionID)
}

func (s *PinStore) byClientActionLocked(hostedThreadID, clientActionID string) (threadPinRecord, error) {
	return scanThreadPin(s.db.QueryRow(`SELECT `+threadPinColumns+` FROM thread_pins WHERE hosted_thread_id = ? AND client_action_id = ?`,
		hostedThreadID, clientActionID))
}

func newThreadPinID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return "pin_" + hex.EncodeToString(raw[:])
}
