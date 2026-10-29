package hosted

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func TestExecutionFamilyViewUsesDurableInvocationLineage(t *testing.T) {
	store := newTestAgentExecutionStore(t, "family")
	root, _, err := fixtureBeginSession(store, "root", "root-hash", "codex", ownedthreads.PresentationShared)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := fixtureBeginSessionWithInvoker(store, "child", "child-hash", "codex", ownedthreads.PresentationBackground, root)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := fixtureBeginTurn(store, child.ID, "child-turn", "prompt")
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
