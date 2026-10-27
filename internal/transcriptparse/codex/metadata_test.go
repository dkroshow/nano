package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexAuxiliaryFilesRemainScopedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, id, field, precision string
	}{
		{"sidecar-session-index-jsonl", "id-3362dc196d", "updated_at", "native_string"},
		{"sidecar-history-jsonl", "[redacted]", "ts", "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, tc.name)))
			if err != nil || len(batch.Diagnostics) != 0 || len(envelopeObservations(batch)) != 1 {
				t.Fatalf("sidecar: %v %+v", err, batch.Diagnostics)
			}
			if len(state.Items) != 0 || len(state.Turns) != 0 || len(state.Continuation.NativeThreads) != 0 {
				t.Error("auxiliary record created conversation state or a default session")
			}
			obs := envelopeObservations(batch)[0]
			if obs.Scope.Kind != model.ScopeThread || obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != tc.id || obs.Scope.NativeTurnID != nil || obs.Scope.TurnKey != "" {
				t.Error("auxiliary record lost scope")
			}
			var native map[string]json.RawMessage
			if json.Unmarshal(obs.Detail, &native) != nil || len(native[tc.field]) == 0 || len(obs.Times) != 1 || obs.Times[0].Value == nil || obs.Times[0].Class != model.TimeNative || obs.Times[0].SourceField != tc.field || obs.Times[0].Precision != tc.precision {
				t.Error("auxiliary payload or native clock lost")
			}
		})
	}
}

func TestCodexUnknownFileRowsRetainTheirWholeEnvelope(t *testing.T) {
	for _, raw := range []string{
		`{"type":"future_native_row","extra":{"nested":"keep"},"payload":{"number":9007199254740993}}`,
		`{"type":"response_item","outer":"keep","payload":{"type":"future_item","value":true}}`,
	} {
		state, batch, err := parseFileView(parsedView{}, inputFor([]byte(raw+"\n")))
		if err != nil || len(batch.Diagnostics) != 1 || len(envelopeObservations(batch)) != 1 {
			t.Fatalf("unknown row: %v %+v", err, batch.Diagnostics)
		}
		if len(state.Turns) != 0 || len(state.Items) != 0 || batch.Records[0].Disposition != model.RecordUnknown || !strings.Contains(string(envelopeObservations(batch)[0].Detail), "keep") {
			t.Error("unknown row lost its outer envelope or invented conversation state")
		}
	}
}
