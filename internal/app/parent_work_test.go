package app

import (
	"testing"
	"time"
)

func TestChildLaunchDoesNotBecomeParentWork(t *testing.T) {
	projection := newLiveProjection("claude")
	runtime := newProviderRuntimeFixture(t, sessionConfig{ID: "parent", HostedThreadID: "hosted-parent", Provider: "claude", Projection: projection}, "parent", "")
	sess := runtime.session
	at := time.Now()
	for _, line := range []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"background-tool","name":"Agent","input":{}}]}}`,
		`{"type":"user","message":{"content":[{"tool_use_id":"background-tool","type":"tool_result","content":"Async agent launched successfully. agentId: child-1"}]}}`,
	} {
		runtime.emitAtAndWait(t, []byte(line), at, "")
	}
	// The provider's own turn ends after the asynchronous launch. The child
	// remains running, but must not keep the parent's runtime working.
	runtime.emitAtAndWait(t, []byte(`{"type":"result","subtype":"success","is_error":false}`), at, "")
	waitReconnectCondition(t, func() bool { return !sess.Summary().TurnActive })
	app := testServerWithSessions(&server{Registry: newSessionRegistry()}, map[string]*session{"parent": sess})
	summary := sess.Summary()
	if summary.TurnActive || app.hostedViews().SessionView("hosted-parent").Working {
		t.Fatal("child launch became parent's own work")
	}
}
