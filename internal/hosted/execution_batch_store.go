package hosted

import (
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// RecordReference names a hosted identity or a provider-scoped native identity.
type RecordReference struct{ HostedThreadID, Provider, NativeThreadID string }

// RecordsForReferences resolves the durable hosted identity overlay in one
// control-store read. The ordinary point helper is ideal for one detail row,
// but calling it for every graph node turns a cold sidebar read into more than
// a thousand serialized SQLite queries.
func (store *ExecutionStore) RecordsForReferences(refs []RecordReference) ([]agentSessionRecord, []bool) {
	records := make([]agentSessionRecord, len(refs))
	found := make([]bool, len(refs))
	if store == nil || len(refs) == 0 {
		return records, found
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	requested := map[attachmentKey]bool{}
	for i, thread := range refs {
		if thread.HostedThreadID != "" {
			if record, ok := store.sessions[thread.HostedThreadID]; ok {
				records[i], found[i] = record, true
			}
			continue
		}
		if thread.Provider != "" && thread.NativeThreadID != "" {
			requested[attachmentKey{Provider: thread.Provider, NativeThreadID: thread.NativeThreadID}] = true
		}
	}

	claims := map[attachmentKey]string{}
	if store.control != nil && len(requested) > 0 {
		rows, err := store.control.db.Query(`SELECT provider,native_thread_id,hosted_thread_id
			FROM hosted_thread_native_ids WHERE retired_at IS NULL`)
		if err != nil {
			return records, found
		}
		for rows.Next() {
			var provider, nativeID, hostedID string
			if err := rows.Scan(&provider, &nativeID, &hostedID); err != nil {
				rows.Close()
				return records, found
			}
			key := attachmentKey{Provider: provider, NativeThreadID: nativeID}
			if requested[key] {
				claims[key] = hostedID
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return records, found
		}
		rows.Close()
	}

	fallback := map[attachmentKey]agentSessionRecord{}
	for _, record := range store.sessions {
		key := attachmentKey{Provider: record.Provider, NativeThreadID: record.NativeThreadID}
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
		key := attachmentKey{Provider: thread.Provider, NativeThreadID: thread.NativeThreadID}
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
