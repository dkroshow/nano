package sessions

import (
	"encoding/json"
	"testing"
)

// The compact state/event copies still omit app-server history, but the
// normalized conversation projection must consume the bounded tail before
// that omission or a restart in the middle of a turn loses its prompt prefix.
func TestResumeHandshakeHydratesConversationProjection(t *testing.T) {
	fake := newFakeAppServer(t)
	const threadID = "0199a213-abcd-7000-8000-000000000007"
	fake.SetResumeTurns(json.RawMessage(`[
		{"id":"turn-1","status":"completed","startedAt":1785510000,"completedAt":1785510001,"items":[
			{"id":"user-1","type":"userMessage","content":[{"type":"text","text":"before restart"}]},
			{"id":"agent-1","type":"agentMessage","text":"completed answer"}
		]},
		{"id":"turn-2","status":"inProgress","startedAt":1785510100,"items":[
			{"id":"user-2","type":"userMessage","content":[{"type":"text","text":"survive restart"}]},
			{"id":"agent-2","type":"agentMessage","text":"partial answer"}
		]}
	]`))
	projection := newLiveProjection("codex")
	resumeCodexFixture(t, runtimeFixtureConfig(t, Config{ID: "resume-hydrate", Provider: "codex", HostedThreadID: "hydrate", Projection: projection}), threadID, fake)
	snapshot := projectionStateForTest(projection)
	if snapshot.NativeThreadID != threadID {
		t.Fatalf("projection thread = %q, want %q", snapshot.NativeThreadID, threadID)
	}
	if len(snapshot.Turns) != 2 || snapshot.Turns[1].Status != "in_progress" || !snapshot.Runtime.TurnActive {
		t.Fatalf("resumed projection turns/runtime = %#v / %#v", snapshot.Turns, snapshot.Runtime)
	}
}
