package main

import (
	"encoding/json"
	"os"
	"testing"
)

const testTurnIDSession = "nht_turn_ids"

func TestShimEmitsTurnIDOnAcceptanceItemsAndResult(t *testing.T) {
	sh := newTestShim(t)
	client := sh.dial()
	var started startResult
	client.mustCall(methodStart, map[string]any{
		"bin": os.Args[0], "args": fakeChildArgs("native-turn-ids", "raw"),
		"cwd": sh.cwd, "turnIdSession": testTurnIDSession,
	}, &started)
	client.waitLines(1) // system.init is sequence 1

	var accepted map[string]any
	client.mustCall(methodInput, inputParams{
		ShimSessionID: started.ShimSessionID,
		Line:          `{"type":"user","message":{"role":"user","content":"work"}}`,
	}, &accepted)
	sh.session(started.ShimSessionID).record(dirRecv, `{"type":"assistant","message":{"role":"assistant","content":"working"}}`, true)
	sh.session(started.ShimSessionID).record(dirRecv, `{"type":"result","subtype":"success"}`, true)

	want := "claude:" + testTurnIDSession + ":2"
	if got, _ := accepted["turnId"].(string); got != want {
		t.Fatalf("acceptance turnId = %q, want %q", got, want)
	}
	lines := client.waitLines(3)
	for _, line := range lines[len(lines)-2:] {
		if got := jsonTurnID(t, line); got != want {
			t.Fatalf("%s frame turnId = %q, want %q", line.Line, got, want)
		}
	}
}

func TestLegacySpoolReplayDerivesStableTurnIDs(t *testing.T) {
	sp, err := newSpool(t.TempDir(), testTurnIDSession)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.close(true)
	legacy := []spoolEvent{
		{Seq: 1, Dir: dirRecv, Line: `{"type":"system","subtype":"init"}`},
		{Seq: 2, Dir: dirSend, Line: `{"type":"user","message":{"role":"user","content":"old"}}`},
		{Seq: 3, Dir: dirRecv, Line: `{"type":"assistant","message":{"role":"assistant","content":"answer"}}`},
		{Seq: 4, Dir: dirRecv, Line: `{"type":"result","subtype":"success"}`},
	}
	for _, evt := range legacy {
		if err := sp.append(evt); err != nil {
			t.Fatal(err)
		}
	}

	replayed, err := sp.readAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	want := "claude:" + testTurnIDSession + ":2"
	for _, evt := range replayed[1:] {
		if got := jsonTurnID(t, evt); got != want {
			t.Fatalf("replayed seq %d turnId = %q, want %q", evt.Seq, got, want)
		}
	}
}

func TestShimTurnIDSequenceSurvivesSpoolRestart(t *testing.T) {
	dir := t.TempDir()
	firstSpool, err := newSpool(dir, testTurnIDSession)
	if err != nil {
		t.Fatal(err)
	}
	first := &shimSession{turnIDSession: testTurnIDSession, state: stateExited, spool: firstSpool}
	first.record(dirSend, `{"type":"user","message":{"role":"user","content":"first"}}`, false)
	first.record(dirRecv, `{"type":"assistant","message":{"role":"assistant","content":"answer"}}`, false)
	first.record(dirRecv, `{"type":"result","subtype":"success"}`, false)
	before, err := firstSpool.readAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	first.close()

	restartedSpool, err := newSpool(dir, testTurnIDSession)
	if err != nil {
		t.Fatalf("reopening the same spool after shim restart: %v", err)
	}
	defer restartedSpool.close(true)
	after, err := restartedSpool.readAfter(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("restarted spool has %d events, want %d", len(after), len(before))
	}
	for i := range before {
		if got, want := jsonTurnID(t, after[i]), jsonTurnID(t, before[i]); got != want {
			t.Fatalf("restarted seq %d turnId = %q, want original %q", after[i].Seq, got, want)
		}
	}

	restarted := &shimSession{
		turnIDSession: testTurnIDSession, state: stateRunning, spool: restartedSpool,
		seq: restartedSpool.lastSeq,
	}
	restarted.record(dirSend, `{"type":"user","message":{"role":"user","content":"second"}}`, false)
	newEvents, err := restartedSpool.readAfter(after[len(after)-1].Seq)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonTurnID(t, newEvents[0]), "claude:"+testTurnIDSession+":4"; got != want {
		t.Fatalf("next prompt turnId = %q, want %q", got, want)
	}
}

func TestShimClientReattachReplaysTurnIDAndAdvances(t *testing.T) {
	sh := newTestShim(t)
	first := sh.dial()
	var started startResult
	first.mustCall(methodStart, map[string]any{
		"bin": os.Args[0], "args": fakeChildArgs("native-reattach", "raw"),
		"cwd": sh.cwd, "turnIdSession": testTurnIDSession,
	}, &started)
	first.waitLines(1)
	var accepted map[string]any
	first.mustCall(methodInput, inputParams{
		ShimSessionID: started.ShimSessionID,
		Line:          `{"type":"user","message":{"role":"user","content":"first"}}`,
	}, &accepted)
	sh.session(started.ShimSessionID).record(dirRecv, `{"type":"result","subtype":"success"}`, true)
	first.waitLines(2)
	first.close()
	sh.waitDetached(started.ShimSessionID)

	reattached := sh.dial()
	var attached attachResult
	reattached.mustCall(methodAttach, attachParams{ShimSessionID: started.ShimSessionID}, &attached)
	replayed := reattached.waitLines(3)
	firstID, _ := accepted["turnId"].(string)
	if got := jsonTurnID(t, replayed[1]); got != firstID {
		t.Fatalf("replayed prompt turnId = %q, want accepted id %q", got, firstID)
	}
	if got := jsonTurnID(t, replayed[2]); got != firstID {
		t.Fatalf("replayed result turnId = %q, want accepted id %q", got, firstID)
	}

	var next map[string]any
	reattached.mustCall(methodInput, inputParams{
		ShimSessionID: started.ShimSessionID,
		Line:          `{"type":"user","message":{"role":"user","content":"second"}}`,
	}, &next)
	if nextID, _ := next["turnId"].(string); nextID == "" || nextID == firstID {
		t.Fatalf("next prompt turnId = %q, want a non-empty id after %q", nextID, firstID)
	}
}

func jsonTurnID(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	turnID, _ := fields["turnId"].(string)
	return turnID
}
