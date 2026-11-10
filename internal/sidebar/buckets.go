package sidebar

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Buckets are operator-named sidebar groups in an order only the operator
// sets. A thread joins one by its provider-native identity, the same key a
// star uses, so membership follows the thread and never a pane or session.
// Its subagents travel with it in the browser; they are never members.
// The tables share the stars database: both are the operator's curation of
// the sidebar, and neither is derived from anything else.

const maxBucketNameRunes = 80

// BucketThread names one member thread.
type BucketThread struct {
	Provider string `json:"provider"`
	NativeID string `json:"nativeId"`
}

// Bucket is one operator-named group and its member threads.
type Bucket struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Threads []BucketThread `json:"threads"`
}

// BucketState is the whole bucket set in the operator's order. It is small,
// so every read and every change event carries all of it.
type BucketState struct {
	Buckets []Bucket `json:"buckets"`
}

var (
	errBucketNotFound   = errors.New("bucket not found")
	errBucketName       = fmt.Errorf("bucket name must be 1 to %d characters", maxBucketNameRunes)
	errBucketOrderStale = errors.New("bucket order must list every bucket exactly once")
)

// bucketNudges wakes event streams after a bucket change. A stream rereads
// the whole state, so one pending wake covers any number of changes.
type bucketNudges struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func (n *bucketNudges) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.subs == nil {
		n.subs = map[chan struct{}]struct{}{}
	}
	n.subs[ch] = struct{}{}
	return ch
}

func (n *bucketNudges) unsubscribe(ch chan struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.subs, ch)
}

func (n *bucketNudges) notify() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func migrateBuckets(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sidebar_buckets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			position INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sidebar_bucket_threads (
			provider TEXT NOT NULL,
			native_thread_id TEXT NOT NULL,
			bucket_id TEXT NOT NULL,
			assigned_at TEXT NOT NULL,
			PRIMARY KEY (provider, native_thread_id)
		)`)
	return err
}

func cleanBucketName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxBucketNameRunes {
		return "", errBucketName
	}
	return name, nil
}

// buckets reads the whole state in the operator's order.
func (s *Stars) buckets() (BucketState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readBuckets(s.db)
}

func readBuckets(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) (BucketState, error) {
	state := BucketState{Buckets: []Bucket{}}
	rows, err := q.Query(`SELECT id, name FROM sidebar_buckets ORDER BY position, created_at`)
	if err != nil {
		return state, err
	}
	index := map[string]int{}
	for rows.Next() {
		var bucket Bucket
		if err := rows.Scan(&bucket.ID, &bucket.Name); err != nil {
			rows.Close()
			return state, err
		}
		bucket.Threads = []BucketThread{}
		index[bucket.ID] = len(state.Buckets)
		state.Buckets = append(state.Buckets, bucket)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return state, err
	}
	members, err := q.Query(`SELECT bucket_id, provider, native_thread_id FROM sidebar_bucket_threads ORDER BY assigned_at, provider, native_thread_id`)
	if err != nil {
		return state, err
	}
	defer members.Close()
	for members.Next() {
		var bucketID string
		var thread BucketThread
		if err := members.Scan(&bucketID, &thread.Provider, &thread.NativeID); err != nil {
			return state, err
		}
		if i, ok := index[bucketID]; ok {
			state.Buckets[i].Threads = append(state.Buckets[i].Threads, thread)
		}
	}
	return state, members.Err()
}

// change runs one bucket mutation in a transaction, then wakes the streams.
func (s *Stars) change(apply func(*sql.Tx) error) (BucketState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return BucketState{}, err
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return BucketState{}, err
	}
	state, err := readBuckets(tx)
	if err != nil {
		return BucketState{}, err
	}
	if err := tx.Commit(); err != nil {
		return BucketState{}, err
	}
	s.bucketEvents.notify()
	return state, nil
}

func bucketExists(tx *sql.Tx, id string) error {
	var found int
	err := tx.QueryRow(`SELECT 1 FROM sidebar_buckets WHERE id = ?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return errBucketNotFound
	}
	return err
}

func bucketTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// createBucket appends a bucket after the existing ones.
func (s *Stars) createBucket(name string) (BucketState, error) {
	name, err := cleanBucketName(name)
	if err != nil {
		return BucketState{}, err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return BucketState{}, err
	}
	id := "bkt_" + hex.EncodeToString(raw)
	return s.change(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO sidebar_buckets(id, name, position, created_at)
			 VALUES(?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM sidebar_buckets), ?)`,
			id, name, bucketTime(),
		)
		return err
	})
}

func (s *Stars) renameBucket(id, name string) (BucketState, error) {
	name, err := cleanBucketName(name)
	if err != nil {
		return BucketState{}, err
	}
	return s.change(func(tx *sql.Tx) error {
		if err := bucketExists(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE sidebar_buckets SET name = ? WHERE id = ?`, name, id)
		return err
	})
}

// deleteBucket removes a bucket; its threads return to the ungrouped list.
func (s *Stars) deleteBucket(id string) (BucketState, error) {
	return s.change(func(tx *sql.Tx) error {
		if err := bucketExists(tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM sidebar_bucket_threads WHERE bucket_id = ?`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM sidebar_buckets WHERE id = ?`, id)
		return err
	})
}

// orderBuckets sets the whole order. It must name every bucket once, so a
// browser holding a stale list cannot drop a bucket another device created.
func (s *Stars) orderBuckets(ids []string) (BucketState, error) {
	return s.change(func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sidebar_buckets`).Scan(&count); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				return errBucketOrderStale
			}
			seen[id] = true
		}
		if len(ids) != count {
			return errBucketOrderStale
		}
		for position, id := range ids {
			result, err := tx.Exec(`UPDATE sidebar_buckets SET position = ? WHERE id = ?`, position, id)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				return errBucketOrderStale
			}
		}
		return nil
	})
}

// assignBucket puts a thread in a bucket, or takes it out when bucketID is
// empty. A thread is in at most one bucket.
func (s *Stars) assignBucket(provider, nativeID, bucketID string) (BucketState, error) {
	if provider == "" || nativeID == "" {
		return BucketState{}, errStarIdentityMissing
	}
	return s.change(func(tx *sql.Tx) error {
		if bucketID == "" {
			_, err := tx.Exec(`DELETE FROM sidebar_bucket_threads WHERE provider = ? AND native_thread_id = ?`, provider, nativeID)
			return err
		}
		if err := bucketExists(tx, bucketID); err != nil {
			return err
		}
		_, err := tx.Exec(
			`INSERT INTO sidebar_bucket_threads(provider, native_thread_id, bucket_id, assigned_at) VALUES(?,?,?,?)
			 ON CONFLICT(provider, native_thread_id) DO UPDATE SET bucket_id = excluded.bucket_id, assigned_at = excluded.assigned_at`,
			provider, nativeID, bucketID, bucketTime(),
		)
		return err
	})
}
