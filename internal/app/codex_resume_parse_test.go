package app

import (
	"encoding/json"
	"testing"
)

func TestCodexOperationalResponseUsesNativeTextInWireAndResume(t *testing.T) {
	for _, route := range []string{"wire", "resume"} {
		t.Run(route, func(t *testing.T) {
			store := newTestAgentExecutionStore(t, "run-1")
			hosted, _, _ := fixtureBeginSession(store, "caller", "request", "codex", presentationShared)
			_ = store.BindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared)
			op, _, _ := fixtureBeginTurn(store, hosted.ID, "action-1", "prompt")
			_ = store.MarkTurnDelivery(op.ID, "turn-1", agentTurnRunning, nil)
			sess, fake := newCodexObserverFixture(t, store, hosted.ID, "native-1")
			answer := json.RawMessage(`{"id":"answer","type":"agentMessage","phase":"final_answer","text":"  native answer\n","summary":["unrelated summary"]}`)
			if route == "wire" {
				startCodexObserverFixture(t, sess, fake)
				frame, _ := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "native-1", "turnId": "turn-1", "item": answer}})
				var payload map[string]any
				if err := json.Unmarshal(frame, &payload); err != nil {
					t.Fatal(err)
				}
				fake.EmitThreadNotification("native-1", payload)
				waitForClaude(t, "native answer candidate", func() bool { got, _ := store.Turn(op.ID); return got.CandidateItemID == "answer" })
				before, _ := store.Turn(op.ID)
				if before.Status != agentTurnRunning {
					t.Fatal("item completion settled the operation")
				}
				fake.EmitThreadNotification("native-1", map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "native-1", "turn": map[string]any{"id": "turn-1", "status": "completed"}}})
			} else {
				turns, _ := json.Marshal([]any{map[string]any{"id": "turn-1", "status": "completed", "items": []json.RawMessage{answer}}})
				fake.SetResumeTurns(turns)
				startCodexObserverFixture(t, sess, fake)
			}
			waitForClaude(t, "native answer terminal", func() bool { got, _ := store.Turn(op.ID); return got.Status == agentTurnCompleted })
			after, _ := store.Turn(op.ID)
			if after.Status != agentTurnCompleted || after.FinalItemID != "answer" || after.FinalResponse != "  native answer\n" {
				t.Fatalf("response changed at %s boundary: %#v", route, after)
			}
		})
	}
}
