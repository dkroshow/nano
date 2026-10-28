package app

import (
	"testing"
	"time"
)

// quietRuntimeExecutions keeps attachment setup out of tests of stored turn evidence.
// All turn-observer operations still use the real execution store.
type quietRuntimeExecutions struct{ *agentExecutionStore }

func (*quietRuntimeExecutions) BindSession(string, string, string, string, string) error { return nil }
func (*quietRuntimeExecutions) UpdateSession(string, string, string, string, string) error {
	return nil
}

func newCodexObserverFixture(t *testing.T, store *agentExecutionStore, hostedID, nativeID string) (*session, *fakeAppServer) {
	t.Helper()
	migrateTestControl(t, store.control)
	prompts := testPromptStore(t, store.control.db)
	queue, err := newQueuedTurnStore(store.control.db, "", prompts)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := newSendAttemptStore(store.control.db, "", "observer", prompts)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(sessionConfig{ID: "observer-attachment", Provider: "codex", HostedThreadID: hostedID, Projection: newLiveProjection("codex", hostedID), AgentExecutions: &quietRuntimeExecutions{store}, Turns: prompts, QueuedTurns: queue, SendAttempts: journal, CreatedAt: time.Now()})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID(nativeID)
	fake := newFakeAppServer(t)
	t.Cleanup(sess.Close)
	return sess, fake
}
func startCodexObserverFixture(t *testing.T, sess *session, fake *fakeAppServer) {
	t.Helper()
	if err := sess.BeginProviderSpawn(providerRequest{Provider: "codex", Mode: "resume", ThreadID: sess.ThreadID()}, fake.daemon(), func() {}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status := sess.Summary().Status; status == "ready" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("observer attachment did not become ready: %+v", sess.Summary())
}
