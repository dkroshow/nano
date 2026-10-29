package sidebar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	nanofiles "github.com/dkroshow/nano/internal/files"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

const threadStarKeySep = "\x00"

// Stars owns the operator-pinned thread identities and their durable database.
type Stars struct {
	db     *sql.DB
	mu     sync.Mutex
	cache  map[string]bool
	loaded bool
}

// OpenStars opens the operator-owned stars database.
func OpenStars(path string) (*Stars, error) {
	path = strings.TrimSpace(path)
	absolute, err := nanofiles.CanonicalProspective(path)
	if err != nil {
		return nil, fmt.Errorf("resolve thread stars database: %w", err)
	}
	db, err := statedb.Open(context.Background(), absolute)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS thread_stars (
			provider TEXT NOT NULL,
			native_thread_id TEXT NOT NULL,
			starred_at TEXT NOT NULL,
			PRIMARY KEY (provider, native_thread_id)
		)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate thread stars database: %w", err)
	}
	return &Stars{db: db, cache: map[string]bool{}}, nil
}

// Close releases the stars database.
func (s *Stars) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func threadStarKey(provider, nativeID string) string {
	return strings.TrimSpace(provider) + threadStarKeySep + strings.TrimSpace(nativeID)
}

func (s *Stars) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	rows, err := s.db.Query(`SELECT provider, native_thread_id FROM thread_stars`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := map[string]bool{}
	for rows.Next() {
		var provider, nativeID string
		if err := rows.Scan(&provider, &nativeID); err != nil {
			return err
		}
		next[threadStarKey(provider, nativeID)] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.cache = next
	s.loaded = true
	return nil
}

// IsStarred reads whether one provider-native identity is pinned.
func (s *Stars) IsStarred(provider, nativeID string) bool {
	if s == nil {
		return false
	}
	provider = strings.TrimSpace(provider)
	nativeID = strings.TrimSpace(nativeID)
	if provider == "" || nativeID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return false
	}
	return s.cache[threadStarKey(provider, nativeID)]
}

// SetStarred changes one operator pin and its read cache atomically.
func (s *Stars) SetStarred(provider, nativeID string, starred bool) error {
	if s == nil {
		return errors.New("thread stars store unavailable")
	}
	provider = strings.TrimSpace(provider)
	nativeID = strings.TrimSpace(nativeID)
	if provider == "" || nativeID == "" {
		return ErrStarIdentityMissing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return err
	}
	key := threadStarKey(provider, nativeID)
	if starred {
		if _, err := s.db.Exec(
			`INSERT INTO thread_stars(provider, native_thread_id, starred_at) VALUES(?,?,?)
			 ON CONFLICT(provider, native_thread_id) DO UPDATE SET starred_at=excluded.starred_at`,
			provider, nativeID, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return err
		}
		s.cache[key] = true
		return nil
	}
	if _, err := s.db.Exec(
		`DELETE FROM thread_stars WHERE provider = ? AND native_thread_id = ?`,
		provider, nativeID,
	); err != nil {
		return err
	}
	delete(s.cache, key)
	return nil
}

// UnstarMany removes an operator-approved set atomically. Bulk close must not
// close half a tree and only then discover that one requested pin could not be
// removed.
func (s *Stars) UnstarMany(identities []hostedstore.AttachmentKey) error {
	if s == nil {
		return errors.New("thread stars store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keys := make([]string, 0, len(identities))
	for _, identity := range identities {
		provider := strings.TrimSpace(identity.Provider)
		nativeID := strings.TrimSpace(identity.NativeThreadID)
		if provider == "" || nativeID == "" {
			return ErrStarIdentityMissing
		}
		if _, err := tx.Exec(`DELETE FROM thread_stars WHERE provider = ? AND native_thread_id = ?`, provider, nativeID); err != nil {
			return err
		}
		keys = append(keys, threadStarKey(provider, nativeID))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, key := range keys {
		delete(s.cache, key)
	}
	return nil
}

// ErrStarIdentityMissing reports an incomplete identity supplied to a pin mutation.
var ErrStarIdentityMissing = errors.New("thread has no provider-native id to star")

// StarredIdentities lists the current provider-native pins.
func (s *Stars) StarredIdentities() []hostedstore.AttachmentKey {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return nil
	}
	out := make([]hostedstore.AttachmentKey, 0, len(s.cache))
	for key, starred := range s.cache {
		if !starred {
			continue
		}
		provider, nativeID, ok := strings.Cut(key, threadStarKeySep)
		if !ok || provider == "" || nativeID == "" {
			continue
		}
		out = append(out, hostedstore.AttachmentKey{Provider: provider, NativeThreadID: nativeID})
	}
	return out
}
