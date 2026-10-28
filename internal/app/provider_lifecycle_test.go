package app

import (
	"testing"
	"time"
)

func TestProviderLinkLossDoesNotCompleteProjectedTurn(t *testing.T) {
	fixture := newProviderRuntimeFixture(t, sessionConfig{ID: "link-loss", Provider: "codex"}, "native-link-loss", "model")
	sess := fixture.session
	fixture.emit(t, []byte(`{"method":"turn/started","params":{"threadId":"native-link-loss","turn":{"id":"accepted","status":"inProgress"}}}`))
	waitReconnectCondition(t, func() bool { return len(projectionStateForTest(fixture.projection).Turns) == 1 })
	fixture.codex.SetReplyDelay("thread/resume", time.Second)
	fixture.codex.DropConnections()
	waitReconnectCondition(t, func() bool { return sess.Summary().Status == "reconnecting" })
	snapshot := projectionStateForTest(fixture.projection)
	if snapshot.Runtime.TurnID != "" || snapshot.Runtime.TurnActive {
		t.Fatalf("disconnected runtime exposes work: %+v", snapshot.Runtime)
	}
	if len(snapshot.Turns) != 1 || snapshot.Turns[0].Status != "in_progress" {
		t.Fatalf("connection loss invented terminal evidence: %+v", snapshot.Turns)
	}
}
