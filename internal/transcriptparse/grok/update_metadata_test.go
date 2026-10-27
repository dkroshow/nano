package grok

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokKnownModeAndRetryUpdatesAreMetadata(t *testing.T) {
	for _, name := range []string{"capture-session-update-current-mode-update", "capture-x-ai-session-update-retry-state"} {
		input := inputBytes(fixtureBytes(t, name))
		input.Closed = true
		thread, turn := "id-1", "unrelated-turn"
		if name == "capture-session-update-current-mode-update" {
			thread = "id-20241"
		}
		input.Scope.NativeThreadID, input.Scope.NativeTurnID = &thread, &turn
		state, batch, err := parseFileView(parsedView{}, input)
		if err != nil || len(batch.Diagnostics) != 0 || len(state.Items) != 0 || len(state.Turns) != 0 {
			t.Fatalf("known metadata %s: %v %+v", name, err, batch.Diagnostics)
		}
		found := false
		for _, obs := range batch.Observations {
			if fieldsOf(obs.Detail)["sessionUpdate"] != nil {
				found = obs.Kind == model.ObservationMetadata && obs.Scope.NativeThreadID != nil && obs.Scope.NativeTurnID == nil
			}
		}
		if !found {
			t.Error("known native update lost metadata body")
		}
	}
}

func TestGrokClientSentUpdateCannotClaimProviderCompletion(t *testing.T) {
	input := inputBytes([]byte(`{"dir":"send","msg":{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"turn_completed","prompt_id":"p","stopReason":"end_turn"}}}}`))
	state, batch, err := parseWireView(parsedView{}, input)
	if err != nil || len(state.Turns) != 0 || len(state.Items) != 0 || !hasRPCDiagnostic(batch, "unexpected_update_direction") {
		t.Fatal("sent update acquired provider authority")
	}
	found := false
	for _, obs := range batch.Observations {
		if bytes.Contains(obs.Detail, []byte("turn_completed")) {
			found = true
			if obs.Outcome != "" || obs.Kind != model.ObservationMetadata {
				t.Error("outgoing update has outcome authority")
			}
		}
	}
	if !found {
		t.Error("sent update native evidence discarded")
	}
}

func TestGrokUpdateEnvelopeMetadataSurvivesReplay(t *testing.T) {
	rows := []string{
		`{"timestamp":1,"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ha"}},"_meta":{"eventId":"e","promptId":"p","chunkId":7,"streamStartMs":1000,"turnStartMs":900,"totalTokens":44,"future":9007199254740993}}}`,
		`{"timestamp":2,"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ha"}},"_meta":{"eventId":"e","promptId":"p","chunkId":7,"isReplay":true}}}`,
	}
	var state parsedView
	var envelopes []model.Observation
	for _, row := range rows {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("replay metadata: %v %+v", err, batch.Diagnostics)
		}
		for _, obs := range batch.Observations {
			if fieldsOf(obs.Detail)["_meta"] != nil {
				envelopes = append(envelopes, obs)
			}
		}
	}
	if len(envelopes) != 2 || !bytes.Contains(envelopes[0].Detail, []byte("9007199254740993")) {
		t.Fatal("native envelope metadata lost or rounded")
	}
	for _, obs := range envelopes {
		if obs.Source.BlockPath != "$.params._meta" || obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != "s" || obs.Scope.NativeTurnID == nil || *obs.Scope.NativeTurnID != "p" {
			t.Error("envelope metadata lost source/scope")
		}
	}
	item := controlItems(state)[0]
	if item.Text != "ha" || len(item.Observations) != 2 || len(item.Relations) != 1 || item.Relations[0].Kind != model.RelationReplayOf {
		t.Fatal("envelope replay changed content assembly")
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) == 0 {
		t.Fatal("checkpoint not serializable")
	}
}

func TestGrokUnknownNotificationRetainsNativeFields(t *testing.T) {
	row := []byte(`{"jsonrpc":"2.0","method":"_x.ai/future_notification","params":{"sessionId":"s","value":9007199254740993},"future":true}`)
	_, batch, err := parseWireView(parsedView{}, inputBytes(row))
	if err != nil || len(batch.Observations) != 1 || !hasRPCDiagnostic(batch, "unsupported_envelope") {
		t.Fatal("unknown notification missing")
	}
	if rpcDigest(fieldsOf(batch.Observations[0].Detail)) != rpcDigest(fieldsOf(row)) {
		t.Fatal("unknown notification native fields discarded")
	}
}
