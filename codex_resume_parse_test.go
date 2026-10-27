package main

import (
	"encoding/json"
	"testing"
)

func TestCodexOperationalResponseUsesNativeTextInWireAndResume(t *testing.T) {
	for _, route := range []string{"wire", "resume"} {
		t.Run(route, func(t *testing.T) {
			store := newTestAgentExecutionStore(t, "run-1")
			hosted, _, _ := store.beginSession("caller", "request", "codex", presentationShared)
			_ = store.bindSession(hosted.ID, "nano-1", "native-1", "ready", presentationShared)
			op, _, _ := store.beginTurn(hosted.ID, "action-1", "prompt")
			_ = store.markTurnDelivery(op.ID, "turn-1", agentTurnRunning, nil)
			sess := &session{provider: "codex", hostedThreadID: hosted.ID, agentExecutions: store}
			answer := json.RawMessage(`{"id":"answer","type":"agentMessage","phase":"final_answer","text":"  native answer\n","summary":["unrelated summary"]}`)
			if route == "wire" {
				frame, _ := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "native-1", "turnId": "turn-1", "item": answer}})
				sess.observeAgentExecutionWire("recv", frame)
				before, _ := store.turn(op.ID)
				if before.Status != agentTurnRunning {
					t.Fatal("item completion settled the operation")
				}
				sess.observeAgentExecutionWire("recv", []byte(`{"method":"turn/completed","params":{"threadId":"native-1","turn":{"id":"turn-1","status":"completed"}}}`))
			} else {
				response, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "native-1", "turns": []any{map[string]any{"id": "turn-1", "status": "completed", "items": []json.RawMessage{answer}}}}})
				sess.observeCodexResume(decodedCodexResume(response))
			}
			after, _ := store.turn(op.ID)
			if after.Status != agentTurnCompleted || after.FinalItemID != "answer" || after.FinalResponse != "  native answer\n" {
				t.Fatalf("response changed at %s boundary: %#v", route, after)
			}
		})
	}
}

func TestCodexResumeKeepsSnapshotCompletionAndClocksSeparate(t *testing.T) {
	response := json.RawMessage(`{"thread":{"id":"thread","turns":[
		{"id":"turn","status":"completed","startedAt":200,"items":[
			{"id":"answer","type":"agentMessage","text":"answer"}
		]}
	]}}`)
	parsed, ok := decodedCodexResume(response).items(0)[[2]string{"turn", "answer"}]
	if !ok || parsed.Complete || parsed.CreatedAt != nil {
		t.Fatal("resume invented an item completion or native item clock")
	}
}
