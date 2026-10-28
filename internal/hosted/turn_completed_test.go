package hosted

import (
	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
)

func TestRetainedLegacyCompletedTurnStoreRemainsReadable(t *testing.T) {
	store := mustCompletedStore(t)
	at := time.Date(2026, 8, 26, 3, 4, 5, 6, time.UTC)
	turn, created, err := recordTestCompletedTurn(store, coreturns.Finish{HostedThreadID: "nht_target", Provider: "codex", TurnID: "turn_1", FinalMessageID: "final_1", Outcome: "completed", At: at, Source: "adapter"})
	if err != nil || !created {
		t.Fatalf("initial publish = %#v created=%v err=%v", turn, created, err)
	}
	if turn.TargetHostedThreadID != "nht_target" || turn.Provider != "codex" || turn.TurnID != "turn_1" || turn.FinalMessageID != "final_1" || turn.CompletedAt != at.Format(time.RFC3339Nano) || turn.Source != "adapter" {
		t.Fatalf("published source fact = %#v", turn)
	}
	if _, created, err := recordTestCompletedTurn(store, coreturns.Finish{HostedThreadID: "nht_target", Provider: "codex", TurnID: "turn_1", FinalMessageID: "final_1", Outcome: "completed", At: at.Add(time.Second), Source: "adapter"}); err != nil || created {
		t.Fatalf("duplicate source publish created=%v err=%v", created, err)
	}
}
