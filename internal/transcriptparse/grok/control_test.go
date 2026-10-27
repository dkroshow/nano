package grok

import (
	"bytes"
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"os"
	"testing"
)

func TestGrokGenuineWrappedCaptureRetainsScopedEvidence(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/paired-repo-parent-child/wire-capture.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var state parsedView
	var observations []model.Observation
	for _, row := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var batch model.Batch
		state, batch, err = parseWireView(state, inputBytes(row))
		if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, batch.Observations...)
	}
	if len(state.Items) < 3 {
		t.Fatal("capture wrappers hid native items")
	}
	parentDone := false
	childOpen := false
	for _, turn := range state.Turns {
		if turn.Scope.NativeTurnID == nil {
			continue
		}
		switch *turn.Scope.NativeTurnID {
		case "parent-prompt-1":
			parentDone = turn.Outcome == model.OutcomeCompleted
		case "child-prompt-1":
			childOpen = turn.Outcome != model.OutcomeCompleted
		}
	}
	if !parentDone || !childOpen {
		t.Fatal("alternate parent terminal did not preserve child scope")
	}
	found := false
	for _, item := range state.Items {
		if item.Type == model.ItemUserMessage && item.Scope.NativeThreadID != nil && *item.Scope.NativeThreadID == "parent-1" {
			found = item.Scope.NativeTurnID != nil && *item.Scope.NativeTurnID == "parent-prompt-1" && item.Observations[0].Source.Direction == "recv"
		}
	}
	if !found {
		t.Fatal("queue evidence or capture direction lost")
	}
	if len(observations) == 0 {
		t.Fatal("control provenance lost")
	}
}
func TestGrokReverseRequestIDsRemainTypedAndScoped(t *testing.T) {
	raw := fixtureBytes(t, "reverse-requests")
	var state parsedView
	var observations []model.Observation
	for _, row := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputBytes(row))
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Diagnostics) != 0 {
			t.Fatalf("known reverse request rejected: %+v", batch.Diagnostics)
		}
		observations = append(observations, batch.Observations...)
	}
	if len(state.Turns) != 0 || len(observations) != 6 {
		t.Fatal("reverse request created a conversation turn or disappeared")
	}
	for i, obs := range observations {
		var detail map[string]json.RawMessage
		_ = json.Unmarshal(obs.Detail, &detail)
		if len(detail["rpcId"]) == 0 {
			t.Fatalf("request scalar lost at %d", i)
		}
		if i >= 3 && len(detail["requestObservationRef"]) == 0 {
			t.Fatal("reverse reply lost correlation")
		}
	}
}
func TestGrokUnknownRPCResponseCannotLeakCredentials(t *testing.T) {
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"access_token":"private-token","refreshToken":"another-private-token"}}`)
	_, batch, err := parseWireView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(batch)
	if bytes.Contains(encoded, []byte("private-token")) {
		t.Fatal("unknown RPC credentials entered generic native detail")
	}
	if len(batch.Observations) != 1 {
		t.Fatal("unknown RPC evidence disappeared")
	}
}

func TestGrokQueueDoesNotBindReplayedUserToFreshPrompt(t *testing.T) {
	rows := []string{
		`{"method":"_x.ai/queue/changed","params":{"sessionId":"s1","runningPromptId":"fresh","runningText":"Again"}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Again"}},"_meta":{"eventId":"old-user","isReplay":true}}}`,
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"New answer"}},"_meta":{"eventId":"new-answer","promptId":"fresh"}}}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range state.Items {
		if item.Type == model.ItemUserMessage && item.Scope.NativeTurnID != nil {
			t.Fatal("replayed user text claimed a fresh prompt")
		}
	}
}
func TestGrokBidirectionalRPCIdentityCannotCross(t *testing.T) {
	rows := []string{
		`{"dir":"send","msg":{"id":1,"method":"session/load","params":{"sessionId":"client-session"}}}`,
		`{"dir":"recv","msg":{"id":1,"method":"session/request_permission","params":{"sessionId":"server-session"}}}`,
		`{"id":1,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`,
	}
	var state parsedView
	var last model.Batch
	for _, row := range rows {
		var err error
		state, last, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	found := false
	for _, d := range last.Diagnostics {
		if d.Code == "ambiguous_rpc_namespace" {
			found = true
		}
	}
	if !found {
		t.Fatal("directionless reply chose one of two matching request namespaces")
	}
	for _, obs := range last.Observations {
		var detail map[string]json.RawMessage
		_ = json.Unmarshal(obs.Detail, &detail)
		if len(detail["requestObservationRef"]) > 0 {
			t.Fatal("ambiguous reply claimed a request")
		}
	}
}
func TestGrokNumericAndStringRPCIDsRemainDifferent(t *testing.T) {
	rows := []string{
		`{"id":1,"method":"session/request_permission","params":{"sessionId":"numeric"}}`,
		`{"id":"1","method":"session/request_permission","params":{"sessionId":"string"}}`,
		`{"id":1,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`,
		`{"id":"1","result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`,
	}
	var state parsedView
	for i, row := range rows {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
		if i >= 2 {
			expected := "numeric"
			if i == 3 {
				expected = "string"
			}
			obs := batch.Observations[0]
			if obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != expected {
				t.Fatal("RPC scalar type was coerced")
			}
		}
	}
}

func TestGrokCaptureWrapperInvalidUTF8IsNotHiddenByUnwrap(t *testing.T) {
	raw := append([]byte(`{"seq":"`), 0xff)
	raw = append(raw, []byte(`","dir":"recv","msg":{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"text"}},"_meta":{"promptId":"p1"}}}}`)...)
	state, batch, err := parseWireView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 0 || len(batch.Records) != 1 || batch.Records[0].Disposition != model.RecordMalformed {
		t.Fatal("invalid outer UTF-8 was hidden by a valid inner message")
	}
}
