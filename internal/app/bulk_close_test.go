package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBulkCloseTestServer(t *testing.T) (*server, *agentExecutionStore, *controlMetadataStore) {
	t.Helper()
	control := newTestControlMetadata(t)
	executions, err := newControlAgentExecutionStore(control, "bulk-close-test")
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		Registry:        newSessionRegistry(),
		agentExecutions: executions,
		controlMetadata: control,
		threadStars:     openTestThreadStars(t),
	}, executions, control
}

func createBulkCloseMember(t *testing.T, store *agentExecutionStore, clientID string, invoker agentSessionRecord) agentSessionRecord {
	t.Helper()
	record, _, err := store.beginSessionWithInvoker(clientID, clientID+"-hash", "codex", presentationShared, invoker)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(record.ID, "nano-"+clientID, "native-"+clientID, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	record, _ = store.session(record.ID)
	return record
}

func bulkCloseRequest(t *testing.T, s *server, rootID, policy string) (int, bulkCloseResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"starredPolicy":"` + policy + `"}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+rootID+"/family/close", body)
	applicationHandler(s).ServeHTTP(recorder, request)
	var response bulkCloseResponse
	if strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, response
}

func bulkClosePreviewRequest(t *testing.T, s *server, rootID string) (int, bulkClosePreview) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/"+rootID+"/family/close", nil)
	applicationHandler(s).ServeHTTP(recorder, request)
	var response bulkClosePreview
	if strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, response
}

func TestBulkCloseUsesCurrentRelationshipTreeNotImmutableInvokerLineage(t *testing.T) {
	s, store, control := newBulkCloseTestServer(t)
	rootA := createBulkCloseMember(t, store, "root-a", agentSessionRecord{})
	rootC := createBulkCloseMember(t, store, "root-c", agentSessionRecord{})
	staysWithA := createBulkCloseMember(t, store, "stays-a", rootA)
	movedToC := createBulkCloseMember(t, store, "moved-c", rootA)
	movedChild := createBulkCloseMember(t, store, "moved-child", movedToC)
	if _, err := control.appendOrchestrationAssignment(movedToC.ID, "move-to-c", &orchestrationAssignment{
		AssignmentRole: "worker", Focus: "Now belongs to C", RelatedHostedThreadID: rootC.ID,
	}); err != nil {
		t.Fatal(err)
	}
	previewStatus, preview := bulkClosePreviewRequest(t, s, rootA.ID)
	if previewStatus != http.StatusOK || preview.MemberCount != 2 || len(preview.Members) != 2 {
		t.Fatalf("bulk close preview A = %d %+v", previewStatus, preview)
	}
	if preview.Members[0].HostedThreadID != rootA.ID || preview.Members[0].Depth != 0 || preview.Members[1].HostedThreadID != staysWithA.ID || preview.Members[1].Depth != 1 {
		t.Fatalf("bulk close preview members = %+v", preview.Members)
	}

	status, response := bulkCloseRequest(t, s, rootA.ID, bulkClosePreserveStars)
	if status != http.StatusOK || !response.Complete || response.MemberCount != 2 {
		t.Fatalf("bulk close A = %d %+v", status, response)
	}
	for _, record := range []agentSessionRecord{rootA, staysWithA} {
		closed, _ := store.session(record.ID)
		if closed.Status != sessionStatusExited {
			t.Fatalf("member %s status = %q, want exited", record.ID, closed.Status)
		}
	}
	for _, record := range []agentSessionRecord{rootC, movedToC, movedChild} {
		open, _ := store.session(record.ID)
		if open.Status == sessionStatusExited {
			t.Fatalf("reassigned C-tree member %s was closed with A", record.ID)
		}
	}
}

func TestBulkCloseExcludesStandaloneInvocationButAssignmentOverrides(t *testing.T) {
	s, store, control := newBulkCloseTestServer(t)
	root := createBulkCloseMember(t, store, "root", agentSessionRecord{})
	standalone := createBulkCloseMember(t, store, "standalone", root)
	if _, _, _, err := store.setSidebarPlacement(standalone.ID, sidebarPlacementStandalone); err != nil {
		t.Fatal(err)
	}
	status, preview := bulkClosePreviewRequest(t, s, root.ID)
	if status != http.StatusOK || preview.MemberCount != 1 {
		t.Fatalf("standalone preview = %d %+v", status, preview)
	}
	if _, err := control.appendOrchestrationAssignment(standalone.ID, "assign", &orchestrationAssignment{
		AssignmentRole: "worker", Focus: "Operational child", RelatedHostedThreadID: root.ID,
	}); err != nil {
		t.Fatal(err)
	}
	status, preview = bulkClosePreviewRequest(t, s, root.ID)
	if status != http.StatusOK || preview.MemberCount != 2 || preview.Members[1].HostedThreadID != standalone.ID {
		t.Fatalf("assigned preview = %d %+v", status, preview)
	}
}

func TestBulkCloseKeepsResumedHostedAliasesInOneVisibleTree(t *testing.T) {
	s, store, _ := newBulkCloseTestServer(t)
	oldRoot := createBulkCloseMember(t, store, "old-root", agentSessionRecord{})
	child := createBulkCloseMember(t, store, "child", oldRoot)
	if err := store.UpdateSession(oldRoot.ID, "", "", sessionStatusExited, "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.control.db.Exec(`UPDATE hosted_thread_native_ids SET retired_at=? WHERE hosted_thread_id=? AND provider=? AND native_thread_id=? AND retired_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), oldRoot.ID, oldRoot.Provider, oldRoot.NativeThreadID); err != nil {
		t.Fatal(err)
	}
	newRoot := createBulkCloseMember(t, store, "new-root", agentSessionRecord{})
	if err := store.BindSession(newRoot.ID, "nano-new-root", oldRoot.NativeThreadID, "ready", presentationShared); err != nil {
		t.Fatal(err)
	}

	status, response := bulkCloseRequest(t, s, newRoot.ID, bulkClosePreserveStars)
	if status != http.StatusOK || !response.Complete || response.MemberCount != 2 {
		t.Fatalf("bulk close resumed root = %d %+v", status, response)
	}
	for _, record := range []agentSessionRecord{newRoot, child} {
		closed, _ := store.session(record.ID)
		if closed.Status != sessionStatusExited {
			t.Fatalf("visible-tree member %s status = %q, want exited", record.ID, closed.Status)
		}
	}
}

func TestBulkCloseRequiresAndAppliesExplicitStarPolicy(t *testing.T) {
	s, store, _ := newBulkCloseTestServer(t)
	root := createBulkCloseMember(t, store, "root", agentSessionRecord{})
	child := createBulkCloseMember(t, store, "child", root)
	if err := s.threadStars.setStarred(root.Provider, root.NativeThreadID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.threadStars.setStarred(child.Provider, child.NativeThreadID, true); err != nil {
		t.Fatal(err)
	}

	status, preserve := bulkCloseRequest(t, s, root.ID, bulkClosePreserveStars)
	if status != http.StatusOK || !preserve.Complete || preserve.StarredCount != 2 {
		t.Fatalf("preserve response = %d %+v", status, preserve)
	}
	if !s.threadStars.isStarred(root.Provider, root.NativeThreadID) || !s.threadStars.isStarred(child.Provider, child.NativeThreadID) {
		t.Fatal("preserve policy changed a star")
	}

	status, remove := bulkCloseRequest(t, s, root.ID, bulkCloseRemoveStars)
	if status != http.StatusOK || !remove.Complete {
		t.Fatalf("unstar response = %d %+v", status, remove)
	}
	if s.threadStars.isStarred(root.Provider, root.NativeThreadID) || s.threadStars.isStarred(child.Provider, child.NativeThreadID) {
		t.Fatal("unstar policy left a tree member pinned")
	}
	for _, member := range remove.Members {
		if member.CloseAction != "already_exited" || member.StarAction != "unstarred" {
			t.Fatalf("idempotent member result = %+v", member)
		}
	}

	bad := httptest.NewRecorder()
	applicationHandler(s).ServeHTTP(bad, httptest.NewRequest(http.MethodPost,
		"/api/v1/hosted-threads/"+root.ID+"/family/close", strings.NewReader(`{}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("missing star policy status = %d body=%s", bad.Code, bad.Body.String())
	}
}

func TestBulkCloseReportsPartialMemberFailure(t *testing.T) {
	s, store, _ := newBulkCloseTestServer(t)
	root := createBulkCloseMember(t, store, "root", agentSessionRecord{})
	queuePath := filepath.Join(t.TempDir(), "queue.json")
	queue, err := newTestQueuedTurnStore(t, queuePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := QueueEnqueue(queue, queuedTurnRecord{
		ClientActionID: "queued-root", Provider: root.Provider, HostedThreadID: root.ID,
		NativeThreadID: root.NativeThreadID, Text: "still queued", TextHash: "hash", State: submissionQueued,
	}); err != nil {
		t.Fatal(err)
	}
	// A synthetic database failure must roll back the prompt's refusal and
	// remain visible in the member result.
	if _, err := queue.Database().Exec(`CREATE TRIGGER reject_queue_removal BEFORE UPDATE OF state ON queued_turns BEGIN SELECT RAISE(ABORT,'synthetic queue failure'); END`); err != nil {
		t.Fatal(err)
	}
	s.queuedTurns = queue

	status, response := bulkCloseRequest(t, s, root.ID, bulkClosePreserveStars)
	if status != http.StatusMultiStatus || response.Complete || len(response.Members) != 1 {
		t.Fatalf("partial response = %d %+v", status, response)
	}
	if response.Members[0].CloseAction != "failed" || !strings.Contains(response.Members[0].Error, "drop queued prompts") {
		t.Fatalf("partial member evidence = %+v", response.Members[0])
	}
}

func TestChildCreationQueuedBehindTreeMutationCannotUseClosedInvoker(t *testing.T) {
	s, store, _ := newBulkCloseTestServer(t)
	root := createBulkCloseMember(t, store, "root", agentSessionRecord{})
	s.defaults.cwd = t.TempDir()

	s.relationshipMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		body := fmt.Sprintf(`{"provider":"codex","cwd":%q,"clientSessionId":"late-child","invokerThreadRef":%q}`, s.defaults.cwd, root.ID)
		s.handleAgentSessionCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads", strings.NewReader(body)))
		done <- recorder
	}()
	select {
	case recorder := <-done:
		t.Fatalf("child creation escaped relationship lock: %d %s", recorder.Code, recorder.Body.String())
	case <-time.After(25 * time.Millisecond):
	}
	if err := store.UpdateSession(root.ID, "", "", sessionStatusExited, "closed by bulk close"); err != nil {
		t.Fatal(err)
	}
	s.relationshipMu.Unlock()
	select {
	case recorder := <-done:
		if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "closed hosted thread") {
			t.Fatalf("late child response = %d %s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("child creation did not resume after tree mutation")
	}
}
