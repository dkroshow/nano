package claude

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeEnvelopeRetainsPromptAndAPIRequestIdentities(t *testing.T) {
	for _, name := range []string{"capture-row-user", "capture-row-assistant"} {
		raw := fixture(t, name)
		state, batch, err := parseFileView(parsedView{}, inputFor(raw))
		if err != nil {
			t.Fatal(err)
		}
		var source map[string]json.RawMessage
		_ = json.Unmarshal(raw, &source)
		field, namespace := "promptId", "claude.promptId"
		if name == "capture-row-assistant" {
			field, namespace = "requestId", "claude.apiRequestId"
		}
		item := orderedItems(state)[0]
		foundAlias, foundEnvelope := false, false
		for _, alias := range item.Aliases {
			if alias.Namespace == namespace && string(alias.Value) == string(source[field]) {
				foundAlias = true
			}
		}
		for _, obs := range envelopeObservations(batch) {
			var detail map[string]json.RawMessage
			_ = json.Unmarshal(obs.Detail, &detail)
			if string(detail[field]) == string(source[field]) {
				foundEnvelope = true
			}
		}
		if !foundAlias || !foundEnvelope {
			t.Fatal("native prompt/API request identity was lost outside content blocks")
		}
		for _, turn := range state.Turns {
			if turn.NativeTurnID != "" || turn.Scope.NativeTurnID != nil {
				t.Fatal("prompt/API request ID became a native turn ID")
			}
			if name == "capture-row-assistant" && (turn.Effort != "[scrubbed]" || turn.ServiceTier != "standard") {
				t.Fatal("native effort/service tier was not retained on its turn")
			}
		}
	}
}

func TestClaudeStopEvidenceKeepsNativeReason(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"Answer"}],"stop_reason":"stop_sequence","stop_sequence":"END"}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		found := false
		for _, obs := range turn.Observations {
			if obs.Kind != model.ObservationStop {
				continue
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(obs.Detail, &fields)
			if rawString(fields, "stop_reason") == "stop_sequence" {
				found = true
			}
		}
		if !found {
			t.Fatal("derived completion lost the native stop reason")
		}
	}
}

func TestClaudeWireMessageModelIsScopedEvidence(t *testing.T) {
	state, batch, err := parseWireView(parsedView{}, inputFor([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-example","usage":{"input_tokens":1,"service_tier":"standard"}}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if turn.Model != "claude-example" || turn.ServiceTier != "standard" {
			t.Fatal("wire message configuration was lost")
		}
		for _, obs := range envelopeObservations(batch) {
			if obs.Scope.Kind != model.ScopeTurn || obs.Scope.TurnKey != turn.Key {
				t.Fatal("wire API-message evidence has the wrong scope")
			}
		}
	}
}
