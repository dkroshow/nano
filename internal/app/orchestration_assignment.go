package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const orchestrationAssignmentField = "orchestrationAssignment"

var errAssignmentIdempotencyConflict = errors.New("assignment idempotency conflict")

// orchestrationAssignment is deliberately small. It records the execution
// assignment Nano accepted; workflow ownership remains with the caller.
type orchestrationAssignment struct {
	AssignmentRole        string                     `json:"assignmentRole"`
	Focus                 string                     `json:"focus"`
	RelatedHostedThreadID string                     `json:"relatedHostedThreadId,omitempty"`
	AuditTargets          []orchestrationAuditTarget `json:"auditTargets"`
}

type orchestrationAuditTarget struct {
	HostedThreadID string `json:"hostedThreadId"`
	OperationID    string `json:"operationId,omitempty"`
	Unresolved     bool   `json:"unresolved,omitempty"`
}

type orchestrationAssignmentView struct {
	EventID               int64                      `json:"eventId"`
	AssignmentRole        string                     `json:"assignmentRole"`
	Focus                 string                     `json:"focus"`
	RelatedHostedThreadID string                     `json:"relatedHostedThreadId,omitempty"`
	AuditTargets          []orchestrationAuditTarget `json:"auditTargets"`
}

// normalizeOrchestrationAssignment makes aliases a transport concern rather
// than part of stored evidence or creation idempotency.
func (s *server) normalizeOrchestrationAssignment(input *orchestrationAssignment) (*orchestrationAssignment, error) {
	if input == nil {
		return nil, nil
	}
	value := *input
	value.AssignmentRole = strings.TrimSpace(value.AssignmentRole)
	value.Focus = strings.TrimSpace(value.Focus)
	value.RelatedHostedThreadID = strings.TrimSpace(value.RelatedHostedThreadID)
	if value.AssignmentRole != "worker" && value.AssignmentRole != "auditor" {
		return nil, fmt.Errorf("assignmentRole must be worker or auditor")
	}
	if value.Focus == "" || len([]byte(value.Focus)) > 500 {
		return nil, fmt.Errorf("assignment focus is required and must be at most 500 UTF-8 bytes")
	}
	if value.AssignmentRole == "worker" && len(value.AuditTargets) != 0 {
		return nil, fmt.Errorf("auditTargets are allowed only for an auditor assignment")
	}
	if len(value.AuditTargets) > 16 {
		return nil, fmt.Errorf("at most 16 auditTargets are allowed")
	}
	if value.RelatedHostedThreadID != "" {
		record, ok := s.resolveHostedThreadRecord(value.RelatedHostedThreadID)
		if !ok {
			return nil, fmt.Errorf("relatedHostedThreadId does not resolve to a hosted thread")
		}
		value.RelatedHostedThreadID = record.ID
	}
	value.AuditTargets = append([]orchestrationAuditTarget(nil), value.AuditTargets...)
	seen := map[string]bool{}
	for i := range value.AuditTargets {
		target := &value.AuditTargets[i]
		target.Unresolved = false // read projection only; never canonical input.
		target.HostedThreadID = strings.TrimSpace(target.HostedThreadID)
		target.OperationID = strings.TrimSpace(target.OperationID)
		if target.HostedThreadID == "" {
			return nil, fmt.Errorf("audit target hostedThreadId is required")
		}
		record, ok := s.resolveHostedThreadRecord(target.HostedThreadID)
		if !ok {
			return nil, fmt.Errorf("audit target does not resolve to a hosted thread")
		}
		target.HostedThreadID = record.ID
		if target.OperationID != "" {
			op, ok := s.agentExecutions.turn(target.OperationID)
			if !ok {
				return nil, fmt.Errorf("audit target operation does not exist")
			}
			if op.HostedThreadID != record.ID {
				return nil, fmt.Errorf("audit target operation does not belong to its hosted thread")
			}
		}
		key := target.HostedThreadID + "\x00" + target.OperationID
		if seen[key] {
			return nil, fmt.Errorf("duplicate audit target")
		}
		seen[key] = true
	}
	if value.AuditTargets == nil {
		value.AuditTargets = []orchestrationAuditTarget{}
	}
	return &value, nil
}

// validateAssignmentRelationship rejects visual-parent cycles while leaving
// the immutable invocation edge untouched. The mutable edge wins when it is
// present; clearing an assignment exposes the original invocation lineage
// again.
func (s *server) validateAssignmentRelationship(hostedID string, assignment *orchestrationAssignment) error {
	if assignment == nil || assignment.RelatedHostedThreadID == "" {
		return nil
	}
	subject, ok := s.agentExecutions.session(hostedID)
	if !ok {
		return fmt.Errorf("hosted thread does not exist")
	}
	if assignment.RelatedHostedThreadID == subject.ID {
		return fmt.Errorf("relatedHostedThreadId cannot refer to the same thread")
	}

	cursor := assignment.RelatedHostedThreadID
	visited := map[string]bool{}
	for cursor != "" {
		if cursor == subject.ID {
			return fmt.Errorf("relatedHostedThreadId would create a thread relationship cycle")
		}
		if visited[cursor] {
			return fmt.Errorf("existing thread relationships contain a cycle")
		}
		visited[cursor] = true
		record, ok := s.agentExecutions.session(cursor)
		if !ok {
			return nil
		}
		parentID := record.InvokerHostedThreadID
		current, err := s.controlMetadata.currentOrchestrationAssignment(record.ID)
		if err != nil {
			return fmt.Errorf("resolve related thread lineage: %w", err)
		}
		if current != nil && current.RelatedHostedThreadID != "" {
			parentID = current.RelatedHostedThreadID
		}
		cursor = parentID
	}
	return nil
}

func assignmentEventValue(assignment *orchestrationAssignment) any {
	if assignment == nil {
		return nil
	}
	return *assignment
}

// orchestrationAssignmentActionExists preserves retry semantics even when the
// relationship graph changed after the original action. A matching retry
// names old evidence; it does not apply that assignment again, so current
// cycle validation must not reject it.
func (s *controlMetadataStore) orchestrationAssignmentActionExists(hostedID, actionID string, assignment *orchestrationAssignment) (bool, error) {
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
	if priorHostedID != hostedID || field != orchestrationAssignmentField || priorValue != string(value) || priorDisposition != disposition || source != postureSourceOperator {
		return false, fmt.Errorf("%w: assignment action %q has different content", errAssignmentIdempotencyConflict, actionID)
	}
	return true, nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *controlMetadataStore) currentOrchestrationAssignment(hostedID string) (*orchestrationAssignmentView, error) {
	if s == nil {
		return nil, fmt.Errorf("canonical metadata unavailable")
	}
	var eventID sql.NullInt64
	var disposition string
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT desired_event_id,desired_disposition,desired_value_json FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, hostedID, orchestrationAssignmentField).Scan(&eventID, &disposition, &raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if disposition == metadataDispositionCleared || !eventID.Valid || !raw.Valid {
		return nil, nil
	}
	var assignment orchestrationAssignment
	if err := json.Unmarshal([]byte(raw.String), &assignment); err != nil {
		return nil, fmt.Errorf("decode orchestration assignment: %w", err)
	}
	if err := s.markUnresolvedAssignmentTargets(&assignment); err != nil {
		return nil, err
	}
	return &orchestrationAssignmentView{EventID: eventID.Int64, AssignmentRole: assignment.AssignmentRole, Focus: assignment.Focus, RelatedHostedThreadID: assignment.RelatedHostedThreadID, AuditTargets: assignment.AuditTargets}, nil
}

func (s *controlMetadataStore) markUnresolvedAssignmentTargets(assignment *orchestrationAssignment) error {
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

func (s *controlMetadataStore) operationOrchestrationAssignment(eventID *int64) (*orchestrationAssignmentView, error) {
	if eventID == nil {
		return nil, nil
	}
	var raw string
	if err := s.db.QueryRow(`SELECT value_json FROM thread_metadata_events WHERE event_id=? AND field=?`, *eventID, orchestrationAssignmentField).Scan(&raw); err != nil {
		return nil, err
	}
	var assignment orchestrationAssignment
	if err := json.Unmarshal([]byte(raw), &assignment); err != nil {
		return nil, err
	}
	if err := s.markUnresolvedAssignmentTargets(&assignment); err != nil {
		return nil, err
	}
	return &orchestrationAssignmentView{EventID: *eventID, AssignmentRole: assignment.AssignmentRole, Focus: assignment.Focus, RelatedHostedThreadID: assignment.RelatedHostedThreadID, AuditTargets: assignment.AuditTargets}, nil
}

// appendInitialOrchestrationAssignmentTx is intentionally narrow: creation
// needs hosted identity, execution row, and initial assignment in one commit.
func appendInitialOrchestrationAssignmentTx(tx *sql.Tx, hostedID string, assignment *orchestrationAssignment, now string) (int64, int64, error) {
	if assignment == nil {
		return 0, 0, nil
	}
	raw, err := json.Marshal(assignment)
	if err != nil {
		return 0, 0, err
	}
	result, err := tx.Exec(`INSERT INTO thread_metadata_events(hosted_thread_id,field,value_json,disposition,source,evidence,attachment_generation,evidence_freshness,source_event_key,observed_at,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, hostedID, orchestrationAssignmentField, string(raw), metadataDispositionConfirmed, postureSourceCreation, "", 0, metadataFreshnessLive, nil, now, now)
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
	_, err = tx.Exec(`INSERT INTO thread_metadata_current(hosted_thread_id,field,desired_value_json,desired_disposition,desired_source,desired_event_id,desired_settlement_event_id,effective_value_json,effective_disposition,effective_source,effective_event_id,thread_revision,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, hostedID, orchestrationAssignmentField, string(raw), metadataDispositionConfirmed, postureSourceCreation, eventID, nil, string(raw), metadataDispositionConfirmed, postureSourceCreation, eventID, revision, now)
	return eventID, revision, err
}

func (s *controlMetadataStore) appendOrchestrationAssignment(hostedID, actionID string, assignment *orchestrationAssignment) (metadataEvent, error) {
	disposition := metadataDispositionConfirmed
	if assignment == nil {
		disposition = metadataDispositionCleared
	}
	return s.AppendEvent(metadataEvent{HostedThreadID: hostedID, Field: orchestrationAssignmentField, Value: assignmentEventValue(assignment), Disposition: disposition, Source: postureSourceOperator, Freshness: metadataFreshnessLive, SourceEventKey: "assignment:" + hostedID + ":" + actionID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}
