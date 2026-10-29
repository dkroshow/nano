package sessions

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	liveview "github.com/dkroshow/nano/internal/liveview"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestDurableTerminalAuthority(t *testing.T) {
	completed := liveview.DurableTurnEvidence{Turn: liveview.Turn{ID: "turn-1", Status: "completed", CompletedAtMs: 200}}
	cases := []struct {
		name   string
		exact  bool
		newer  bool
		wireAt int64
		want   bool
	}{
		{"newer durable turn proves finality", true, true, 300, true},
		{"exact completed evidence with no newer wire", true, false, 200, true},
		{"newer live wire keeps reopenable completion active", true, false, 201, false},
		{"missing completion timestamp fails safe", true, false, 0, false},
		{"exact id mismatch fails safe", false, true, 0, false},
	}
	for _, tc := range cases {
		exact := completed
		exact.Newer = tc.newer
		if tc.name == "missing completion timestamp fails safe" {
			exact.Turn.CompletedAtMs = 0
		}
		if got := liveview.DurableTerminalAuthoritative(exact, tc.exact, tc.wireAt); got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The pane marks a turn's final answer, and shows its duration, from the
// published turn's finalItemID and durationMs. That id must name the turn's
// final answer, not commentary after it. Boundary: the projection's turn
// after a completed Codex turn.
func TestCompletedTurnNamesItsFinalAnswerNotItsCommentary(t *testing.T) {
	p := newLiveProjection("codex", "hosted-final")
	at := time.Now()
	for _, line := range []string{
		`{"method":"item/completed","params":{"threadId":"native-final","turnId":"turn-1","item":{"id":"commentary-1","type":"agentMessage","phase":"commentary","text":"working"}}}`,
		`{"method":"item/completed","params":{"threadId":"native-final","turnId":"turn-1","item":{"id":"final-1","type":"agentMessage","phase":"final_answer","text":"answer"}}}`,
		`{"method":"item/completed","params":{"threadId":"native-final","turnId":"turn-1","item":{"id":"commentary-2","type":"agentMessage","phase":"commentary","text":"one more note"}}}`,
	} {
		p.applyWire(protocol.Recv, []byte(line), at)
	}
	p.updateCompletedTurn("turn-1", "completed", 1500, at.UnixMilli())
	if turns := p.turnList(); len(turns) != 1 || turns[0].DurationMS != 1500 || turns[0].FinalItemID != "final-1" {
		t.Fatalf("completed turns = %#v, want final-1", turns)
	}
}

func TestCodexProjectionSelectsFinalAnswerWithinTerminalTurn(t *testing.T) {
	p := newLiveProjection("codex")
	at := time.Now()
	for _, line := range []string{
		`{"method":"item/completed","params":{"turnId":"turn-1","item":{"id":"commentary-1","type":"agentMessage","phase":"commentary","text":"working"}}}`,
		`{"method":"item/completed","params":{"turnId":"turn-1","item":{"id":"final-1","type":"agentMessage","phase":"final_answer","text":"answer"}}}`,
		`{"method":"item/completed","params":{"turnId":"turn-1","item":{"id":"late-commentary","type":"agentMessage","phase":"commentary","text":"late"}}}`,
	} {
		p.applyWire(protocol.Recv, []byte(line), at)
	}
	p.updateCompletedTurn("turn-1", "completed", 10, at.UnixMilli())
	p.applyWire(protocol.Recv, []byte(`{"method":"item/completed","params":{"turnId":"turn-2","item":{"id":"failed-partial","type":"agentMessage","text":"partial"}}}`), at)
	p.updateCompletedTurn("turn-2", "failed", 5, at.UnixMilli(), json.RawMessage(`{"message":"boom"}`))

	turns := p.turnList()
	if len(turns) != 2 {
		t.Fatalf("turns = %#v", turns)
	}
	if turns[0].FinalItemID != "final-1" {
		t.Fatalf("completed final item = %q", turns[0].FinalItemID)
	}
	if turns[1].FinalItemID != "" || string(turns[1].Error) != `{"message":"boom"}` {
		t.Fatalf("failed turn leaked response or lost error: %#v", turns[1])
	}
}

func TestCodexResumeSeedsFiveCompleteTurnsAndActiveTurn(t *testing.T) {
	p := newLiveProjection("codex")
	at := time.Unix(1785511000, 0)
	// A notification can beat the thread/resume reply. Its newer text must win
	// when the historical baseline is inserted ahead of it.
	p.applyWire(protocol.Recv, []byte(`{"method":"item/started","params":{"turnId":"turn-8","item":{"id":"agent-8","type":"agentMessage","text":"live partial answer"}}}`), at)

	turns := make([]map[string]any, 0, 8)
	for ordinal := 1; ordinal <= 7; ordinal++ {
		turns = append(turns, map[string]any{
			"id": fmt.Sprintf("turn-%d", ordinal), "status": "completed",
			"startedAt": 1785510000 + ordinal, "completedAt": 1785510001 + ordinal,
			"items": []map[string]any{
				{"id": fmt.Sprintf("user-%d", ordinal), "type": "userMessage", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("prompt %d", ordinal)}}},
				{"id": fmt.Sprintf("agent-%d", ordinal), "type": "agentMessage", "text": fmt.Sprintf("answer %d", ordinal)},
			},
		})
	}
	turns = append(turns, map[string]any{
		"id": "turn-8", "status": "inProgress", "startedAt": 1785511000,
		"items": []map[string]any{
			{"id": "user-8", "type": "userMessage", "content": []map[string]any{{"type": "text", "text": "prompt 8"}}},
			{"id": "agent-8", "type": "agentMessage", "text": "stale partial"},
		},
	})
	response, err := json.Marshal(map[string]any{"thread": map[string]any{"id": "native-1", "turns": turns}})
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := json.Unmarshal(response, &reply); err != nil {
		t.Fatal(err)
	}
	newProviderRuntimeFixture(t, Config{ID: "seed-five", Provider: "codex", Projection: p}, "native-1", "", func(f *providerRuntimeFixture) { f.resumeResult = reply })
	turnsAfter := p.turnList()

	if got, want := len(turnsAfter), 6; got != want {
		t.Fatalf("turns = %d, want %d: %#v", got, want, turnsAfter)
	}
	if turnsAfter[0].ID != "turn-3" || turnsAfter[5].ID != "turn-8" || turnsAfter[5].Status != "in_progress" {
		t.Fatalf("retained turns = %#v, want turn-3 through an open turn-8", turnsAfter)
	}
	if turnsAfter[0].FinalItemID != "agent-3" {
		t.Fatalf("resumed final answer = %q, want agent-3", turnsAfter[0].FinalItemID)
	}
}

func TestLiveProjectionPrunesOnlyWholeCompletedTurns(t *testing.T) {
	p := newLiveProjection("codex")
	at := time.Now()
	for ordinal := 1; ordinal <= 8; ordinal++ {
		turnID := fmt.Sprintf("turn-%d", ordinal)
		line := fmt.Sprintf(`{"method":"item/completed","params":{"turnId":%q,"item":{"id":%q,"type":"agentMessage","text":"done"}}}`, turnID, "msg-"+turnID)
		p.applyWire(protocol.Recv, []byte(line), at)
		p.updateCompletedTurn(turnID, "completed", 1, at.UnixMilli())
	}
	turns := p.turnList()
	if got, want := len(turns), 5; got != want {
		t.Fatalf("turns = %d, want %d", got, want)
	}
	if turns[0].ID != "turn-4" {
		t.Fatalf("oldest retained turn = %q, want turn-4", turns[0].ID)
	}
}

func TestClaudeProjectionUsesShimTurnIDs(t *testing.T) {
	p := newLiveProjection("claude")
	at := time.Now()
	for ordinal := 1; ordinal <= 2; ordinal++ {
		p.setClaudeProviderTurnID(fmt.Sprintf("shim-turn-%d", ordinal))
		prompt := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"prompt %d"}}`, ordinal)
		p.applyWire(protocol.Send, []byte(prompt), at)
		p.applyWire(protocol.Recv, []byte(`{"type":"result","subtype":"success"}`), at.Add(time.Second))
	}
	turns := p.turnList()
	if len(turns) != 2 || turns[0].ID != "shim-turn-1" || turns[1].ID != "shim-turn-2" {
		t.Fatalf("turns = %#v", turns)
	}
}

func applyClaudeFinalMessageForTest(p *LiveProjection, at time.Time, live, replay bool) {
	p.applyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"final-1"}}}`), at, live, replay)
	p.applyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text"}}}`), at, live, replay)
	p.applyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"text":"done"}}}`), at, live, replay)
	p.applyWireLive(protocol.Recv, []byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`), at, live, replay)
}

func TestClaudeLiveProjectionKeepsCompletedTurnIdentity(t *testing.T) {
	p := newLiveProjection("claude")
	at := time.Date(2026, time.August, 27, 5, 44, 0, 0, time.UTC)
	p.setClaudeProviderTurnID("shim-turn-1")

	p.applyWireLive(protocol.Send, []byte(`{"type":"user","message":{"role":"user","content":"inspect"}}`), at.Add(-time.Second), true, false)
	applyClaudeFinalMessageForTest(p, at.Add(-time.Millisecond), true, false)
	result := []byte(`{"type":"result","subtype":"success"}`)
	p.setClaudeResultTurnID("shim-turn-1") // the turn the session loop finds the result names
	p.applyWireLive(protocol.Recv, result, at, true, false)
	p.applyWireLive(protocol.Recv, result, at.Add(time.Second), true, false)

	turns := projectionStateForTest(p).Turns
	if len(turns) != 1 || turns[0].ID != "shim-turn-1" || turns[0].FinalItemID == "" {
		t.Fatalf("completed Claude turn = %#v", turns)
	}
}

func TestClaudeProjectionUsesResultErrorAndAbortEvidence(t *testing.T) {
	p := newLiveProjection("claude")
	at := time.Now()
	p.setClaudeProviderTurnID("shim-turn-1")
	p.applyWire(protocol.Send, []byte(`{"type":"user","message":{"role":"user","content":"first"}}`), at)
	p.setClaudeResultTurnID("shim-turn-1")
	p.applyWire(protocol.Recv, []byte(`{"type":"result","is_error":true,"subtype":"error_during_execution","result":"failed","terminal_reason":"error"}`), at.Add(time.Second))
	p.setClaudeProviderTurnID("shim-turn-2")
	p.applyWire(protocol.Send, []byte(`{"type":"user","message":{"role":"user","content":"second"}}`), at.Add(2*time.Second))
	p.setClaudeResultTurnID("shim-turn-2")
	p.applyWire(protocol.Recv, []byte(`{"type":"result","is_error":true,"terminal_reason":"aborted_streaming"}`), at.Add(3*time.Second))

	turns := projectionStateForTest(p).Turns
	if len(turns) != 2 || turns[0].Status != agentTurnFailed || len(turns[0].Error) == 0 {
		t.Fatalf("failed Claude turn = %#v", turns)
	}
	if turns[1].Status != agentTurnInterrupted || turns[1].FinalItemID != "" {
		t.Fatalf("interrupted Claude turn = %#v", turns[1])
	}
}

func TestClaudeFable51CompletedTurnKeepsStandaloneNextIdentity(t *testing.T) {
	p := newLiveProjection("claude")
	p.setClaudeProviderTurnID("claude:shim-fable:1")
	p.applyWire(protocol.Send, []byte(`{"type":"user","uuid":"user-1","message":{"role":"user","content":[{"type":"text","text":"Inspect"}]}}`), time.Now())
	for _, line := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_body"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Body"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_next"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Next: owner — continue"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	} {
		if strings.Contains(line, `"type":"result"`) {
			p.setClaudeResultTurnID("claude:shim-fable:1")
		}
		p.applyWire(protocol.Recv, []byte(line), time.Now())
	}
	turns := p.turnList()
	if len(turns) != 1 || turns[0].ID != "claude:shim-fable:1" || turns[0].FinalItemID != "msg_next:0" {
		t.Fatalf("completed split Fable turn = %#v", turns)
	}
}

// Failure caught: a stream that falls behind silently loses an update, which
// may be the terminal turn update no later one exposes. Boundary: the
// projection's subscription, flooded past its queue.
func TestLiveProjectionClosesALaggingSubscriber(t *testing.T) {
	p := newLiveProjection("codex")
	ch, cancel := p.subscribe()
	defer cancel()
	p.mu.Lock()
	for i := 0; i < cap(ch)+10; i++ {
		p.appendLocked(liveview.Update{Op: projectionTurnUpsert,
			Turn: &liveview.Turn{ID: "turn", Status: "in_progress"}})
	}
	p.mu.Unlock()
	closed := false
drain:
	for i := 0; i <= cap(ch); i++ {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
				break drain
			}
		case <-time.After(50 * time.Millisecond):
			break drain
		}
	}
	if !closed {
		t.Fatal("lagging projection subscriber remained open after its queue overflowed")
	}
	p.mu.Lock()
	subscriberCount := len(p.subs)
	p.mu.Unlock()
	if subscriberCount != 0 {
		t.Fatalf("lagging projection subscriber remained registered: %d", subscriberCount)
	}
}
