package main

import (
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestGrokOperationalModuleKeepsPromptAndNativeClock(t *testing.T) {
	for _, update := range []string{"agent_message_chunk", "current_mode_update", "unknown_native_update"} {
		for _, clock := range []string{"42", "null"} {
			line := []byte(fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":%q,"content":{"type":"text","text":"A"}},"_meta":{"promptId":"p","eventId":"e","agentTimestampMs":%s}}}`, update, clock))
			active, prompt, at, ok := grokWorkEvidence(protocol.Recv, line)
			want := int64(42)
			if clock == "null" {
				want = 0
			}
			if !ok || !active || prompt != "p" || at != want || grokPromptIDFromWire(line) != "p" {
				t.Fatalf("%s/%s lost prompt or native clock", update, clock)
			}
			if _, _, _, ok := grokWorkEvidence(protocol.Send, line); ok {
				t.Fatal("outgoing update became provider work evidence")
			}
		}
	}
}

func TestGrokOperationalModuleCannotPromoteUnknownReplayToResponse(t *testing.T) {
	store := newTestAgentExecutionStore(t, "run")
	hosted, _, _ := store.beginSession("caller", "hash", "grok", presentationShared)
	_ = store.bindSession(hosted.ID, "nano", "native", "ready", presentationShared)
	op, _, _ := store.beginTurn(hosted.ID, "action", "hash")
	_ = store.markTurnDelivery(op.ID, "prompt", agentTurnRunning, nil)
	sess := &session{provider: "grok", hostedThreadID: hosted.ID, threadID: "native",
		agentExecutions: store, projection: newLiveProjection("grok"), lifecycleState: testLifecycleSnapshot("ready", false)}
	for _, message := range []struct {
		text, event string
		replay      bool
	}{{"live answer", "live", false}, {"old answer", "missing-original", true}} {
		line := []byte(fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}},"_meta":{"promptId":"prompt","eventId":%q,"isReplay":%t}}}`, message.text, message.event, message.replay))
		sess.observeAgentExecutionWire(protocol.Recv, line)
	}
	sess.observeAgentExecutionWire(protocol.Recv, []byte(`{"method":"_x.ai/session/prompt_complete","params":{"sessionId":"native","promptId":"prompt","stopReason":"end_turn"}}`))
	after, _ := store.turn(op.ID)
	if after.Status != agentTurnCompleted || after.FinalResponse != "live answer" {
		t.Fatal("unknown replay replaced the current response candidate")
	}
}

// Failure caught: frames share parsed evidence, so a frame must never get
// another frame's batch, even one of the same length read just before it or
// one whose buffer the caller reused. Boundary: grokWireEvidence.
func TestGrokWireEvidenceBelongsToItsOwnFrame(t *testing.T) {
	frame := func(prompt string) []byte {
		return []byte(fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"A"}},"_meta":{"promptId":%q,"eventId":"e"}}}`, prompt))
	}
	first, second := frame("p1"), frame("p2")
	for _, want := range []string{"p1", "p2", "p1", "p2"} {
		line := first
		if want == "p2" {
			line = second
		}
		if got := grokPromptIDFromWire(line); got != want {
			t.Fatalf("prompt = %q, want %q", got, want)
		}
	}
	reused := frame("q1")
	_ = grokPromptIDFromWire(reused)
	copy(reused, frame("q2"))
	if got := grokPromptIDFromWire(reused); got != "q2" {
		t.Fatalf("prompt after the caller reused its buffer = %q, want q2", got)
	}
}

// Failure caught: the shared evidence store must stay inside its byte budget,
// dropping its oldest frames first. Boundary: the evidence store.
func TestGrokEvidenceStoreKeepsItsByteBudget(t *testing.T) {
	store := newGrokEvidenceStore(1000)
	for i := range 30 {
		store.put([]byte(fmt.Sprintf("%0100d", i)), model.Batch{})
	}
	if store.bytes > 1000 || len(store.batches) != 10 || len(store.order) != 10 {
		t.Fatalf("store holds %d bytes in %d batches (%d ordered), want at most 1000 in 10", store.bytes, len(store.batches), len(store.order))
	}
	if _, ok := store.get([]byte(fmt.Sprintf("%0100d", 19))); ok {
		t.Fatal("an evicted frame is still served")
	}
	if _, ok := store.get([]byte(fmt.Sprintf("%0100d", 29))); !ok {
		t.Fatal("the newest frame was evicted")
	}
}
