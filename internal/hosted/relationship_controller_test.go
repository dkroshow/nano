package hosted

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChildCreationQueuedBehindTreeMutationCannotUseClosedInvoker(t *testing.T) {
	store := newTestAgentExecutionStore(t, "bulk-close-test")
	root, _, err := store.beginSessionWithInvoker("root", "root-hash", "codex", presentationShared, agentSessionRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(root.ID, "nano-root", "native-root", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	root, _ = store.Session(root.ID)
	s := testController(t, store)
	cwd := t.TempDir()
	s.PrepareCreation = func(req CreationRequest) (CreationPlan, error) {
		return CreationPlan{ClientSessionID: req.ClientSessionID, Provider: req.Provider, Mode: "start", PresentationMode: presentationShared, SidebarPlacement: sidebarPlacementInvoker,
			Attach: func(record agentSessionRecord, _ bool) (string, error) { return record.ID, nil },
		}, nil
	}

	s.relationshipMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		body := fmt.Sprintf(`{"provider":"codex","cwd":%q,"clientSessionId":"late-child","invokerThreadRef":%q}`, cwd, root.ID)
		s.HandleCreate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads", strings.NewReader(body)))
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
