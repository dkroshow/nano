package store

import (
	"strings"

	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// RecordReference names a hosted identity or a provider-scoped native identity.
type RecordReference struct{ HostedThreadID, Provider, NativeThreadID string }

// RecordsForReferences resolves the durable hosted identity overlay in one
// control-store read. The ordinary point helper is ideal for one detail row,
// but calling it for every graph node turns a cold sidebar read into more than
// a thousand serialized SQLite queries.
func (store *ExecutionStore) RecordsForReferences(refs []RecordReference) ([]threadvalues.SessionRecord, []bool) {
	records := make([]threadvalues.SessionRecord, len(refs))
	found := make([]bool, len(refs))
	if store == nil || len(refs) == 0 {
		return records, found
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	requested := map[AttachmentKey]bool{}
	var keys []AttachmentKey // requested, in request order
	for i, thread := range refs {
		if thread.HostedThreadID != "" {
			if record, ok := store.sessions[thread.HostedThreadID]; ok {
				records[i], found[i] = record, true
			}
			continue
		}
		if key := (AttachmentKey{Provider: thread.Provider, NativeThreadID: thread.NativeThreadID}); key.Provider != "" && key.NativeThreadID != "" && !requested[key] {
			requested[key] = true
			keys = append(keys, key)
		}
	}

	// Only the requested claims, in bounded batches: reading every claim
	// cost each one-row catalog repair about 10 ms on a fleet of 1,100.
	claims := map[AttachmentKey]string{}
	for start := 0; store.control != nil && start < len(keys); start += 400 {
		batch := keys[start:min(start+400, len(keys))]
		pairs := make([]string, len(batch))
		args := make([]any, 0, 2*len(batch))
		for i, key := range batch {
			pairs[i] = "(?, ?)"
			args = append(args, key.Provider, key.NativeThreadID)
		}
		rows, err := store.control.db.Query(`SELECT provider,native_thread_id,hosted_thread_id
			FROM hosted_thread_native_ids WHERE retired_at IS NULL
			AND (provider, native_thread_id) IN (VALUES `+strings.Join(pairs, ", ")+`)`, args...)
		if err != nil {
			return records, found
		}
		for rows.Next() {
			var provider, nativeID, hostedID string
			if err := rows.Scan(&provider, &nativeID, &hostedID); err != nil {
				rows.Close()
				return records, found
			}
			claims[AttachmentKey{Provider: provider, NativeThreadID: nativeID}] = hostedID
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return records, found
		}
		rows.Close()
	}

	fallback := map[AttachmentKey]threadvalues.SessionRecord{}
	for _, record := range store.sessions {
		key := AttachmentKey{Provider: record.Provider, NativeThreadID: record.NativeThreadID}
		if !requested[key] {
			continue
		}
		if _, claimed := claims[key]; claimed {
			continue
		}
		best := fallback[key]
		bestOpen := best.ID != "" && best.Status != StatusExited
		recordOpen := record.Status != StatusExited
		// Compared as times: RFC3339Nano trims trailing zeros, so as text
		// ".2374Z" sorts after the later ".237401Z".
		if best.ID == "" || (recordOpen != bestOpen && recordOpen) || (recordOpen == bestOpen && threadvalues.TimestampBefore(best.UpdatedAt, record.UpdatedAt)) {
			fallback[key] = record
		}
	}
	for i, thread := range refs {
		if found[i] || thread.Provider == "" || thread.NativeThreadID == "" {
			continue
		}
		key := AttachmentKey{Provider: thread.Provider, NativeThreadID: thread.NativeThreadID}
		if hostedID, claimed := claims[key]; claimed {
			records[i], found[i] = store.sessions[hostedID]
			if records[i].ID == "" {
				found[i] = false
			}
			continue
		}
		if record, ok := fallback[key]; ok {
			records[i], found[i] = record, true
		}
	}
	return records, found
}
