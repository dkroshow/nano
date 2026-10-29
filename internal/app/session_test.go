package app

import (
	"context"

	"testing"
	"time"

	coreturns "github.com/dkroshow/nano/internal/core/turns"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
)

func TestCompletedTurnSourceDoesNotRequireHeartbeat(t *testing.T) {
	store := mustCompletedStore(t)
	s := &server{completedTurns: hostedmodel.NewCompletedTurnSource(store)}
	if s.heartbeat != nil {
		t.Fatal("test requires no heartbeat")
	}
	if _, created, err := recordTestCompletedTurn(store, coreturns.Finish{HostedThreadID: "nht_target", Provider: "codex", TurnID: "turn_1", FinalMessageID: "final_1", Outcome: "completed", At: time.Now(), Source: "adapter"}); err != nil || !created {
		t.Fatalf("publish = %v %v", created, err)
	}
}

func completedFeedForTest(t *testing.T, source *hostedmodel.CompletedTurnSource) *coreturns.Store {
	t.Helper()
	store, err := source.Store().CoreStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return store
}
