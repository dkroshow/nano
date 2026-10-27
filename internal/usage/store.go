package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	statedb "github.com/dkroshow/nano/internal/state/db"
	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS usage_limit_observations (
  id INTEGER PRIMARY KEY,
  observed_at TEXT NOT NULL,
  provider TEXT NOT NULL,
  bucket_key TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  bucket_group TEXT NOT NULL DEFAULT '',
  used_percent REAL NOT NULL,
  window_minutes INTEGER,
  resets_at TEXT,
  severity TEXT,
  plan_type TEXT
);
CREATE INDEX IF NOT EXISTS usage_limit_observations_series
  ON usage_limit_observations(provider, bucket_key, observed_at DESC);
CREATE INDEX IF NOT EXISTS usage_limit_observations_recent
  ON usage_limit_observations(observed_at DESC);
`

const attemptLogSize = 450

type Store struct {
	db *sql.DB

	attemptMu sync.Mutex
	attempts  []Attempt

	mu          sync.Mutex
	writeMu     sync.Mutex
	byProvider  map[string]*ProviderSnapshot
	lastWritten map[string]string
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("usage store path is required")
	}
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("open usage-limits database: %w", err)
	}
	db.SetMaxOpenConns(2)
	if err := applySchema(db); err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{db: db, byProvider: make(map[string]*ProviderSnapshot), lastWritten: make(map[string]string)}
	if err := store.loadLastWritten(); err != nil {
		log.Printf("[usage-limits] seed change detection: %v", err)
	}
	if err := store.loadSnapshot(); err != nil {
		log.Printf("[usage-limits] seed snapshot: %v", err)
	}
	return store, nil
}

func applySchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read usage schema version: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("usage schema version %d is newer than supported version %d", version, SchemaVersion)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin usage schema migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("apply usage schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, SchemaVersion)); err != nil {
		return fmt.Errorf("record usage schema version: %w", err)
	}
	return tx.Commit()
}

func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

// Database exposes the owned handle only to the transitional root adapter and
// its pre-existing lock-contention tests. New callers use Store operations.
func (store *Store) Database() *sql.DB { return store.db }

func (store *Store) Observe(snapshot *ProviderSnapshot) {
	if store == nil || snapshot == nil || snapshot.Provider == "" {
		return
	}
	store.mu.Lock()
	previous := store.byProvider[snapshot.Provider]
	wasFailing := previous != nil && previous.Error != ""
	if (snapshot.Error != "" || snapshot.Notice != "") && len(snapshot.Buckets) == 0 {
		if previous != nil {
			carried := cloneSnapshot(previous)
			carried.Error, carried.ErrorDetail, carried.Notice = snapshot.Error, snapshot.ErrorDetail, snapshot.Notice
			store.byProvider[snapshot.Provider] = &carried
		} else {
			copy := cloneSnapshot(snapshot)
			store.byProvider[snapshot.Provider] = &copy
		}
		store.mu.Unlock()
		if snapshot.Error != "" && !wasFailing {
			log.Printf("[usage-limits] %s reads are failing: %s (%s)", snapshot.Provider, snapshot.Error, snapshot.ErrorDetail)
		}
		return
	}
	copy := cloneSnapshot(snapshot)
	store.byProvider[snapshot.Provider] = &copy
	store.mu.Unlock()
	if wasFailing {
		log.Printf("[usage-limits] %s reads recovered", snapshot.Provider)
	}

	store.writeMu.Lock()
	defer store.writeMu.Unlock()
	store.mu.Lock()
	changed := make([]Bucket, 0, len(snapshot.Buckets))
	for _, bucket := range snapshot.Buckets {
		if store.lastWritten[snapshot.Provider+"|"+bucket.Key] != bucketSignature(bucket) {
			changed = append(changed, bucket)
		}
	}
	store.mu.Unlock()
	if len(changed) == 0 {
		return
	}
	if err := store.appendRows(snapshot, changed); err != nil {
		log.Printf("[usage-limits] append %s: %v", snapshot.Provider, err)
		return
	}
	store.mu.Lock()
	for _, bucket := range changed {
		store.lastWritten[snapshot.Provider+"|"+bucket.Key] = bucketSignature(bucket)
	}
	store.mu.Unlock()
}

func cloneSnapshot(snapshot *ProviderSnapshot) ProviderSnapshot {
	copy := *snapshot
	copy.Buckets = append([]Bucket(nil), snapshot.Buckets...)
	copy.ResetCreditDetails = append([]ResetCredit(nil), snapshot.ResetCreditDetails...)
	return copy
}

func bucketSignature(bucket Bucket) string {
	return fmt.Sprintf("%.2f|%s", bucket.UsedPercent, bucket.Severity)
}

func (store *Store) appendRows(snapshot *ProviderSnapshot, buckets []Bucket) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO usage_limit_observations
		(observed_at,provider,bucket_key,label,bucket_group,used_percent,window_minutes,resets_at,severity,plan_type)
		VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	observedAt := snapshot.ObservedAt.UTC().Format(time.RFC3339)
	for _, bucket := range buckets {
		var window, resets any
		if bucket.WindowMinutes > 0 {
			window = bucket.WindowMinutes
		}
		if bucket.ResetsAt > 0 {
			resets = time.Unix(bucket.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		if _, err := stmt.ExecContext(ctx, observedAt, snapshot.Provider, bucket.Key, bucket.Label, bucket.Group,
			bucket.UsedPercent, window, resets, nilIfEmpty(bucket.Severity), nilIfEmpty(snapshot.PlanType)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (store *Store) loadLastWritten() error {
	rows, err := store.db.Query(`SELECT provider,bucket_key,used_percent,COALESCE(severity,'')
		FROM usage_limit_observations WHERE id IN
		(SELECT MAX(id) FROM usage_limit_observations GROUP BY provider,bucket_key)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var provider, key, severity string
		var percent float64
		if err := rows.Scan(&provider, &key, &percent, &severity); err != nil {
			return err
		}
		store.lastWritten[provider+"|"+key] = bucketSignature(Bucket{UsedPercent: percent, Severity: severity})
	}
	return rows.Err()
}

func (store *Store) loadSnapshot() error {
	rows, err := store.db.Query(`SELECT observed_at,provider,bucket_key,label,bucket_group,
		used_percent,window_minutes,resets_at,severity,plan_type FROM usage_limit_observations
		WHERE id IN (SELECT MAX(id) FROM usage_limit_observations GROUP BY provider,bucket_key)
		ORDER BY provider,bucket_key`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var observedAt, provider, key, label, group string
		var percent float64
		var window sql.NullInt64
		var resets, severity, plan sql.NullString
		if err := rows.Scan(&observedAt, &provider, &key, &label, &group, &percent, &window, &resets, &severity, &plan); err != nil {
			return err
		}
		snapshot := store.byProvider[provider]
		if snapshot == nil {
			snapshot = &ProviderSnapshot{Provider: provider, Buckets: []Bucket{}}
			store.byProvider[provider] = snapshot
		}
		if plan.Valid && snapshot.PlanType == "" {
			snapshot.PlanType = plan.String
		}
		if at, err := time.Parse(time.RFC3339, observedAt); err == nil && at.After(snapshot.ObservedAt) {
			snapshot.ObservedAt = at.UTC()
		}
		bucket := Bucket{Key: key, Label: label, Group: group, UsedPercent: percent}
		if window.Valid {
			bucket.WindowMinutes = int(window.Int64)
		}
		if resets.Valid {
			if at, err := time.Parse(time.RFC3339, resets.String); err == nil {
				bucket.ResetsAt = at.Unix()
			}
		}
		if severity.Valid {
			bucket.Severity = severity.String
		}
		snapshot.Buckets = append(snapshot.Buckets, bucket)
	}
	return rows.Err()
}

func (store *Store) Snapshot() []ProviderSnapshot {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]ProviderSnapshot, 0, len(store.byProvider))
	for _, snapshot := range store.byProvider {
		copy := cloneSnapshot(snapshot)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

func (store *Store) RecordAttempt(attempt Attempt) {
	if store == nil {
		return
	}
	store.attemptMu.Lock()
	defer store.attemptMu.Unlock()
	store.attempts = append(store.attempts, attempt)
	if len(store.attempts) > attemptLogSize {
		store.attempts = store.attempts[len(store.attempts)-attemptLogSize:]
	}
}

func (store *Store) Attempts(limit int) []Attempt {
	if store == nil {
		return []Attempt{}
	}
	store.attemptMu.Lock()
	defer store.attemptMu.Unlock()
	if limit <= 0 || limit > len(store.attempts) {
		limit = len(store.attempts)
	}
	out := make([]Attempt, 0, limit)
	for index := len(store.attempts) - 1; index >= 0 && len(out) < limit; index-- {
		out = append(out, store.attempts[index])
	}
	return out
}

func (store *Store) History(ctx context.Context, window time.Duration) ([]Series, error) {
	if store == nil {
		return []Series{}, nil
	}
	since := time.Now().UTC().Add(-window).Format(time.RFC3339)
	byKey := make(map[string]*Series)
	order := []string{}
	add := func(provider, key, label, group string, at time.Time, percent float64) {
		id := provider + "|" + key
		series := byKey[id]
		if series == nil {
			series = &Series{Provider: provider, Key: key, Label: label, Group: group}
			byKey[id] = series
			order = append(order, id)
		}
		if label != "" {
			series.Label = label
		}
		series.Points = append(series.Points, SeriesPoint{At: at, Percent: percent})
	}
	scan := func(rows *sql.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var observedAt, provider, key, label, group string
			var percent float64
			if err := rows.Scan(&observedAt, &provider, &key, &label, &group, &percent); err != nil {
				return err
			}
			if at, err := time.Parse(time.RFC3339, observedAt); err == nil {
				add(provider, key, label, group, at.UTC(), percent)
			}
		}
		return rows.Err()
	}
	seed, err := store.db.QueryContext(ctx, `SELECT observed_at,provider,bucket_key,label,bucket_group,used_percent
		FROM usage_limit_observations WHERE id IN (SELECT MAX(id) FROM usage_limit_observations
		WHERE observed_at < ? GROUP BY provider,bucket_key) ORDER BY observed_at`, since)
	if err != nil {
		return nil, err
	}
	if err := scan(seed); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT observed_at,provider,bucket_key,label,bucket_group,used_percent
		FROM usage_limit_observations WHERE observed_at >= ? ORDER BY observed_at`, since)
	if err != nil {
		return nil, err
	}
	if err := scan(rows); err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(order))
	for _, id := range order {
		out = append(out, *byKey[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}
