package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrchestrationAssignmentLifecycleSnapshotsAndProjections(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	invoker, _, err := store.beginSession("parent-alias", "parent", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := store.beginSessionWithInvoker("worker-alias", "worker", "claude", presentationShared, invoker, &orchestrationAssignment{AssignmentRole: "worker", Focus: "Implement exactly this"})
	if err != nil {
		t.Fatal(err)
	}
	s := &server{agentExecutions: store, controlMetadata: control, sessions: map[string]*session{}}

	first, err := control.currentOrchestrationAssignment(worker.ID)
	if err != nil || first == nil || first.AssignmentRole != "worker" || first.Focus != "Implement exactly this" {
		t.Fatalf("initial assignment = %#v, %v", first, err)
	}
	var desired, effective string
	if err := control.db.QueryRow(`SELECT desired_disposition,effective_disposition FROM thread_metadata_current WHERE hosted_thread_id=? AND field=?`, worker.ID, orchestrationAssignmentField).Scan(&desired, &effective); err != nil || desired != metadataDispositionConfirmed || effective != metadataDispositionConfirmed {
		t.Fatalf("initial authoritative projection = %q/%q, err=%v", desired, effective, err)
	}
	metadataView, err := control.view(worker.ID)
	if err != nil || metadataView.Fields[orchestrationAssignmentField].DesiredDisposition != metadataDispositionConfirmed || metadataView.Fields[orchestrationAssignmentField].EffectiveDisposition != metadataDispositionConfirmed || metadataView.Fields[orchestrationAssignmentField].DisplayDisposition != metadataDispositionConfirmed {
		t.Fatalf("generic metadata assignment view = %#v, err=%v", metadataView.Fields[orchestrationAssignmentField], err)
	}
	retry, created, err := store.beginSessionWithInvoker("worker-alias", "worker", "claude", presentationShared, invoker, &orchestrationAssignment{AssignmentRole: "worker", Focus: "Implement exactly this"})
	if err != nil || created || retry.ID != worker.ID {
		t.Fatalf("creation retry = %#v, created=%v, err=%v", retry, created, err)
	}
	var initialEvents int
	if err := control.db.QueryRow(`SELECT count(*) FROM thread_metadata_events WHERE hosted_thread_id=? AND field=?`, worker.ID, orchestrationAssignmentField).Scan(&initialEvents); err != nil || initialEvents != 1 {
		t.Fatalf("creation retry assignment events = %d, err=%v", initialEvents, err)
	}
	op1, created, err := store.beginTurn(worker.ID, "one", "prompt-1")
	if err != nil || !created || op1.AssignmentEventID == nil || *op1.AssignmentEventID != first.EventID {
		t.Fatalf("first operation snapshot = %#v, created=%v err=%v", op1, created, err)
	}
	if err := store.markTurnDelivery(op1.ID, "provider-one", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"clientActionId":"assign-audit","assignment":{"assignmentRole":"auditor","focus":"Review the worker","auditTargets":[{"hostedThreadId":"worker-alias","operationId":"` + op1.ID + `"}]}}`)
	rec := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/worker-alias/assignment", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("assignment update = %d %s", rec.Code, rec.Body.String())
	}
	var updated agentSessionView
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Assignment == nil || updated.Assignment.AssignmentRole != "auditor" || len(updated.Assignment.AuditTargets) != 1 {
		t.Fatalf("thread projection = %#v", updated.Assignment)
	}
	var updateResult struct {
		AssignmentEventID int64 `json:"assignmentEventId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &updateResult); err != nil || updateResult.AssignmentEventID != updated.Assignment.EventID {
		t.Fatalf("assignment mutation result = %#v, err=%v", updateResult, err)
	}
	listed := httptest.NewRecorder()
	s.handleAgentSessionsList(listed, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads", nil))
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(`"assignmentRole":"auditor"`)) {
		t.Fatalf("thread list projection = %d %s", listed.Code, listed.Body.String())
	}
	detail := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/worker-alias", nil))
	if detail.Code != http.StatusOK || !bytes.Contains(detail.Body.Bytes(), []byte(`"assignmentRole":"auditor"`)) {
		t.Fatalf("thread detail projection = %d %s", detail.Code, detail.Body.String())
	}
	second := updated.Assignment.EventID
	// Same action and content is one immutable evidence event.
	rec = httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+worker.ID+"/assignment", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("assignment retry = %d %s", rec.Code, rec.Body.String())
	}
	conflict := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(conflict, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+worker.ID+"/assignment", bytes.NewBufferString(`{"clientActionId":"assign-audit","assignment":{"assignmentRole":"auditor","focus":"Different content","auditTargets":[]}}`)))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("assignment action conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	if got, _ := control.currentOrchestrationAssignment(worker.ID); got.EventID != second {
		t.Fatalf("retry event = %#v, want %d", got, second)
	}
	// A new action is an intentional new immutable event, even for identical content.
	reassert := httptest.NewRecorder()
	reassertBody := bytes.Replace(body, []byte(`"assign-audit"`), []byte(`"reassert-audit"`), 1)
	applicationHandler(s).ServeHTTP(reassert, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+worker.ID+"/assignment", bytes.NewReader(reassertBody)))
	if reassert.Code != http.StatusOK {
		t.Fatalf("reassert = %d %s", reassert.Code, reassert.Body.String())
	}
	var reassertResult struct {
		AssignmentEventID int64 `json:"assignmentEventId"`
	}
	_ = json.Unmarshal(reassert.Body.Bytes(), &reassertResult)
	if reassertResult.AssignmentEventID == second {
		t.Fatalf("new assignment action reused event %d", second)
	}
	oldRetry := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(oldRetry, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+worker.ID+"/assignment", bytes.NewReader(body)))
	var oldRetryResult struct {
		AssignmentEventID int64 `json:"assignmentEventId"`
	}
	_ = json.Unmarshal(oldRetry.Body.Bytes(), &oldRetryResult)
	if oldRetry.Code != http.StatusOK || oldRetryResult.AssignmentEventID != second {
		t.Fatalf("old retry event = %#v, status=%d", oldRetryResult, oldRetry.Code)
	}
	// Existing work keeps the old event after both reassignment and status upsert.
	if err := store.markTurnDelivery(op1.ID, "", agentTurnCompleted, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := store.turn(op1.ID)
	if after.AssignmentEventID == nil || *after.AssignmentEventID != first.EventID {
		t.Fatalf("status upsert rewrote operation assignment = %#v", after)
	}
	op2, _, err := store.beginTurn(worker.ID, "two", "prompt-2")
	if err != nil || op2.AssignmentEventID == nil || *op2.AssignmentEventID != reassertResult.AssignmentEventID {
		t.Fatalf("next operation snapshot = %#v, %v", op2, err)
	}
	if err := store.markTurnDelivery(op2.ID, "", agentTurnQueued, nil); err != nil {
		t.Fatal(err)
	}
	queued, _ := store.turn(op2.ID)
	if queued.AssignmentEventID == nil || *queued.AssignmentEventID != reassertResult.AssignmentEventID {
		t.Fatalf("queued snapshot = %#v", queued)
	}
	if view := s.agentTurnView(op1); view.Assignment == nil || view.Assignment.EventID != first.EventID {
		t.Fatalf("operation projection = %#v", view.Assignment)
	}
	bulk := httptest.NewRecorder()
	s.handleTurnOperationsCollection(bulk, httptest.NewRequest(http.MethodGet, "/api/v1/turn-operations?ids="+op1.ID+","+op2.ID, nil))
	if bulk.Code != http.StatusOK || !bytes.Contains(bulk.Body.Bytes(), []byte(`"assignmentRole":"worker"`)) || !bytes.Contains(bulk.Body.Bytes(), []byte(`"assignmentRole":"auditor"`)) {
		t.Fatalf("bulk projection = %d %s", bulk.Code, bulk.Body.String())
	}

	clear := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(clear, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/"+worker.ID+"/assignment", bytes.NewBufferString(`{"clientActionId":"clear-audit","assignment":null}`)))
	if clear.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", clear.Code, clear.Body.String())
	}
	var clearResult struct {
		AssignmentEventID int64 `json:"assignmentEventId"`
	}
	_ = json.Unmarshal(clear.Body.Bytes(), &clearResult)
	if clearResult.AssignmentEventID == 0 {
		t.Fatalf("clear omitted assignment event: %s", clear.Body.String())
	}
	if got, err := control.currentOrchestrationAssignment(worker.ID); err != nil || got != nil {
		t.Fatalf("cleared current = %#v, %v", got, err)
	}
	op3, _, err := store.beginTurn(worker.ID, "three", "prompt-3")
	if err != nil || op3.AssignmentEventID != nil {
		t.Fatalf("cleared operation = %#v, %v", op3, err)
	}
	page, err := control.eventsAfter(worker.ID, 0, 20, metadataEventFilter{Field: orchestrationAssignmentField})
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("assignment history = %#v, %v", page, err)
	}

	// Loading the normalized rows after a restart preserves immutable snapshots.
	reloaded, err := newControlAgentExecutionStore(control, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	reloadedOp, ok := reloaded.turn(op1.ID)
	if !ok || reloadedOp.AssignmentEventID == nil || *reloadedOp.AssignmentEventID != first.EventID {
		t.Fatalf("restart operation = %#v", reloadedOp)
	}
}

func TestOrchestrationAssignmentDoesNotMutateCallerPrompt(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := store.beginSession("parent", "parent", "codex", presentationShared)
	worker, _, err := store.beginSessionWithInvoker("worker", "worker", "codex", presentationShared, parent, &orchestrationAssignment{AssignmentRole: "worker", Focus: "Do not inject this"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.bindSession(worker.ID, "nano-worker", "native-worker", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	prompts := make(chan string, 1)
	runtimeDir := t.TempDir()
	queue, err := newTestQueuedTurnStore(t, filepath.Join(runtimeDir, "queued-turns.json"))
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := newTestSendAttemptStore(t, filepath.Join(runtimeDir, "send-attempts.json"), "assignment-test")
	if err != nil {
		t.Fatal(err)
	}
	turnStore := mustBindTransactionalTurnStore(t, queue, attempts)
	sess := &session{id: "nano-worker",
		provider:        "codex",
		hostedThreadID:  worker.ID,
		threadID:        "native-worker",
		createdAt:       time.Now(),
		interactions:    map[string]pendingInteraction{},
		projection:      newLiveProjection("codex", worker.ID),
		agentExecutions: store,
		sendOps:         map[string]*turnSendOperation{},
		turns:           turnStore,
		queuedTurns:     queue,
		sendAttempts:    attempts,
		lifecycleState:  testLifecycleSnapshot("ready", false)}
	sess.turnSender = func(_ context.Context, prompt string) (json.RawMessage, error) {
		prompts <- prompt
		return json.RawMessage(`{"turn":{"id":"provider-turn"}}`), nil
	}
	s := &server{agentExecutions: store, controlMetadata: control, catalog: renamedCatalog{name: "Worker sender"}, sessions: map[string]*session{sess.id: sess}}
	const prompt = "  Exact caller prompt: do not alter it.  "
	rec := httptest.NewRecorder()
	s.handleAgentTurnSubmit(rec, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/worker/turns", bytes.NewBufferString(`{"clientActionId":"prompt-1","prompt":"  Exact caller prompt: do not alter it.  ","senderThreadRef":"worker"}`)), worker.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-prompts:
		if !strings.HasPrefix(got, prompt+"\n\n[from Worker sender · native-worker · submitted ") || !strings.HasSuffix(got, "Z]") {
			t.Fatalf("provider prompt = %q, want caller text plus one sender footer", got)
		}
		if strings.Contains(got, "Do not inject this") {
			t.Fatalf("assignment focus leaked into prompt: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("provider did not receive prompt")
	}
	var operation agentTurnView
	if err := json.Unmarshal(rec.Body.Bytes(), &operation); err != nil {
		t.Fatal(err)
	}
	if operation.Assignment == nil || operation.Assignment.AssignmentRole != "worker" {
		t.Fatalf("turn submission assignment = %#v", operation.Assignment)
	}
	waitTurnSendDone(t, sess, agentTransportActionID(worker.ID, "prompt-1"))
}

func TestOrchestrationAssignmentNormalizesAndValidatesTargets(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := store.beginSession("parent", "parent", "codex", presentationShared)
	target, _, _ := store.beginSession("target-alias", "target", "codex", presentationShared)
	op, _, _ := store.beginTurn(target.ID, "result", "hash")
	s := &server{agentExecutions: store, controlMetadata: control, sessions: map[string]*session{}}
	if view := s.agentSessionView(parent.ID); view.Assignment != nil {
		t.Fatalf("unassigned thread defaulted to %#v", view.Assignment)
	}
	byAlias, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "  Audit it  ", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: "target-alias", OperationID: op.ID}}})
	if err != nil || byAlias.Focus != "Audit it" || byAlias.AuditTargets[0].HostedThreadID != target.ID {
		t.Fatalf("alias normalization = %#v, %v", byAlias, err)
	}
	byID, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "Audit it", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: target.ID, OperationID: op.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	firstHash, _ := json.Marshal(spawnRequest{Provider: "claude", ClientSessionID: "retry", InvokerThreadRef: parent.ID, Assignment: byAlias})
	secondHash, _ := json.Marshal(spawnRequest{Provider: "claude", ClientSessionID: "retry", InvokerThreadRef: parent.ID, Assignment: byID})
	if agentHash(firstHash) != agentHash(secondHash) {
		t.Fatalf("normalized target aliases changed creation hash: %s != %s", firstHash, secondHash)
	}
	related, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "worker", Focus: "Implement it", RelatedHostedThreadID: "target-alias"})
	if err != nil || related.RelatedHostedThreadID != target.ID {
		t.Fatalf("related thread normalization = %#v, %v", related, err)
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "worker", Focus: "x", RelatedHostedThreadID: "not-a-thread"}); err == nil {
		t.Fatal("unknown related thread accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "worker", Focus: "x", AuditTargets: byAlias.AuditTargets}); err == nil {
		t.Fatal("worker audit target accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "worker", Focus: string(bytes.Repeat([]byte("x"), 501))}); err == nil {
		t.Fatal("501-byte focus accepted")
	}
	exact, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "worker", Focus: string(bytes.Repeat([]byte("é"), 250))})
	if err != nil || len([]byte(exact.Focus)) != 500 {
		t.Fatalf("500-byte focus = %#v, err=%v", exact, err)
	}
	normalizedUnresolved, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: target.ID, Unresolved: true}}})
	if err != nil || normalizedUnresolved.AuditTargets[0].Unresolved {
		t.Fatalf("request unresolved leaked into canonical value: %#v, err=%v", normalizedUnresolved, err)
	}
	tooMany := make([]orchestrationAuditTarget, 17)
	for i := range tooMany {
		tooMany[i] = orchestrationAuditTarget{HostedThreadID: target.ID, OperationID: "op-" + string(rune('a'+i))}
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: tooMany}); err == nil {
		t.Fatal("17 targets accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: parent.ID, OperationID: op.ID}}}); err == nil {
		t.Fatal("cross-thread operation accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: "not-a-thread"}}}); err == nil {
		t.Fatal("unknown target accepted")
	}
	if _, err := s.normalizeOrchestrationAssignment(&orchestrationAssignment{AssignmentRole: "auditor", Focus: "x", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: target.ID}, {HostedThreadID: "target-alias"}}}); err == nil {
		t.Fatal("normalized duplicate accepted")
	}
	if _, err := control.appendOrchestrationAssignment(parent.ID, "record-target", &orchestrationAssignment{AssignmentRole: "auditor", Focus: "Review target", AuditTargets: []orchestrationAuditTarget{{HostedThreadID: target.ID}}}); err != nil {
		t.Fatal(err)
	}
	if err := control.destroy(target.ID); err != nil {
		t.Fatal(err)
	}
	historical, err := control.currentOrchestrationAssignment(parent.ID)
	if err != nil || historical == nil || len(historical.AuditTargets) != 1 || !historical.AuditTargets[0].Unresolved {
		t.Fatalf("deleted target projection = %#v, %v", historical, err)
	}
}

func TestOrchestrationAssignmentRelationshipReparentsWithoutChangingInvocationLineage(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run")
	if err != nil {
		t.Fatal(err)
	}
	originalParent, _, _ := store.beginSession("original-parent", "parent", "codex", presentationShared)
	newParent, _, _ := store.beginSession("new-parent", "new-parent", "claude", presentationShared)
	child, _, err := store.beginSessionWithInvoker("child", "child", "codex", presentationShared, originalParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{agentExecutions: store, controlMetadata: control, sessions: map[string]*session{}}

	update := httptest.NewRecorder()
	body := `{"clientActionId":"move-child","assignment":{"assignmentRole":"worker","focus":"Implement the change","relatedHostedThreadId":"new-parent","auditTargets":[]}}`
	applicationHandler(s).ServeHTTP(update, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/child/assignment", bytes.NewBufferString(body)))
	if update.Code != http.StatusOK {
		t.Fatalf("relationship update = %d %s", update.Code, update.Body.String())
	}
	current, err := control.currentOrchestrationAssignment(child.ID)
	if err != nil || current == nil || current.RelatedHostedThreadID != newParent.ID {
		t.Fatalf("current relationship = %#v, %v", current, err)
	}
	unchanged, _ := store.session(child.ID)
	if unchanged.InvokerHostedThreadID != originalParent.ID {
		t.Fatalf("invocation lineage changed = %#v", unchanged)
	}

	self := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(self, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/child/assignment", bytes.NewBufferString(`{"clientActionId":"self","assignment":{"assignmentRole":"worker","focus":"x","relatedHostedThreadId":"child","auditTargets":[]}}`)))
	if self.Code != http.StatusBadRequest || !bytes.Contains(self.Body.Bytes(), []byte("same thread")) {
		t.Fatalf("self relationship = %d %s", self.Code, self.Body.String())
	}

	cycle := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(cycle, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/new-parent/assignment", bytes.NewBufferString(`{"clientActionId":"cycle","assignment":{"assignmentRole":"auditor","focus":"review","relatedHostedThreadId":"child","auditTargets":[{"hostedThreadId":"child"}]}}`)))
	if cycle.Code != http.StatusBadRequest || !bytes.Contains(cycle.Body.Bytes(), []byte("cycle")) {
		t.Fatalf("cycle relationship = %d %s", cycle.Code, cycle.Body.String())
	}

	moveBack := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(moveBack, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/child/assignment", bytes.NewBufferString(`{"clientActionId":"move-back","assignment":{"assignmentRole":"worker","focus":"back","relatedHostedThreadId":"original-parent","auditTargets":[]}}`)))
	if moveBack.Code != http.StatusOK {
		t.Fatalf("move back = %d %s", moveBack.Code, moveBack.Body.String())
	}
	parentUnderChild := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(parentUnderChild, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/new-parent/assignment", bytes.NewBufferString(`{"clientActionId":"parent-under-child","assignment":{"assignmentRole":"auditor","focus":"review","relatedHostedThreadId":"child","auditTargets":[{"hostedThreadId":"child"}]}}`)))
	if parentUnderChild.Code != http.StatusOK {
		t.Fatalf("parent under child = %d %s", parentUnderChild.Code, parentUnderChild.Body.String())
	}
	// Retrying old evidence does not apply it again. Although A→B would now be
	// a cycle, the original action remains a successful idempotent retry.
	retry := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(retry, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/child/assignment", bytes.NewBufferString(body)))
	if retry.Code != http.StatusOK {
		t.Fatalf("historical relationship retry = %d %s", retry.Code, retry.Body.String())
	}
	current, err = control.currentOrchestrationAssignment(child.ID)
	if err != nil || current.RelatedHostedThreadID != originalParent.ID {
		t.Fatalf("historical retry changed current relationship = %#v, %v", current, err)
	}
}

func TestAssignmentResolutionFailuresAreVisible(t *testing.T) {
	control := newTestControlMetadata(t)
	store, err := newControlAgentExecutionStore(control, "run")
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := store.beginSession("parent", "parent", "codex", presentationShared)
	worker, _, err := store.beginSessionWithInvoker("worker", "worker", "codex", presentationShared, parent, &orchestrationAssignment{AssignmentRole: "worker", Focus: "x"})
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.beginTurn(worker.ID, "one", "hash")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{agentExecutions: store, controlMetadata: control, sessions: map[string]*session{}}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	if assignment, err := control.currentOrchestrationAssignment(worker.ID); err == nil || assignment != nil {
		t.Fatalf("closed DB current assignment = %#v, %v", assignment, err)
	}
	if view := s.agentSessionView(worker.ID); !view.AssignmentUnavailable {
		t.Fatalf("thread assignment failure hidden: %#v", view)
	}
	if view := s.agentTurnView(op); !view.AssignmentUnavailable {
		t.Fatalf("operation assignment failure hidden: %#v", view)
	}
	failed := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(failed, httptest.NewRequest(http.MethodPut, "/api/v1/hosted-threads/worker/assignment", bytes.NewBufferString(`{"clientActionId":"broken-store","assignment":{"assignmentRole":"worker","focus":"x","auditTargets":[]}}`)))
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("store failure status = %d %s", failed.Code, failed.Body.String())
	}
}
