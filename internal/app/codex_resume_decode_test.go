package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	prompts "github.com/dkroshow/nano/internal/core/prompts"
	fixture "github.com/dkroshow/nano/internal/testsupport"
)

// Long resume histories made every independent consumer scan the same reply.
// Measure the full public resume path, including the real live projection.
func BenchmarkCodexResumeConsumers(b *testing.B) {
	turns := make([]any, 200)
	for i := range turns {
		turns[i] = map[string]any{"id": "turn", "status": "completed", "startedAt": 200, "items": []any{map[string]any{"id": "answer", "type": "agentMessage", "text": strings.Repeat("resume answer ", 1000)}}}
	}
	raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "native", "turns": turns}})
	fake := newFakeAppServer(b)
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		b.Fatal(err)
	}
	fake.SetResumeResult(reply)
	daemon := fake.daemon()
	b.Cleanup(fake.DropConnections)
	db, path, err := fixture.TurnDatabase(b, filepath.Join(b.TempDir(), "turns.json"))
	if err != nil {
		b.Fatal(err)
	}
	store := fixture.PromptStore(b, db)
	queue, err := prompts.NewLegacyQueue(db, path, store)
	if err != nil {
		b.Fatal(err)
	}
	journal, err := prompts.NewLegacyJournal(db, path, "benchmark", store)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		p := newLiveProjection("codex")
		changed := make(chan struct{}, 1)
		sess := newSession(sessionConfig{ID: "benchmark", Provider: "codex", HostedThreadID: "benchmark", Projection: p, Turns: store, QueuedTurns: queue, SendAttempts: journal, RuntimeNotify: func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}})
		if err := sess.InitializeBinding(1); err != nil {
			b.Fatal(err)
		}
		sess.ReserveNativeID("native")
		b.StartTimer()
		if err := sess.BeginProviderSpawn(providerRequest{Provider: "codex", Mode: "resume", ThreadID: "native"}, daemon, func() {}); err != nil {
			b.Fatal(err)
		}
		deadline := time.NewTimer(10 * time.Second)
		for sess.Summary().Status != "ready" {
			select {
			case <-changed:
			case <-deadline.C:
				b.Fatal("resume did not become ready")
			}
		}
		deadline.Stop()
		if len(p.TurnList()) == 0 {
			b.Fatal("resume skipped the real projection")
		}
		b.StopTimer()
		sess.Close()
	}
}
