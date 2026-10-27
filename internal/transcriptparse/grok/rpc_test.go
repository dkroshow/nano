package grok

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokPendingRPCReplayKeepsFirstRequestEvidence(t *testing.T) {
	request := []byte(`{"id":1,"method":"session/load","params":{"sessionId":"s1"}}`)
	state, first, err := parseWireView(parsedView{}, inputBytes(request))
	if err != nil {
		t.Fatal(err)
	}
	state, replay, err := parseWireView(state, inputBytes(request))
	if err != nil || len(replay.Diagnostics) != 0 || !hasRPCRelation(replay, model.RelationReplayOf) {
		t.Fatalf("request replay: %v %+v", err, replay.Diagnostics)
	}
	state, conflict, err := parseWireView(state, inputBytes([]byte(`{"id":1,"method":"session/load","params":{"sessionId":"different"}}`)))
	if err != nil || !hasRPCDiagnostic(conflict, "rpc_request_conflict") {
		t.Error("conflicting pending request replaced original")
	}
	_, reply, err := parseWireView(state, inputBytes([]byte(`{"id":1,"result":{"_meta":{"x.ai/runningPromptId":"p1"}}}`)))
	if err != nil || len(reply.Diagnostics) != 0 {
		t.Fatalf("load reply: %v %+v", err, reply.Diagnostics)
	}
	if !hasRPCRelation(reply, model.RelationResultOf) {
		t.Error("first response lacks typed request relationship")
	}
	var detail map[string]json.RawMessage
	obs := reply.Observations[len(reply.Observations)-1]
	_ = json.Unmarshal(obs.Detail, &detail)
	if documentString(detail, "requestObservationRef") != first.Observations[0].ID || obs.Scope.NativeThreadID == nil || *obs.Scope.NativeThreadID != "s1" {
		t.Error("reply lost original request scope or provenance")
	}
}

func TestGrokPromptReplyReplayDoesNotRepeatCompletion(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputBytes([]byte(`{"id":1,"method":"session/prompt","params":{"sessionId":"s1"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	reply := []byte(`{"id":1,"result":{"stopReason":"end_turn","_meta":{"promptId":"p1"}}}`)
	state, _, err = parseWireView(state, inputBytes(reply))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state)
	next, batch, err := parseWireView(state, inputBytes(reply))
	if err != nil || len(batch.Diagnostics) != 0 || !hasRPCRelation(batch, model.RelationReplayOf) {
		t.Fatalf("prompt replay: %v %+v", err, batch.Diagnostics)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Error("reply replay changed prior checkpoint")
	}
	for _, turn := range next.Turns {
		if len(turn.Completion) != 1 || turn.Outcome != model.OutcomeCompleted {
			t.Error("replay repeated native completion")
		}
	}
	_, batch, err = parseWireView(next, inputBytes([]byte(`{"id":1,"result":{"stopReason":"error","_meta":{"promptId":"p1"}}}`)))
	if err != nil || !hasRPCDiagnostic(batch, "rpc_response_conflict") {
		t.Error("conflicting RPC reply supplied new terminal authority")
	}
}

func TestGrokCompletedRPCIDCanNameANewRequest(t *testing.T) {
	var state parsedView
	for _, row := range []string{
		`{"id":1,"method":"session/load","params":{"sessionId":"first"}}`,
		`{"id":1,"result":{"_meta":{}}}`,
		`{"id":1,"method":"session/load","params":{"sessionId":"second"}}`,
		`{"id":1,"result":{"_meta":{"x.ai/runningPromptId":"new"}}}`,
	} {
		var err error
		var batch model.Batch
		state, batch, err = parseWireView(state, inputBytes([]byte(row)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("reuse: %v %+v", err, batch.Diagnostics)
		}
	}
	if len(state.Continuation.RunningPrompts) != 1 {
		t.Fatal("new request lost native state")
	}
	for _, running := range state.Continuation.RunningPrompts {
		if running.Scope.NativeThreadID == nil || *running.Scope.NativeThreadID != "second" {
			t.Error("completed ID reuse kept old request scope")
		}
	}
}

func TestGrokMalformedRPCIDsCannotJoinRequests(t *testing.T) {
	for _, id := range []string{`null`, `true`, `{"value":1}`, `[1]`} {
		state, batch, err := parseWireView(parsedView{}, inputBytes([]byte(`{"id":`+id+`,"method":"session/request_permission","params":{"sessionId":"s1"}}`)))
		if err != nil || len(state.Continuation.Requests) != 0 || !hasRPCDiagnostic(batch, "invalid_rpc_id") {
			t.Errorf("invalid ID %s accepted", id)
		}
	}
}

func hasRPCRelation(batch model.Batch, kind model.RelationKind) bool {
	for _, change := range batch.Changes {
		if change.Relation != nil && change.Relation.Kind == kind {
			return true
		}
	}
	return false
}

func hasRPCDiagnostic(batch model.Batch, code string) bool {
	for _, diagnostic := range batch.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
