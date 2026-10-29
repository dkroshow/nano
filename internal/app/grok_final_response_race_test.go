package app

import (
	"testing"
)

// The session sibling checks that the queued outcome and timer invoke the
// terminal consumer once. This owner retains the first durable terminal when
// another source reports completion or a late failure.
func TestGrokRPCOutcomeTerminalAppliesExactlyOnce(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run")
	hosted, _, err := fixtureBeginSession(store, "grok-race", "hash", "grok", presentationShared)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSession(hosted.ID, "nano", "native-thread", "ready", presentationShared); err != nil {
		t.Fatal(err)
	}
	op, _, err := fixtureBeginTurn(store, hosted.ID, "action", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnDelivery(op.ID, "prompt-race", agentTurnRunning, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveCandidate("grok", "native-thread", "prompt-race", "prompt-race/agentMessage", "final_answer", "-R2-OK"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTurnTerminal("grok", "native-thread", "prompt-race", agentTurnCompleted, nil, "", ""); err != nil {
		t.Fatal(err)
	}
	first, _ := store.Turn(op.ID)
	if first.Status != agentTurnCompleted || first.FinalResponse != "-R2-OK" || first.FinalResponseTruncated {
		t.Fatalf("first terminal = %+v", first)
	}
	for _, status := range []string{agentTurnCompleted, agentTurnFailed} {
		if err := store.MarkTurnTerminal("grok", "native-thread", "prompt-race", status, nil, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := store.Turn(op.ID)
	if after.Revision != first.Revision || after.Status != agentTurnCompleted || after.FinalResponse != first.FinalResponse {
		t.Fatalf("terminal rewritten: revision %d->%d status %q finalResponse %q", first.Revision, after.Revision, after.Status, after.FinalResponse)
	}
}
