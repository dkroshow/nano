package app

// thread_stars.go — operator-owned starred threads.
//
// Stars are Gmail-style pins for the sidebar: a small durable set of
// (provider, native_thread_id) keys the compact rail splits above the rest.
// They are operator truth, not derived catalog data, so they live in a sibling
// SQLite file (~/.nanod/thread-stars.sqlite, flag -thread-stars-db) rather
// than history.sqlite. A delete-and-rebuild of the catalog must not forget
// which threads the operator pinned. The names JSONL is the other operator-owned
// overlay; stars stay out of it because this feature asked for SQLite.
//
// Listings overlay the flag at read time (listCatalogPage, thread detail,
// session summaries, and GET /api/v1/sidebar/threads). Starred threads are
// rescued into the active sidebar even when they sit outside the eligibility
// window.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

const threadStarKeySep = "\x00"

type threadStarsStore struct {
	db     *sql.DB
	mu     sync.Mutex
	cache  map[string]bool
	loaded bool
}

func openThreadStarsStore(path string) (*threadStarsStore, error) {
	path = strings.TrimSpace(path)
	absolute, err := canonicalProspectivePath(path)
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
	return &threadStarsStore{db: db, cache: map[string]bool{}}, nil
}

func (s *threadStarsStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func threadStarKey(provider, nativeID string) string {
	return strings.TrimSpace(provider) + threadStarKeySep + strings.TrimSpace(nativeID)
}

func (s *threadStarsStore) ensureLoaded() error {
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

func (s *threadStarsStore) isStarred(provider, nativeID string) bool {
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

func (s *threadStarsStore) setStarred(provider, nativeID string, starred bool) error {
	if s == nil {
		return errors.New("thread stars store unavailable")
	}
	provider = strings.TrimSpace(provider)
	nativeID = strings.TrimSpace(nativeID)
	if provider == "" || nativeID == "" {
		return errThreadStarIdentityMissing
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

// unstarMany removes an operator-approved set atomically. Bulk close must not
// close half a tree and only then discover that one requested pin could not be
// removed.
func (s *threadStarsStore) unstarMany(identities []attachmentKey) error {
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
			return errThreadStarIdentityMissing
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

var errThreadStarIdentityMissing = errors.New("thread has no provider-native id to star")

func (s *server) starThread(thread *catalogThread) {
	if s == nil || s.threadStars == nil || thread == nil {
		return
	}
	thread.Starred = s.threadStars.isStarred(thread.Provider, thread.NativeID)
}

func (s *server) attachStarredOverlay(threads []catalogThread) {
	for i := range threads {
		s.starThread(&threads[i])
	}
}

func (s *threadStarsStore) starredIdentities() []attachmentKey {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return nil
	}
	out := make([]attachmentKey, 0, len(s.cache))
	for key, starred := range s.cache {
		if !starred {
			continue
		}
		provider, nativeID, ok := strings.Cut(key, threadStarKeySep)
		if !ok || provider == "" || nativeID == "" {
			continue
		}
		out = append(out, attachmentKey{Provider: provider, NativeThreadID: nativeID})
	}
	return out
}

func (s *server) attachStarredOnSessions(summaries []sessions.Summary) {
	if s == nil || s.threadStars == nil {
		return
	}
	for i := range summaries {
		summaries[i].Starred = s.threadStars.isStarred(summaries[i].Provider, summaries[i].ThreadID)
	}
}

func (s *server) handleThreadStarred(w http.ResponseWriter, r *http.Request, rawID string) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.threadStars == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "stars_unavailable", "Thread stars store unavailable")
		return
	}
	if s.catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	if rawID == "" || strings.Contains(rawID, "/") {
		httpapi.WriteError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
		return
	}
	reference, err := url.PathUnescape(rawID)
	if err != nil || strings.TrimSpace(reference) == "" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_thread_id", "Invalid thread id")
		return
	}
	id, err := s.resolveThreadReference(r.Context(), reference)
	if err != nil {
		writeThreadReferenceError(w, err)
		return
	}
	thread, err := s.catalog.LoadThread(r.Context(), id)
	if err != nil {
		writeCatalogBackendError(w, err)
		return
	}
	var payload struct {
		Starred *bool `json:"starred"`
	}
	body := http.MaxBytesReader(w, r.Body, maxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil || payload.Starred == nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_starred", `Request body must be {"starred": true} or {"starred": false}`)
		return
	}
	if err := s.threadStars.setStarred(thread.Provider, thread.NativeID, *payload.Starred); err != nil {
		if errors.Is(err, errThreadStarIdentityMissing) {
			httpapi.WriteError(w, http.StatusConflict, "star_identity_missing", "Thread has no provider-native id to star yet")
			return
		}
		httpapi.WriteError(w, http.StatusInternalServerError, "star_error", "Thread star update failed")
		return
	}
	s.attachRuntimeOverlay(&thread)
	s.starThread(&thread)
	sidebarCurrent := s.sidebar != nil && s.sidebar.applyStar(thread.Provider, thread.NativeID, *payload.Starred)
	// Open browsers on every device share this SQLite file through this
	// server. Nudge them to refetch so a pin on the laptop is not stuck in
	// the iPad's in-memory snapshot until a hard refresh.
	s.publishRuntimeDelta(true, sidebarCurrent)
	writeCatalogJSON(w, http.StatusOK, thread)
}
