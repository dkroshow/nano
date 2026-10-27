package app

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// The daemon's resume slots bound concurrent thread/resume calls at the app
// server. A slot held through Nano's post-reply work kept the next resume from
// being sent at all while that work waited, here on the execution store that
// boot's registrations also take.
func TestCodexResumeSlotIsFreeOnceTheReplyArrives(t *testing.T) {
	fake := newFakeAppServer(t)
	server := newResumeTestServer(t, fake.wsURL())
	// A completed turn in the reply makes post-reply work read the execution store.
	fake.setResumeTurns(json.RawMessage(`[{"id":"turn-done","status":"completed","completedAt":1787600200,"items":[{"id":"answer-done","type":"agentMessage","phase":"final_answer","text":"done"}]}]`))
	fake.setReplyDelay("thread/resume", 300*time.Millisecond)
	const threads = codexResumeConcurrency + 1
	ids := make([]string, 0, threads)
	for i := range threads {
		response := postResume(t, server, fmt.Sprintf("slot-%d", i))
		var summary sessionSummary
		if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil || summary.ID == "" {
			t.Fatalf("resume %d: %s", i, response.Body.String())
		}
		ids = append(ids, summary.ID)
	}

	server.agentExecutions.mu.Lock()
	deadline := time.Now().Add(3 * time.Second)
	for fake.resumeCount() < threads && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	sent := fake.resumeCount()
	server.agentExecutions.mu.Unlock()
	if sent != threads {
		t.Fatalf("%d of %d resumes sent while the first replies' post-reply work waited; the slots stayed held", sent, threads)
	}
	for _, id := range ids {
		waitForSessionStatus(t, server, id, "ready")
	}
}
