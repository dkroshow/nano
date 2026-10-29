package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dkroshow/nano/internal/threads"
)

// TurnAttribution is the origin/sender stamp for a new operation.
type TurnAttribution struct {
	Origin, SenderHostedThreadID, SenderNativeThreadID, SenderProvider, SenderName, SenderSubmittedAt string
}

// AgentTurnCreationGuard runs while the execution-store mutex is held and,
// for the control-backed store, inside the same SQLite transaction that will
// insert the operation. Every operation creator enters through this boundary,
// so a guard can compare the current durable operation set with a candidate
// without leaving a time-of-check/time-of-use gap before creation. A guard
// must inspect only the supplied snapshot; calling store methods would try to
// reacquire the mutex.
type AgentTurnCreationGuard func(existing []threads.TurnRecord) error

// BeginTurnAttributed admits one idempotent operation with its sender evidence.
func (s *ExecutionStore) BeginTurnAttributed(sessionID, actionID, promptHash string, attribution TurnAttribution) (threads.TurnRecord, bool, error) {
	return s.BeginTurnAttributedGuarded(sessionID, actionID, promptHash, attribution, nil)
}

// BeginTurnAttributedGuarded admits an operation while its supplied guard holds the mutation boundary.
func (s *ExecutionStore) BeginTurnAttributedGuarded(sessionID, actionID, promptHash string, attribution TurnAttribution, guard AgentTurnCreationGuard) (threads.TurnRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionRec, ok := s.sessions[sessionID]
	if !ok {
		return threads.TurnRecord{}, false, fmt.Errorf("unknown hosted thread")
	}
	key := agentActionKey(sessionID, actionID)
	if id := s.byAction[key]; id != "" {
		rec := s.turns[id]
		if rec.PromptHash != promptHash {
			return threads.TurnRecord{}, false, ErrAgentTurnIdempotencyConflict
		}
		return rec, false, nil
	}
	if s.control != nil {
		var existingID, existingHash string
		err := s.control.db.QueryRow(`SELECT operation_id,prompt_hash FROM hosted_turn_operations WHERE hosted_thread_id=? AND client_action_id=?`, sessionID, actionID).Scan(&existingID, &existingHash)
		if err == nil {
			if existingHash != promptHash {
				return threads.TurnRecord{}, false, ErrAgentTurnIdempotencyConflict
			}
			s.resetControlCachesLocked()
			if err := loadControlExecutionRows(s); err != nil {
				return threads.TurnRecord{}, false, err
			}
			return s.turns[existingID], false, nil
		}
		if err != sql.ErrNoRows {
			return threads.TurnRecord{}, false, err
		}
	}
	var tx *sql.Tx
	if s.control != nil {
		var err error
		tx, err = s.control.db.Begin()
		if err != nil {
			return threads.TurnRecord{}, false, err
		}
		defer tx.Rollback()
	}
	if guard != nil {
		existing := make([]threads.TurnRecord, 0)
		for _, operation := range s.turns {
			if operation.HostedThreadID == sessionID {
				existing = append(existing, operation)
			}
		}
		sort.Slice(existing, func(i, j int) bool {
			if a, b := threads.ParseTimestamp(existing[i].CreatedAt), threads.ParseTimestamp(existing[j].CreatedAt); !a.Equal(b) {
				return a.Before(b)
			}
			return existing[i].ID < existing[j].ID
		})
		if err := guard(existing); err != nil {
			return threads.TurnRecord{}, false, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := threads.TurnRecord{
		ID: agentOperationID(sessionID, actionID), HostedThreadID: sessionID, ClientActionID: actionID,
		TransportActionID: agentTransportActionID(sessionID, actionID), PromptHash: promptHash,
		Provider: sessionRec.Provider, NanoSessionID: sessionRec.NanoSessionID,
		NativeThreadID: sessionRec.NativeThreadID, Status: agentTurnDeliveryPending,
		Origin: attribution.Origin, SenderHostedThreadID: attribution.SenderHostedThreadID,
		SenderNativeThreadID: attribution.SenderNativeThreadID, SenderProvider: attribution.SenderProvider,
		SenderName: attribution.SenderName, SenderSubmittedAt: attribution.SenderSubmittedAt,
		RunID: s.runID, CreatedAt: now, UpdatedAt: now, Revision: 1,
	}
	if tx != nil {
		var assignmentID sql.NullInt64
		var assignmentDisposition string
		err := tx.QueryRow(`SELECT desired_event_id,desired_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, sessionID, AssignmentField).Scan(&assignmentID, &assignmentDisposition)
		if err != nil && err != sql.ErrNoRows {
			return threads.TurnRecord{}, false, err
		}
		if assignmentID.Valid && assignmentDisposition != metadataDispositionCleared {
			id := assignmentID.Int64
			rec.AssignmentEventID = &id
		}
		if err := insertTurnOperation(tx, rec); err != nil {
			return threads.TurnRecord{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return threads.TurnRecord{}, false, err
		}
		s.setTurnLocked(rec.ID, &rec)
		s.turnBaseline[rec.ID] = rec.Revision
		s.noteTurnTransitionLocked("", rec)
		s.publishPendingTransitionsLocked()
		return rec, true, nil
	}
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked("", rec)
	if err := s.persistAndNotifyLocked(); err != nil {
		if errors.Is(err, errExecutionConflict) {
			if existingID := s.byAction[key]; existingID != "" {
				if existing := s.turns[existingID]; existing.PromptHash == promptHash {
					return existing, false, nil
				}
			}
		}
		return threads.TurnRecord{}, false, err
	}
	return rec, true, nil
}

// Every turn mutation invalidates only the affected threads. The turn map
// remains authoritative; rollback reloads rebuild these derived indexes.
func (s *ExecutionStore) setTurnLocked(id string, rec *threads.TurnRecord) {
	if before, ok := s.turns[id]; ok {
		if rec == nil || before.HostedThreadID != rec.HostedThreadID {
			delete(s.turnsByHosted[before.HostedThreadID], id)
			if len(s.turnsByHosted[before.HostedThreadID]) == 0 {
				delete(s.turnsByHosted, before.HostedThreadID)
			}
		}
		if rec == nil || before.HostedThreadID != rec.HostedThreadID || before.ClientActionID != rec.ClientActionID {
			delete(s.byAction, agentActionKey(before.HostedThreadID, before.ClientActionID))
		}
		s.dirtyTurnThreads[before.HostedThreadID] = true
		s.attentionDirty[before.HostedThreadID] = true
	}
	s.dirtyTurns[id] = true
	if rec == nil {
		delete(s.turns, id)
		if s.turnBaseline[id] > 0 {
			s.deletedTurns[id] = true
		}
		return
	}
	s.turns[id] = *rec
	if s.turnsByHosted[rec.HostedThreadID] == nil {
		s.turnsByHosted[rec.HostedThreadID] = map[string]bool{}
	}
	s.turnsByHosted[rec.HostedThreadID][id] = true
	s.byAction[agentActionKey(rec.HostedThreadID, rec.ClientActionID)] = id
	delete(s.deletedTurns, id)
	s.dirtyTurnThreads[rec.HostedThreadID] = true
	s.attentionDirty[rec.HostedThreadID] = true
}

func (s *ExecutionStore) resetTurnIndexesLocked() {
	s.turnsByHosted = map[string]map[string]bool{}
	s.dirtyTurnThreads, s.dirtyTurns = map[string]bool{}, map[string]bool{}
	s.attentionDirty, s.attentionCache = map[string]bool{}, map[string]bool{}
}

func (s *ExecutionStore) resetControlCachesLocked() {
	s.sessions, s.turns, s.byAction, s.byClient = map[string]threads.SessionRecord{}, map[string]threads.TurnRecord{}, map[string]string{}, map[string]string{}
	s.deletedSessions, s.deletedTurns, s.sessionBaseline, s.turnBaseline = map[string]bool{}, map[string]bool{}, map[string]int64{}, map[string]int64{}
	s.pendingTransitions = nil
	s.resetTurnIndexesLocked()
}

// Turn reads a cached operation by its durable operation ID.
func (s *ExecutionStore) Turn(id string) (threads.TurnRecord, bool) {
	if s == nil {
		return threads.TurnRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.turns[id]
	return rec, ok
}

// TurnsForHosted reads a hosted thread’s operation records.
func (s *ExecutionStore) TurnsForHosted(hostedID string) []threads.TurnRecord {
	if s == nil || hostedID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]threads.TurnRecord, 0)
	for _, rec := range s.turns {
		if rec.HostedThreadID == hostedID {
			result = append(result, rec)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if a, b := threads.ParseTimestamp(result[i].CreatedAt), threads.ParseTimestamp(result[j].CreatedAt); !a.Equal(b) {
			return a.Before(b)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

// OperationIDsForHosted lists the durable operations owned by one hosted thread.
func (s *ExecutionStore) OperationIDsForHosted(hostedID string) []string {
	if s == nil || hostedID == "" {
		return []string{}
	}
	if s.control != nil {
		rows, err := s.control.db.Query(`SELECT operation_id FROM hosted_turn_operations WHERE hosted_thread_id=? ORDER BY operation_id`, hostedID)
		if err == nil {
			defer rows.Close()
			ids := []string{}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			return ids
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0)
	for id, rec := range s.turns {
		if rec.HostedThreadID == hostedID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// LineageForHosted reads the immutable invocation edges held by Nano's
// execution store. It never accepts lineage identifiers from a tool request.
func (s *ExecutionStore) LineageForHosted(hostedID string) (ancestors, descendants []string) {
	if s == nil || hostedID == "" {
		return []string{}, []string{}
	}
	if s.control != nil {
		current := hostedID
		seen := map[string]bool{hostedID: true}
		for {
			var parent sql.NullString
			err := s.control.db.QueryRow(`SELECT invoker_hosted_id FROM hosted_threads WHERE hosted_thread_id=?`, current).Scan(&parent)
			if err != nil || !parent.Valid || parent.String == "" || seen[parent.String] {
				break
			}
			current, seen[parent.String] = parent.String, true
			ancestors = append(ancestors, current)
		}
		rows, err := s.control.db.Query(`WITH RECURSIVE family(id) AS (
			SELECT hosted_thread_id FROM hosted_threads WHERE invoker_hosted_id=?
			UNION
			SELECT h.hosted_thread_id FROM hosted_threads h JOIN family f ON h.invoker_hosted_id=f.id
		) SELECT id FROM family ORDER BY id`, hostedID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					descendants = append(descendants, id)
				}
			}
		}
		sort.Strings(ancestors)
		return ancestors, descendants
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{hostedID: true}
	current := hostedID
	for {
		record, ok := s.sessions[current]
		if !ok || record.InvokerHostedThreadID == "" || seen[record.InvokerHostedThreadID] {
			break
		}
		current = record.InvokerHostedThreadID
		seen[current] = true
		ancestors = append(ancestors, current)
	}
	queue := []string{hostedID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, record := range s.sessions {
			if record.InvokerHostedThreadID == parent && !seen[record.ID] {
				seen[record.ID] = true
				descendants = append(descendants, record.ID)
				queue = append(queue, record.ID)
			}
		}
	}
	sort.Strings(ancestors)
	sort.Strings(descendants)
	return ancestors, descendants
}

// TurnForAction finds the operation admitted for a hosted thread and client action.
func (s *ExecutionStore) TurnForAction(sessionID, actionID string) (threads.TurnRecord, bool) {
	if s == nil {
		return threads.TurnRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.byAction[agentActionKey(sessionID, actionID)]
	rec, ok := s.turns[id]
	return rec, ok
}

// persistedTurnByTransportQuery seeks one hosted thread's operations for one
// transport action through hosted_turn_operations_transport.
const persistedTurnByTransportQuery = `SELECT operation_id,COALESCE(provider_turn_id,''),status,COALESCE(error_code,''),COALESCE(final_item_id,''),COALESCE(origin,''),COALESCE(sender_submitted_at,'')
	FROM hosted_turn_operations WHERE hosted_thread_id=? AND transport_action_id=? LIMIT 2`

// PersistedTurnForTransportAction reads, from the committed operation rows
// rather than the cache, the one operation of a hosted thread delivered under
// a transport action: its provider turn, status, error code and final item.
// found is false only when the committed rows hold no such operation; an
// unreadable store, or two operations for one action, is an error, never an
// absence. The read assumes nothing about how the action id was formed.
func (s *ExecutionStore) PersistedTurnForTransportAction(ctx context.Context, hostedID, transportActionID string) (threads.TurnRecord, bool, error) {
	if s == nil || s.control == nil || hostedID == "" || transportActionID == "" {
		return threads.TurnRecord{}, false, ErrOperationReadUnavailable
	}
	rows, err := s.control.db.QueryContext(ctx, persistedTurnByTransportQuery, hostedID, transportActionID)
	if err != nil {
		return threads.TurnRecord{}, false, err
	}
	defer rows.Close()
	var found []threads.TurnRecord
	for rows.Next() {
		rec := threads.TurnRecord{HostedThreadID: hostedID, TransportActionID: transportActionID}
		var errorCode string
		if err := rows.Scan(&rec.ID, &rec.ProviderTurnID, &rec.Status, &errorCode, &rec.FinalItemID, &rec.Origin, &rec.SenderSubmittedAt); err != nil {
			return threads.TurnRecord{}, false, err
		}
		if errorCode != "" {
			rec.Error = &threads.ExecutionError{Code: errorCode}
		}
		found = append(found, rec)
	}
	if err := rows.Err(); err != nil {
		return threads.TurnRecord{}, false, err
	}
	switch len(found) {
	case 0:
		return threads.TurnRecord{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return threads.TurnRecord{}, false, fmt.Errorf("transport action %s has more than one operation", transportActionID)
	}
}

// TurnForTransportAction finds an operation by its transport action.
func (s *ExecutionStore) TurnForTransportAction(actionID string) (threads.TurnRecord, bool) {
	if s == nil || actionID == "" {
		return threads.TurnRecord{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.turns {
		if rec.TransportActionID == actionID {
			return rec, true
		}
	}
	return threads.TurnRecord{}, false
}

// OpenTurnsForProviderTurn lists the unfinished operations bound to one
// provider turn. Provider turn ids are scoped by the hosted thread and
// provider so a resumed or migrated transport cannot make an unrelated
// operation own the turn. Prompts steered into a running turn share it.
func (s *ExecutionStore) OpenTurnsForProviderTurn(hostedThreadID, provider, providerTurnID string) []threads.TurnRecord {
	if s == nil || hostedThreadID == "" || providerTurnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var open []threads.TurnRecord
	for _, rec := range s.turns {
		if rec.HostedThreadID == hostedThreadID && rec.ProviderTurnID == providerTurnID &&
			(provider == "" || rec.Provider == provider) && !threads.TurnTerminal(rec.Status) {
			open = append(open, rec)
		}
	}
	return open
}
