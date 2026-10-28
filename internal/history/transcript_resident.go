package history

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"

	parsecodex "github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A Codex turn that keeps running keeps a large reader continuation, and each
// append used to read it back, decode it, encode it again and compare every
// entry: seconds per append for a long turn. While a thread's turn runs, the
// store keeps the decoded continuation it last saved, and the next append for
// that thread resumes from it and writes only the entries that changed. The
// saved rows stay the source of truth: every append still saves its changes,
// the copy is replaced only after the transaction commits, and anything that
// does not match drops the copy so the next append reads the saved rows again.
const (
	residentThreadBytes = 96 << 20
	residentTotalBytes  = 256 << 20
	residentIdle        = 10 * time.Minute
)

// codexTrackedFields are the continuation fields whose changes the Codex
// reader reports (State.Changed); every other field is small and is compared
// whole.
var codexTrackedFields = map[string]bool{"items": true, "turns": true, "usage": true}

type residentContinuation struct {
	projectionKey string
	readOffset    int64
	checkpoint    parsedFileCheckpoint // without Reader
	reader        parsecodex.State     // tracking changes since this save
	// Saved entries, keyed by field and entry key: the hash of each untracked
	// field's entry, and the size of every entry.
	small    map[string][sha256.Size]byte
	sizes    map[string]int
	bytes    int
	recorded bool
	used     time.Time
}

type residentContinuations struct {
	mu      sync.Mutex
	threads map[string]*residentContinuation
	bytes   int
}

func entryID(field, key string) string { return field + "\x00" + key }

// lookup returns the copy saved at projectionKey and readOffset, if any. The
// copy is never changed after it is stored, so concurrent readers can share it.
func (r *residentContinuations) lookup(threadID, projectionKey string, readOffset int64) *residentContinuation {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.threads[threadID]
	if c == nil || c.projectionKey != projectionKey || c.readOffset != readOffset {
		return nil
	}
	c.used = time.Now()
	return c
}

// settle keeps next for threadID after a committed save, or drops the copy.
// It keeps only a running turn, within the per-thread and total bounds; past
// them the thread reads its saved rows as before.
func (r *residentContinuations) settle(threadID string, next *residentContinuation, committed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.threads[threadID]; old != nil {
		r.bytes -= old.bytes
		delete(r.threads, threadID)
	}
	now := time.Now()
	for id, c := range r.threads {
		if now.Sub(c.used) > residentIdle {
			r.bytes -= c.bytes
			delete(r.threads, id)
		}
	}
	if !committed || next == nil || !next.recorded || !next.reader.HasOpenTurn() || next.bytes > residentThreadBytes {
		return
	}
	for r.bytes+next.bytes > residentTotalBytes {
		var oldest string
		for id, c := range r.threads {
			if oldest == "" || c.used.Before(r.threads[oldest].used) {
				oldest = id
			}
		}
		if oldest == "" {
			return
		}
		r.bytes -= r.threads[oldest].bytes
		delete(r.threads, oldest)
	}
	if r.threads == nil {
		r.threads = map[string]*residentContinuation{}
	}
	next.used = now
	r.threads[threadID] = next
	r.bytes += next.bytes
}

// record notes the entries a whole save wrote, so the next append can compare
// against them.
func (c *residentContinuation) record(entries []continuationEntry) {
	c.small, c.sizes, c.bytes = map[string][sha256.Size]byte{}, map[string]int{}, 0
	for _, entry := range entries {
		id := entryID(entry.field, entry.key)
		if !codexTrackedFields[entry.field] {
			c.small[id] = entry.hash
		}
		c.sizes[id] = len(entry.value)
		c.bytes += len(entry.value)
	}
	c.recorded = true
}

// continuationDelta is a resumed save: the base row and the entries that
// changed since the copy it resumed from.
type continuationDelta struct {
	from    int64 // the read offset of the saved copy it resumed from
	base    []byte
	upserts []continuationEntry
	deletes [][2]string
}

// codexContinuationDelta splits a resumed Codex checkpoint the way
// splitTranscriptContinuation would, but encodes only the tracked entries the
// reader reports changed. It reports false when the reader cannot say.
func codexContinuationDelta(checkpoint parsedFileCheckpoint, reader parsecodex.State, prior *residentContinuation) (*continuationDelta, *residentContinuation, bool, error) {
	changed, ok := reader.Changed()
	if !ok || !prior.recorded {
		return nil, nil, false, nil
	}
	untracked := reader
	untracked.Items, untracked.Turns, untracked.Usage = map[string]model.Item{}, map[string]model.Turn{}, map[string]model.UsageObservation{}
	raw, err := json.Marshal(untracked)
	if err != nil {
		return nil, nil, false, err
	}
	checkpoint.Reader = raw
	full, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, nil, false, err
	}
	base, entries, err := splitTranscriptContinuation(full)
	if err != nil {
		return nil, nil, false, err
	}
	delta := &continuationDelta{from: prior.readOffset, base: base}
	next := &residentContinuation{small: map[string][sha256.Size]byte{}, sizes: make(map[string]int, len(prior.sizes)), bytes: prior.bytes, recorded: true}
	for id, size := range prior.sizes {
		next.sizes[id] = size
	}
	put := func(entry continuationEntry) {
		id := entryID(entry.field, entry.key)
		next.bytes += len(entry.value) - next.sizes[id]
		next.sizes[id] = len(entry.value)
		delta.upserts = append(delta.upserts, entry)
	}
	remove := func(field, key string) {
		id := entryID(field, key)
		next.bytes -= next.sizes[id]
		delete(next.sizes, id)
		delta.deletes = append(delta.deletes, [2]string{field, key})
	}
	for _, entry := range entries {
		id := entryID(entry.field, entry.key)
		next.small[id] = entry.hash
		if prior.small[id] != entry.hash {
			put(entry)
		}
	}
	for id := range prior.small {
		if _, kept := next.small[id]; !kept {
			field, key, _ := strings.Cut(id, "\x00")
			remove(field, key)
		}
	}
	for field, keys := range changed {
		for _, key := range keys {
			value, present, err := codexTrackedEntry(reader, field, key)
			if err != nil {
				return nil, nil, false, err
			}
			if !present {
				if _, saved := next.sizes[entryID(field, key)]; saved {
					remove(field, key)
				}
				continue
			}
			put(continuationEntry{field: field, key: key, value: value, hash: sha256.Sum256(value)})
		}
	}
	return delta, next, true, nil
}

func codexTrackedEntry(reader parsecodex.State, field, key string) (json.RawMessage, bool, error) {
	var value any
	var present bool
	switch field {
	case "items":
		value, present = reader.Items[key]
	case "turns":
		value, present = reader.Turns[key]
	case "usage":
		value, present = reader.Usage[key]
	}
	if !present {
		return nil, false, nil
	}
	raw, err := json.Marshal(value)
	return raw, true, err
}

// applyContinuationDelta writes a resumed save's changed entries, as
// syncContinuationEntries would have for the whole continuation.
func applyContinuationDelta(ctx context.Context, tx *sql.Tx, projectionID int64, delta *continuationDelta) error {
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(entry_sequence)+1,0) FROM conversation_parser_state_entries WHERE projection_id=?`, projectionID).Scan(&next); err != nil {
		return err
	}
	for _, entry := range delta.upserts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_parser_state_entries(projection_id,entry_sequence,field,entry_key,entry_json,entry_sha256) VALUES(?,?,?,?,?,?) ON CONFLICT(projection_id,field,entry_key) DO UPDATE SET entry_json=excluded.entry_json,entry_sha256=excluded.entry_sha256`, projectionID, next, entry.field, entry.key, []byte(entry.value), entry.hash[:]); err != nil {
			return err
		}
		next++
	}
	for _, removed := range delta.deletes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_parser_state_entries WHERE projection_id=? AND field=? AND entry_key=?`, projectionID, removed[0], removed[1]); err != nil {
			return err
		}
	}
	return nil
}
