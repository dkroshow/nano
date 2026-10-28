package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// Long resume histories made every independent consumer scan the same reply.
func BenchmarkCodexResumeConsumers(b *testing.B) {
	turns := make([]any, 200)
	for i := range turns {
		turns[i] = map[string]any{"id": "turn", "status": "completed", "startedAt": 200, "items": []any{map[string]any{"id": "answer", "type": "agentMessage", "text": strings.Repeat("resume answer ", 1000)}}}
	}
	raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "native", "turns": turns}})
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := newLiveProjection("codex")
		response := decodedCodexResume(raw)
		seedCodexResume(p, response)
		response.inFlightTurn()
		response.compact()
		(codexLifecycleAdapter{epoch: 1, sequence: 1, response: response, nativeID: "native", settled: true}).Current()
	}
}

func decodedCodexResume(raw []byte) *codexResumeResponse {
	response, err := decodeCodexResumeResponse(raw)
	if err != nil {
		return newCodexResumeResponse(nil)
	}
	return response
}

// The shared decode must preserve unknown provider fields and large integers
// when removing history for the state and wire views.
func TestCodexResumeSharedReplyPreservesCompactFields(t *testing.T) {
	response, err := decodeCodexResumeResponse([]byte(`{"extension":{"counter":9007199254740993},"thread":{"id":"native","extension":true,"turns":[{"id":"active","status":"inProgress","startedAt":200,"items":[]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"extension":{"counter":9007199254740993},"thread":{"extension":true,"id":"native","turns":[]}}`
	if got := string(response.compact()); got != want {
		t.Fatalf("compact response = %s, want %s", got, want)
	}
	// Compaction and the item consumer must not remove the turn from the reply
	// shared with lifecycle evidence later in the handshake.
	response.items(0)
	turn, active := response.inFlightTurn()
	if !active || turn.ID != "active" || turn.startedAtMs() != 200000 {
		t.Fatalf("turn after consumers = %+v, active=%v", turn, active)
	}
}
