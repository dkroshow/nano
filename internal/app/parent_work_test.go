package app

import (
	"testing"
	"time"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestChildLaunchDoesNotBecomeParentWork(t *testing.T) {
	sess := &session{provider: "claude", threadID: "parent", lifecycleState: testLifecycleSnapshot("ready", false), projection: newLiveProjection("claude")}
	at := time.Now()
	for _, line := range []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"background-tool","name":"Agent","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"background-tool","type":"tool_result","content":"Async agent launched successfully. agentId: child-1"}]}}`,
	} {
		sess.projection.applyWireLive(protocol.Recv, []byte(line), at, true, false)
	}
	sess.hostedThreadID = "hosted-parent"
	app := &server{sessions: map[string]*session{"parent": sess}}
	summary := sess.summary()
	if summary.TurnActive || app.bulkCloseMemberWorking(agentSessionRecord{ID: "hosted-parent"}) {
		t.Fatal("child launch became parent's own work")
	}
}
