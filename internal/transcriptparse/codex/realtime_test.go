package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexRealtimeEvidenceDoesNotInventPartIdentity(t *testing.T) {
	for _, name := range []string{"wire-thread-realtime-started", "wire-thread-realtime-closed", "wire-thread-realtime-sdp", "wire-thread-realtime-error", "wire-thread-realtime-itemadded", "wire-thread-realtime-outputaudio-delta", "wire-thread-realtime-transcript-delta", "wire-thread-realtime-transcript-done"} {
		t.Run(name, func(t *testing.T) {
			input := inputFor(fixture(t, name))
			callerTurn := "unrelated"
			input.Scope.NativeTurnID = &callerTurn
			state, batch, err := parseWireView(parsedView{}, input)
			if err != nil || len(batch.Observations) != 1 || len(state.Turns) != 0 {
				t.Fatalf("realtime: %v %+v", err, batch.Diagnostics)
			}
			obs := batch.Observations[0]
			if obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != "thread-example" || obs.Scope.NativeTurnID != nil {
				t.Error("realtime gained unrelated conversation scope")
			}
			if bytes.Contains([]byte(name), []byte("-delta")) {
				if len(state.Items) != 0 || obs.Kind != model.ObservationDelta || !hasSnapshotDiagnostic(batch, "unresolved_realtime_part") {
					t.Error("unaddressed realtime delta acquired a guessed message identity")
				}
				if name == "wire-thread-realtime-transcript-delta" && obs.Text != "Example text." {
					t.Error("flat native delta was lost")
				}
				if name == "wire-thread-realtime-outputaudio-delta" && !bytes.Contains(obs.Detail, []byte(`"data":"example"`)) {
					t.Error("opaque audio bytes were lost or reinterpreted")
				}
			}
			for _, item := range state.Items {
				if !item.Complete || item.Outcome != model.OutcomeUnknown || item.IsFinalAnswer || item.CreatedAt != nil {
					t.Error("realtime body invented finality, action outcome or native time")
				}
			}
			if name == "wire-thread-realtime-transcript-done" {
				items := orderedItems(state)
				if len(items) != 1 || items[0].Text != "Example text." || items[0].Type != model.ItemProviderEvent || !hasSnapshotDiagnostic(batch, "unknown_realtime_role") {
					t.Error("unverified native role was discarded or promoted")
				}
			}
		})
	}
}

func TestCodexRealtimeDoneRetainsFullTextWithoutClaimingEarlierDelta(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputFor([]byte(`{"method":"thread/realtime/transcript/delta","params":{"threadId":"thread","role":"assistant","delta":"tail"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state)
	next, batch, err := parseWireView(state, inputFor([]byte(`{"method":"thread/realtime/transcript/done","params":{"threadId":"thread","role":"assistant","text":"full text"}}`)))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("done: %v %+v", err, batch.Diagnostics)
	}
	after, _ := json.Marshal(state)
	if !bytes.Equal(before, after) {
		t.Error("realtime done changed prior checkpoint")
	}
	items := orderedItems(next)
	if len(items) != 1 || items[0].Text != "full text" || items[0].Type != model.ItemAgentMessage || len(items[0].Observations) != 1 || len(items[0].Relations) != 0 {
		t.Error("done guessed delta overlap or native part identity")
	}
}
