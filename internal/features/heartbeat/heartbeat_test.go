package heartbeat

import (
	"testing"
)

func TestPausedWakePromptCarriesReasonAndArrival(t *testing.T) {
	want := "This heartbeat is paused because: waiting for review\n\nWhat arrived:\nreview complete\n\nHandle what arrived, then restate the outstanding wait."
	if got := PausedWakePrompt(" waiting for review ", " review complete "); got != want {
		t.Fatalf("prompt = %q", got)
	}
}
