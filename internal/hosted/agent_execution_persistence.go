package hosted

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/dkroshow/nano/internal/threads"
)

func boundAgentResult(value string) (string, int, bool) {
	original := len(value)
	if original <= maxAgentResultBytes {
		return value, original, false
	}
	return threads.TruncateText(value, maxAgentResultBytes), original, true
}

func touchAgentSession(rec *agentSessionRecord) {
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Revision++
}

func touchAgentTurn(rec *agentTurnRecord) {
	// Metadata reattachment cannot extend a saved-result deadline. The
	// awaiting transition sets its timestamp once, before this status is set.
	if rec.Status != agentTurnAwaitingSaved {
		rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	rec.Revision++
}

func (s *ExecutionStore) persistAndNotifyLocked() error {
	return s.persistAndNotifyWithLocked(nil)
}

// persistAndNotifyWithLocked commits the changed rows and with, a write owed
// with them, in one transaction.
func (s *ExecutionStore) persistAndNotifyWithLocked(with func(*sql.Tx) error) error {
	if err := s.persistLocked(with); err != nil {
		s.pendingTransitions = nil
		if s.control != nil {
			// Callers update their working maps before asking the repository to
			// commit. Restore from committed rows on failure so no speculative
			// session, action index, or turn leaks into later reads.
			s.resetControlCachesLocked()
			_ = loadControlExecutionRows(s)
		}
		return err
	}
	for _, transition := range s.pendingTransitions {
		if transition.To == agentTurnAwaitingSaved {
			s.startSavedResultLocked(transition.OperationID)
		}
	}
	s.publishPendingTransitionsLocked()
	return nil
}

func (s *ExecutionStore) publishPendingTransitionsLocked() {
	if s.changes != nil {
		for _, transition := range s.pendingTransitions {
			s.changes.append(transition)
		}
	}
	s.pendingTransitions = nil
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *ExecutionStore) persistLocked(with func(*sql.Tx) error) error {
	if s.control == nil {
		return fmt.Errorf("control execution store unavailable")
	}
	return s.persistControlLocked(with)
}

// persistControlLocked applies only rows whose revision changed and deletes
// only rows intentionally absent from the in-memory retention set. It never
// serializes the live control plane outside SQLite.
func (s *ExecutionStore) persistControlLocked(with func(*sql.Tx) error) error {
	if s.deletedSessions == nil {
		s.deletedSessions = map[string]bool{}
	}
	if s.deletedTurns == nil {
		s.deletedTurns = map[string]bool{}
	}
	// Only changed threads can shed surplus, and only when the prompt owner
	// proves that their prompts can never owe a member completion.
	for hostedID := range s.dirtyTurnThreads {
		if len(s.turnsByHosted[hostedID]) <= maxAgentTurns || s.pruneEligible == nil {
			continue
		}
		records := make([]agentTurnRecord, 0, len(s.turnsByHosted[hostedID]))
		changed := false
		for id := range s.turnsByHosted[hostedID] {
			rec := s.turns[id]
			if rec.Revision != s.turnBaseline[id] {
				changed = true
			}
			if threads.TurnTerminal(rec.Status) {
				records = append(records, rec)
			}
		}
		if !changed || len(records) <= maxAgentTurns {
			continue
		}
		sort.Slice(records, func(i, j int) bool { return threads.TimestampBefore(records[j].UpdatedAt, records[i].UpdatedAt) })
		surplus := records[maxAgentTurns:]
		for start := 0; start < len(surplus); start += maxPruneEligibleBatch {
			chunk := surplus[start:min(start+maxPruneEligibleBatch, len(surplus))]
			actions := make([]string, len(chunk))
			for i, rec := range chunk {
				actions[i] = rec.TransportActionID
			}
			ctx, cancel := context.WithTimeout(context.Background(), pruneEligibleReadTimeout)
			eligible, err := s.pruneEligible(ctx, hostedID, actions)
			cancel()
			if err != nil {
				log.Printf("retention of %s keeps %d operations whose prompts are unknown: %v", hostedID, len(chunk), err)
				continue
			}
			for _, rec := range chunk {
				if eligible[rec.TransportActionID] {
					s.setTurnLocked(rec.ID, nil)
				}
			}
		}
	}
	tx, err := s.control.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, rec := range s.sessions {
		expected := s.sessionBaseline[id]
		if rec.Revision == expected {
			continue
		}
		result, err := tx.Exec(`INSERT INTO hosted_execution_sessions(hosted_thread_id,client_session_id,request_hash,nano_session_id,status,error,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(hosted_thread_id) DO UPDATE SET client_session_id=excluded.client_session_id,request_hash=excluded.request_hash,nano_session_id=excluded.nano_session_id,status=excluded.status,error=excluded.error,run_id=excluded.run_id,updated_at=excluded.updated_at,revision=excluded.revision WHERE hosted_execution_sessions.revision=?`, rec.ID, nullableText(rec.ClientSessionID), rec.RequestHash, nullableText(rec.NanoSessionID), rec.Status, nullableText(rec.Error), rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision, expected)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	for id := range s.deletedSessions {
		result, err := tx.Exec(`DELETE FROM hosted_execution_sessions WHERE hosted_thread_id=? AND revision=?`, id, s.sessionBaseline[id])
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	for id := range s.dirtyTurns {
		rec, ok := s.turns[id]
		if !ok {
			continue
		}
		expected := s.turnBaseline[id]
		if rec.Revision == expected {
			continue
		}
		if err := upsertTurnOperationCAS(tx, rec, expected); err != nil {
			return err
		}
	}
	for id := range s.deletedTurns {
		result, err := tx.Exec(`DELETE FROM hosted_turn_operations WHERE operation_id=? AND revision=?`, id, s.turnBaseline[id])
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	if with != nil {
		if err := with(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for id, rec := range s.sessions {
		s.sessionBaseline[id] = rec.Revision
	}
	for id := range s.dirtyTurns {
		if rec, ok := s.turns[id]; ok {
			s.turnBaseline[id] = rec.Revision
		} else {
			delete(s.turnBaseline, id)
		}
	}
	for id := range s.deletedSessions {
		delete(s.sessionBaseline, id)
	}
	s.dirtyTurnThreads, s.dirtyTurns = map[string]bool{}, map[string]bool{}
	s.deletedSessions, s.deletedTurns = map[string]bool{}, map[string]bool{}
	return nil
}

func insertTurnOperation(tx *sql.Tx, rec agentTurnRecord) error {
	return upsertTurnOperation(tx, rec)
}

func upsertTurnOperation(tx *sql.Tx, rec agentTurnRecord) error {
	return writeTurnOperation(tx, rec, nil)
}

func upsertTurnOperationCAS(tx *sql.Tx, rec agentTurnRecord, expected int64) error {
	return writeTurnOperation(tx, rec, &expected)
}

func writeTurnOperation(tx *sql.Tx, rec agentTurnRecord, expected *int64) error {
	var source, code, message, details any
	if rec.Error != nil {
		source, code, message = nullableText(rec.Error.Source), nullableText(rec.Error.Code), nullableText(rec.Error.Message)
		if len(rec.Error.Details) > 0 {
			details = string(rec.Error.Details)
		}
	}
	query := `INSERT INTO hosted_turn_operations(operation_id,hosted_thread_id,client_action_id,transport_action_id,prompt_hash,assignment_event_id,provider_turn_id,status,error_source,error_code,error_message,error_details_json,final_item_id,final_response,final_response_truncated,final_response_bytes,candidate_item_id,candidate_phase,candidate_response,candidate_bytes,candidate_truncated,origin,sender_hosted_id,sender_native_id,sender_provider,sender_name,sender_submitted_at,run_id,created_at,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(operation_id) DO UPDATE SET provider_turn_id=excluded.provider_turn_id,status=excluded.status,error_source=excluded.error_source,error_code=excluded.error_code,error_message=excluded.error_message,error_details_json=excluded.error_details_json,final_item_id=excluded.final_item_id,final_response=excluded.final_response,final_response_truncated=excluded.final_response_truncated,final_response_bytes=excluded.final_response_bytes,candidate_item_id=excluded.candidate_item_id,candidate_phase=excluded.candidate_phase,candidate_response=excluded.candidate_response,candidate_bytes=excluded.candidate_bytes,candidate_truncated=excluded.candidate_truncated,run_id=excluded.run_id,updated_at=excluded.updated_at,revision=excluded.revision`
	args := []any{rec.ID, rec.HostedThreadID, rec.ClientActionID, rec.TransportActionID, rec.PromptHash, nullableInt64(rec.AssignmentEventID), nullableText(rec.ProviderTurnID), rec.Status, source, code, message, details, nullableText(rec.FinalItemID), nullableText(rec.FinalResponse), rec.FinalResponseTruncated, rec.FinalResponseBytes, nullableText(rec.CandidateItemID), nullableText(rec.CandidatePhase), nullableText(rec.CandidateResponse), rec.CandidateBytes, rec.CandidateTruncated, nullableText(rec.Origin), nullableText(rec.SenderHostedThreadID), nullableText(rec.SenderNativeThreadID), nullableText(rec.SenderProvider), nullableText(rec.SenderName), nullableText(rec.SenderSubmittedAt), rec.RunID, rec.CreatedAt, rec.UpdatedAt, rec.Revision}
	if expected != nil {
		query += ` WHERE hosted_turn_operations.revision=?`
		args = append(args, *expected)
	}
	result, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if expected != nil {
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errExecutionConflict
		}
	}
	return nil
}

var errExecutionConflict = errors.New("execution control row changed concurrently")

// errAgentExecutionUnavailable reports unavailable canonical execution storage.
var errAgentExecutionUnavailable = errors.New("agent execution store unavailable")
