package hosted

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/testsupport/codexserver"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

func TestGuardedHostedTurnAdmissionRejectsWithoutSideEffectsAndPreservesReplay(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run-1")
	worker, _, err := fixtureBeginSession(store, "worker", "worker", "codex", ownedthreads.PresentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(worker.ID, "nano-worker", "native-worker", "ready", ownedthreads.PresentationShared); err != nil {
		t.Fatal(err)
	}
	c := testController(t, store)
	turns, err := prompts.OpenStore(context.Background(), store.Control().CanonicalDatabase())
	if err != nil {
		t.Fatal(err)
	}
	provider := codexserver.NewServer(t)
	received := make(chan string, 8)
	provider.OnTurnStart = func(text string) { received <- text }
	provider.SetResumeResult(map[string]any{"thread": map[string]any{"id": "native-worker"}})
	provider.SetTurnStartResult(map[string]any{"turn": map[string]any{"id": "provider-turn"}})
	sess := sessions.New(sessions.Config{ID: "nano-worker", Provider: "codex", HostedThreadID: worker.ID, PresentationMode: ownedthreads.PresentationShared, Turns: turns, QueuedTurns: c.Queue, AgentExecutions: store, Projection: sessions.NewLiveProjection("codex", worker.ID, false)})
	if err := sess.InitializeBinding(1); err != nil {
		t.Fatal(err)
	}
	sess.ReserveNativeID("native-worker")
	daemon := sessions.NewCodexDaemon(provider.WSURL(), "", nil)
	if err := sess.Runtime().BeginProviderSpawn(sessions.ProviderRequest{Provider: "codex", Mode: "resume", ThreadID: "native-worker"}, daemon, func() {}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Teardown().Close)
	deadline := time.Now().Add(5 * time.Second)
	for sess.View().Summary().Status != "ready" {
		if time.Now().After(deadline) {
			t.Fatal("session did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
	c.Sessions.RegisterSession(sess)
	s := c

	rejected := errors.New("superseded by newer input")
	actionID := "nano:guarded-admission"
	if record, _, err := s.submitHostedTurnGuardedWithOptions(worker.ID, actionID, "continue owner work", true, func([]ownedthreads.TurnRecord) error {
		return rejected
	}); !errors.Is(err, rejected) || record.ID != "" {
		t.Fatalf("guarded rejection record=%#v err=%v", record, err)
	}
	if operation, found := store.TurnForAction(worker.ID, actionID); found {
		t.Fatalf("guarded rejection created operation %#v", operation)
	}
	if queued, found := s.Queue.First("codex", "native-worker", worker.ID); found {
		t.Fatalf("guarded rejection created queue row %#v", queued)
	}
	if inFlight := len(provider.StartedTexts()); inFlight != 0 {
		t.Fatalf("guarded rejection created send attempts %#v", inFlight)
	}
	select {
	case prompt := <-received:
		t.Fatalf("guarded rejection reached provider: %q", prompt)
	default:
	}

	admitted, _, err := s.submitHostedTurnGuardedWithOptions(worker.ID, actionID, "continue owner work", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("guarded admission did not reach provider")
	}
	// Wait for the real session to persist provider delivery before checking replay.
	for {
		rec, _ := store.Turn(admitted.ID)
		if rec.Status == ownedthreads.AgentTurnRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider delivery did not settle")
		}
		time.Sleep(time.Millisecond)
	}
	guardCalled := false
	replay, _, err := s.submitHostedTurnGuardedWithOptions(worker.ID, actionID, "continue owner work", true, func([]ownedthreads.TurnRecord) error {
		guardCalled = true
		return rejected
	})
	if err != nil || replay.ID != admitted.ID || guardCalled {
		t.Fatalf("guarded replay=%#v admitted=%#v guardCalled=%v err=%v", replay, admitted, guardCalled, err)
	}
	select {
	case duplicate := <-received:
		t.Fatalf("guarded replay reached provider twice: %q", duplicate)
	default:
	}
}
