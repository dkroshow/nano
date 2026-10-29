package app

import (
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/sessions"
)

func TestGrokOperationalModuleCannotPromoteUnknownReplayToResponse(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run")
	hosted, _, _ := fixtureBeginSession(store, "caller", "hash", "grok", presentationShared)
	_ = store.BindSession(hosted.ID, "nano", "native", "ready", presentationShared)
	fixture := newProviderRuntimeFixture(t, sessions.Config{ID: "nano", Provider: "grok", HostedThreadID: hosted.ID, AgentExecutions: store, Projection: newLiveProjection("grok")}, "native", "")
	op, _, _ := fixtureBeginTurn(store, hosted.ID, "action", "hash")
	_ = store.MarkTurnDelivery(op.ID, "prompt", agentTurnRunning, nil)
	for _, message := range []struct {
		text, event string
		replay      bool
	}{{"live answer", "live", false}, {"old answer", "missing-original", true}} {
		line := []byte(fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}},"_meta":{"promptId":"prompt","eventId":%q,"isReplay":%t}}}`, message.text, message.event, message.replay))
		fixture.emit(t, line)
	}
	fixture.emit(t, []byte(`{"method":"_x.ai/session/prompt_complete","params":{"sessionId":"native","promptId":"prompt","stopReason":"end_turn"}}`))
	waitForClaude(t, "Grok terminal after replay", func() bool { got, _ := store.Turn(op.ID); return got.Status == agentTurnCompleted })
	after, _ := store.Turn(op.ID)
	if after.Status != agentTurnCompleted || after.FinalResponse != "live answer" {
		t.Fatal("unknown replay replaced the current response candidate")
	}
}
