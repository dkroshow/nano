package app

import (
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestChildLaunchDoesNotBecomeParentWork(t *testing.T) {
	projection := newLiveProjection("claude")
	sess := configuredFixtureSession(t, sessionConfig{ID: "parent", HostedThreadID: "hosted-parent", Provider: "claude", Projection: projection}, "parent", "ready", "")
	at := time.Now()
	for _, line := range []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"background-tool","name":"Agent","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"background-tool","type":"tool_result","content":"Async agent launched successfully. agentId: child-1"}]}}`,
	} {
		projection.ApplyWireLive(protocol.Recv, []byte(line), at, true, false)
	}
	app := testServerWithSessions(&server{Registry: newSessionRegistry()}, map[string]*session{"parent": sess})
	summary := sess.Summary()
	if summary.TurnActive || app.agentSessionView("hosted-parent").Working {
		t.Fatal("child launch became parent's own work")
	}
}
