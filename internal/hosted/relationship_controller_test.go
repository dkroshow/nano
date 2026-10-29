package hosted

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func TestChildCreationQueuedBehindTreeMutationCannotUseClosedInvoker(t *testing.T) {
	store := newTestAgentExecutionStore(t, "bulk-close-test")
	root, _, err := fixtureBeginSessionWithInvoker(store, "root", "root-hash", "codex", ownedthreads.PresentationShared, ownedthreads.SessionRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(root.ID, "nano-root", "native-root", "ready", ownedthreads.PresentationShared); err != nil {
		t.Fatal(err)
	}
	root, _ = store.Session(root.ID)
	s := testController(t, store)
	cwd := t.TempDir()
	// Use the real preparation path; admission must still wait on the tree lock.

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
