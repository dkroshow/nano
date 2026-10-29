package app

import (
	"testing"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

// quietRuntimeExecutions keeps attachment setup out of tests of stored turn evidence.
// All turn-observer operations still use the real execution store.
type quietRuntimeExecutions struct{ *hostedstore.ExecutionStore }

func (*quietRuntimeExecutions) BindSession(string, string, string, string, string) error { return nil }
func (*quietRuntimeExecutions) UpdateSession(string, string, string, string, string) error {
	return nil
}

func newCodexObserverFixture(t *testing.T, store *hostedstore.ExecutionStore, hostedID, nativeID string) (*sessions.Session, *fakeAppServer) {
	t.Helper()
	migrateTestControl(t, store.Control())
	prompts := testPromptStore(t, store.Control().CanonicalDatabase())
	queue, err := newQueuedTurnStore(store.Control().CanonicalDatabase(), "", prompts)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newSendAttemptStore(store.Control().CanonicalDatabase(), "", "observer", prompts)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(sessions.Config{ID: "observer-attachment", Provider: "codex", HostedThreadID: hostedID, Projection: newLiveProjection("codex", hostedID), AgentExecutions: &quietRuntimeExecutions{store}, Turns: prompts, QueuedTurns: queue, SendAttempts: journal, CreatedAt: time.Now()})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(nativeID)
	fake := newFakeAppServer(t)
	t.Cleanup(sess.Teardown().Close)
	return sess, fake
}
func startCodexObserverFixture(t *testing.T, sess *sessions.Session, fake *fakeAppServer) {
	t.Helper()
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: sess.State().ThreadID()}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status := sess.View().Summary().Status; status == "ready" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("observer attachment did not become ready: %+v", sess.View().Summary())
}
