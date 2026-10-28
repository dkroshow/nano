package sessions

import (
	"encoding/json"
	"testing"
)

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
