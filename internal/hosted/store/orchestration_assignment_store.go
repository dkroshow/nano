package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AssignmentField names the canonical metadata field for orchestration assignments.
const AssignmentField = "orchestrationAssignment"

// ErrAssignmentIdempotencyConflict reports a reused assignment action with different content.
var ErrAssignmentIdempotencyConflict = errors.New("assignment idempotency conflict")

// Assignment is deliberately small. It records the execution
// assignment Nano accepted; workflow ownership remains with the caller.
type Assignment struct {
	AssignmentRole        string        `json:"assignmentRole"`
	Focus                 string        `json:"focus"`
	RelatedHostedThreadID string        `json:"relatedHostedThreadId,omitempty"`
	AuditTargets          []AuditTarget `json:"auditTargets"`
}

// AuditTarget names a hosted thread and optional operation included in an assignment.
type AuditTarget struct {
	HostedThreadID string `json:"hostedThreadId"`
	OperationID    string `json:"operationId,omitempty"`
	Unresolved     bool   `json:"unresolved,omitempty"`
}

// AssignmentView presents an assignment with its canonical evidence event ID.
type AssignmentView struct {
	EventID               int64         `json:"eventId"`
	AssignmentRole        string        `json:"assignmentRole"`
	Focus                 string        `json:"focus"`
	RelatedHostedThreadID string        `json:"relatedHostedThreadId,omitempty"`
	AuditTargets          []AuditTarget `json:"auditTargets"`
}

func assignmentEventValue(assignment *Assignment) any {
	if assignment == nil {
		return nil
	}
	return *assignment
}

// OrchestrationAssignmentActionExists preserves retry semantics even when the
// relationship graph changed after the original action. A matching retry
// names old evidence; it does not apply that assignment again, so current
// cycle validation must not reject it.
func (s *MetadataStore) OrchestrationAssignmentActionExists(hostedID, actionID string, assignment *Assignment) (bool, error) {
	value, err := json.Marshal(assignmentEventValue(assignment))
	if err != nil {
		return false, err
	}
	disposition := metadataDispositionConfirmed
	if assignment == nil {
		disposition = metadataDispositionCleared
	}
	var priorHostedID, field, priorValue, priorDisposition, source string
	err = s.db.QueryRow(`SELECT hosted_thread_id,field,value_json,disposition,source FROM thread_metadata_events WHERE source_event_key=?`, "assignment:"+hostedID+":"+actionID).Scan(&priorHostedID, &field, &priorValue, &priorDisposition, &source)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if priorHostedID != hostedID || field != AssignmentField || priorValue != string(value) || priorDisposition != disposition || source != PostureSourceOperator {
		return false, fmt.Errorf("%w: assignment action %q has different content", ErrAssignmentIdempotencyConflict, actionID)
	}
	return true, nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// CurrentOrchestrationAssignment reads the current accepted assignment for a hosted thread.
func (s *MetadataStore) CurrentOrchestrationAssignment(hostedID string) (*AssignmentView, error) {
	if s == nil {
		return nil, fmt.Errorf("canonical metadata unavailable")
	}
	var eventID sql.NullInt64
	var disposition string
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT desired_event_id,desired_disposition,desired_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, AssignmentField).Scan(&eventID, &disposition, &raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if disposition == metadataDispositionCleared || !eventID.Valid || !raw.Valid {
		return nil, nil
	}
	var assignment Assignment
	if err := json.Unmarshal([]byte(raw.String), &assignment); err != nil {
		return nil, fmt.Errorf("decode orchestration assignment: %w", err)
	}
	if err := s.markUnresolvedAssignmentTargets(&assignment); err != nil {
		return nil, err
	}
	return &AssignmentView{EventID: eventID.Int64, AssignmentRole: assignment.AssignmentRole, Focus: assignment.Focus, RelatedHostedThreadID: assignment.RelatedHostedThreadID, AuditTargets: assignment.AuditTargets}, nil
}

func (s *MetadataStore) markUnresolvedAssignmentTargets(assignment *Assignment) error {
	for i := range assignment.AuditTargets {
		var exists bool
		err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM hosted_threads WHERE hosted_thread_id=?)`, assignment.AuditTargets[i].HostedThreadID).Scan(&exists)
		if err != nil {
			return err
		}
		assignment.AuditTargets[i].Unresolved = !exists
	}
	return nil
}

// OperationOrchestrationAssignment reads the assignment snapshot admitted with an operation.
func (s *MetadataStore) OperationOrchestrationAssignment(eventID *int64) (*AssignmentView, error) {
	if eventID == nil {
		return nil, nil
	}
	var raw string
	if err := s.db.QueryRow(`SELECT value_json FROM thread_metadata_events WHERE event_id=? AND field=?`, *eventID, AssignmentField).Scan(&raw); err != nil {
		return nil, err
	}
	var assignment Assignment
	if err := json.Unmarshal([]byte(raw), &assignment); err != nil {
		return nil, err
	}
	if err := s.markUnresolvedAssignmentTargets(&assignment); err != nil {
		return nil, err
	}
	return &AssignmentView{EventID: *eventID, AssignmentRole: assignment.AssignmentRole, Focus: assignment.Focus, RelatedHostedThreadID: assignment.RelatedHostedThreadID, AuditTargets: assignment.AuditTargets}, nil
}

// appendInitialOrchestrationAssignmentTx is intentionally narrow: creation
// needs hosted identity, execution row, and initial assignment in one commit.
func appendInitialOrchestrationAssignmentTx(tx *sql.Tx, hostedID string, assignment *Assignment, now string) (int64, int64, error) {
	if assignment == nil {
		return 0, 0, nil
	}
	raw, err := json.Marshal(assignment)
	if err != nil {
		return 0, 0, err
	}
	result, err := tx.Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,attachment_generation,evidence_freshness,source_event_key,observed_at,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, hostedID, AssignmentField, string(raw), metadataDispositionConfirmed, PostureSourceCreation, "", 0, metadataFreshnessLive, nil, now, now)
	if err != nil {
		return 0, 0, err
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	var revision int64
	if err := tx.QueryRow(`UPDATE hosted_threads SET revision=revision+1,updated_at=? WHERE hosted_thread_id=? RETURNING revision`, now, hostedID).Scan(&revision); err != nil {
		return 0, 0, err
	}
	_, err = tx.Exec(`INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_value_json,desired_disposition,desired_source,desired_event_id,desired_settlement_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id,thread_revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, hostedID, AssignmentField, string(raw), metadataDispositionConfirmed, PostureSourceCreation, eventID, nil, string(raw), metadataDispositionConfirmed, PostureSourceCreation, eventID, revision, now)
	return eventID, revision, err
}

// AppendOrchestrationAssignment idempotently records an assignment change as canonical metadata.
func (s *MetadataStore) AppendOrchestrationAssignment(hostedID, actionID string, assignment *Assignment) (metadataEvent, error) {
	disposition := metadataDispositionConfirmed
	if assignment == nil {
		disposition = metadataDispositionCleared
	}
	return s.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: AssignmentField, Value: assignmentEventValue(assignment), Disposition: disposition, Source: PostureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: "assignment:" + hostedID + ":" + actionID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
