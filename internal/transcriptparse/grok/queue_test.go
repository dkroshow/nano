package grok

import (
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokEmptyQueueCannotBindLaterUserToAnOldPrompt(t *testing.T) {
	var state parsedView
	for _, row := range []string{
		`{"method":"_x.ai/queue/changed","params":{"sessionId":"s1","runningPromptId":"old","runningText":"ask"}}`,
		`{"method":"_x.ai/queue/changed","params":{"sessionId":"s1","runningPromptId":null,"entries":[]}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"ask"}},"_meta":{"eventId":"new-user"}}}`,
	} {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(state.Items) != 1 || len(state.Continuation.RunningPrompts) != 0 {
		t.Fatal("empty queue left a stale prompt binding")
	}
	for _, item := range state.Items {
		if item.Scope.NativeTurnID != nil {
			t.Error("later user inherited retired prompt")
		}
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown {
			t.Error("queue change invented conversation outcome")
		}
	}
}

func TestGrokOnlyNonReplayMatchingTerminalRetiresQueueBinding(t *testing.T) {
	for _, replay := range []bool{false, true} {
		state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"method":"_x.ai/queue/changed","params":{"sessionId":"s1","runningPromptId":"p1","runningText":"ask"}}`)))
		if err != nil {
			t.Fatal(err)
		}
		row := fmt.Sprintf(`{"method":"_x.ai/session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn"},"_meta":{"eventId":"terminal","isReplay":%t}}}`, replay)
		state, batch, err := parseWireView(state, inputBytes([]byte(row)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("terminal: %v %+v", err, batch.Diagnostics)
		}
		want := 0
		if replay {
			want = 1
		}
		if len(state.Continuation.RunningPrompts) != want {
			t.Errorf("replay=%t: wrong current binding", replay)
		}
	}
}

func TestGrokQueueAuthorityRequiresProviderDirectionAndNativeSession(t *testing.T) {
	for _, tc := range []struct{ params, direction string }{
		{`{"sessionId":"s1","runningPromptId":"p","runningText":"ask"}`, "send"},
		{`{"runningPromptId":"p","runningText":"ask"}`, "recv"},
	} {
		input := inputBytes([]byte(`{"method":"_x.ai/queue/changed","params":` + tc.params + `}`))
		input.Source.Direction = tc.direction
		caller := "caller-session"
		input.Scope.NativeThreadID = &caller
		state, batch, err := parseWireView(parsedView{}, input)
		if err != nil || len(state.Continuation.RunningPrompts) != 0 || len(batch.Diagnostics) == 0 {
			t.Fatalf("unproven queue: %v %+v", err, batch.Diagnostics)
		}
	}
}
