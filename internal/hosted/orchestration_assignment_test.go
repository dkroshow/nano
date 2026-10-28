package hosted

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestOrchestrationAssignmentNormalizesAndValidatesTargets(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := store.beginSession("parent", "parent", "codex", presentationShared)
	target, _, _ := store.beginSession("target-alias", "target", "codex", presentationShared)
	op, _, _ := store.beginTurn(target.ID, "result", "hash")
	s := testController(t, store)
	if view := s.Views().SessionView(parent.ID); view.Assignment != nil {
		t.Fatalf("unassigned thread defaulted to %#v", view.Assignment)
	}
	byAlias, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "  Audit it  ", AuditTargets: []AuditTarget{{HostedThreadID: "target-alias", OperationID: op.ID}}})
	if err != nil || byAlias.Focus != "Audit it" || byAlias.AuditTargets[0].HostedThreadID != target.ID {
		t.Fatalf("alias normalization = %#v, %v", byAlias, err)
	}
	byID, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "Audit it", AuditTargets: []AuditTarget{{HostedThreadID: target.ID, OperationID: op.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	firstHash, _ := json.Marshal(CreationRequest{Provider: "claude", ClientSessionID: "retry", InvokerThreadRef: parent.ID, Assignment: byAlias})
	secondHash, _ := json.Marshal(CreationRequest{Provider: "claude", ClientSessionID: "retry", InvokerThreadRef: parent.ID, Assignment: byID})
	if AgentHash(firstHash) != AgentHash(secondHash) {
		t.Fatalf("normalized target aliases changed creation hash: %s != %s", firstHash, secondHash)
	}
	related, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "worker", Focus: "Implement it", RelatedHostedThreadID: "target-alias"})
	if err != nil || related.RelatedHostedThreadID != target.ID {
		t.Fatalf("related thread normalization = %#v, %v", related, err)
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "worker", Focus: "x", RelatedHostedThreadID: "not-a-thread"}); err == nil {
		t.Fatal("unknown related thread accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "worker", Focus: "x", AuditTargets: byAlias.AuditTargets}); err == nil {
		t.Fatal("worker audit target accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "worker", Focus: string(bytes.Repeat([]byte("x"), 501))}); err == nil {
		t.Fatal("501-byte focus accepted")
	}
	exact, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "worker", Focus: string(bytes.Repeat([]byte("é"), 250))})
	if err != nil || len([]byte(exact.Focus)) != 500 {
		t.Fatalf("500-byte focus = %#v, err=%v", exact, err)
	}
	normalizedUnresolved, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []AuditTarget{{HostedThreadID: target.ID, Unresolved: true}}})
	if err != nil || normalizedUnresolved.AuditTargets[0].Unresolved {
		t.Fatalf("request unresolved leaked into canonical value: %#v, err=%v", normalizedUnresolved, err)
	}
	tooMany := make([]AuditTarget, 17)
	for i := range tooMany {
		tooMany[i] = AuditTarget{HostedThreadID: target.ID, OperationID: "op-" + string(rune('a'+i))}
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: tooMany}); err == nil {
		t.Fatal("17 targets accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []AuditTarget{{HostedThreadID: parent.ID, OperationID: op.ID}}}); err == nil {
		t.Fatal("cross-thread operation accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []AuditTarget{{HostedThreadID: "not-a-thread"}}}); err == nil {
		t.Fatal("unknown target accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&Assignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []AuditTarget{{HostedThreadID: target.ID}, {HostedThreadID: "target-alias"}}}); err == nil {
		t.Fatal("normalized duplicate accepted")
	}
	if _, err := control.appendOrchestrationAssignment(parent.ID, "record-target", &Assignment{AssignmentRole: "auditor", Focus: "Review target", AuditTargets: []AuditTarget{{HostedThreadID: target.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := control.destroy(target.ID); err != nil {
		t.Fatal(err)
	}
	historical, err := control.CurrentOrchestrationAssignment(parent.ID)
	if err != nil || historical == nil || len(historical.AuditTargets) != 1 || !historical.AuditTargets[0].Unresolved {
		t.Fatalf("deleted target projection = %#v, %v", historical, err)
	}
}
