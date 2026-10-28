package hosted

import (
	"fmt"
	"strings"
)

// normalizeOrchestrationAssignment makes aliases a transport concern rather
// than part of stored evidence or creation idempotency.
func (s *Controller) normalizeOrchestrationAssignment(input *Assignment) (*Assignment, error) {
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
		record, ok := s.ResolveHostedRecord(value.RelatedHostedThreadID)
		if !ok {
			return nil, fmt.Errorf("relatedHostedThreadId does not resolve to a hosted thread")
		}
		value.RelatedHostedThreadID = record.ID
	}
	value.AuditTargets = append([]AuditTarget(nil), value.AuditTargets...)
	seen := map[string]bool{}
	for i := range value.AuditTargets {
		target := &value.AuditTargets[i]
		target.Unresolved = false // read projection only; never canonical input.
		target.HostedThreadID = strings.TrimSpace(target.HostedThreadID)
		target.OperationID = strings.TrimSpace(target.OperationID)
		if target.HostedThreadID == "" {
			return nil, fmt.Errorf("audit target hostedThreadId is required")
		}
		record, ok := s.ResolveHostedRecord(target.HostedThreadID)
		if !ok {
			return nil, fmt.Errorf("audit target does not resolve to a hosted thread")
		}
		target.HostedThreadID = record.ID
		if target.OperationID != "" {
			op, ok := s.Executions.Turn(target.OperationID)
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
		value.AuditTargets = []AuditTarget{}
	}
	return &value, nil
}

// validateAssignmentRelationship rejects visual-parent cycles while leaving
// the immutable invocation edge untouched. The mutable edge wins when it is
// present; clearing an assignment exposes the original invocation lineage
// again.
func (s *Controller) validateAssignmentRelationship(hostedID string, assignment *Assignment) error {
	if assignment == nil || assignment.RelatedHostedThreadID == "" {
		return nil
	}
	subject, ok := s.Executions.Session(hostedID)
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
		record, ok := s.Executions.Session(cursor)
		if !ok {
			return nil
		}
		parentID := record.InvokerHostedThreadID
		current, err := s.Metadata.CurrentOrchestrationAssignment(record.ID)
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
