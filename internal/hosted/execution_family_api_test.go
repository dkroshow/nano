package hosted

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecutionFamilyViewUsesDurableInvocationLineage(t *testing.T) {
	store := newTestAgentExecutionStore(t, "family")
	root, _, err := store.beginSession("root", "root-hash", "codex", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := store.beginSessionWithInvoker("child", "child-hash", "codex", presentationBackground, root)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := store.beginTurn(child.ID, "child-turn", "prompt")
	if err != nil {
		t.Fatal(err)
	}
	s := &ViewService{Executions: store}
	rec := httptest.NewRecorder()
	s.HandleFamily(rec, httptest.NewRequest(http.MethodGet, "/api/v1/hosted-threads/"+root.ID+"/family", nil), root)
	if rec.Code != http.StatusOK {
		t.Fatalf("family status=%d body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		RootHostedThreadID string                      `json:"rootHostedThreadId"`
		Members            []executionFamilyMemberView `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.RootHostedThreadID != root.ID || len(view.Members) != 2 {
		t.Fatalf("family view=%+v", view)
	}
	if view.Members[1].HostedThreadID != child.ID || len(view.Members[1].OperationIDs) != 1 || view.Members[1].OperationIDs[0] != op.ID {
		t.Fatalf("child view=%+v", view.Members[1])
	}
}
