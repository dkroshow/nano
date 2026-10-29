package hosted

import (
	"errors"
	"testing"
	"time"

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
	prompts := make(chan string, 8)
	sess := &guardAdmission{store: store, prompts: prompts}
	s := testController(t, store)
	s.Runtime = guardRuntime{ControlRuntime: s.Runtime, session: sess}

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
	if inFlight := sess.sent; inFlight != 0 {
		t.Fatalf("guarded rejection created send attempts %#v", inFlight)
	}
	select {
	case prompt := <-prompts:
		t.Fatalf("guarded rejection reached provider: %q", prompt)
	default:
	}

	admitted, _, err := s.submitHostedTurnGuardedWithOptions(worker.ID, actionID, "continue owner work", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-prompts:
	case <-time.After(time.Second):
		t.Fatal("guarded admission did not reach provider")
	}
	// The bounded fake delivers synchronously, so this call has already completed.
	guardCalled := false
	replay, _, err := s.submitHostedTurnGuardedWithOptions(worker.ID, actionID, "continue owner work", true, func([]ownedthreads.TurnRecord) error {
		guardCalled = true
		return rejected
	})
	if err != nil || replay.ID != admitted.ID || guardCalled {
		t.Fatalf("guarded replay=%#v admitted=%#v guardCalled=%v err=%v", replay, admitted, guardCalled, err)
	}
	select {
	case duplicate := <-prompts:
		t.Fatalf("guarded replay reached provider twice: %q", duplicate)
	default:
	}
}
