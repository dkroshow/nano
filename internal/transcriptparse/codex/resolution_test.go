package codex

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexExternalResolutionDoesNotInventApproval(t *testing.T) {
	request := []byte(`{"id":1,"method":"item/fileChange/requestApproval","params":{"threadId":"thread","turnId":"turn","itemId":"patch"}}`)
	resolution := []byte(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":1}}`)
	state, _, err := parseWireView(parsedView{}, inputFor(request))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(state)
	resolved, batch, err := parseWireView(state, inputFor(resolution))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("resolution: %v %+v", err, batch.Diagnostics)
	}
	after, _ := json.Marshal(state)
	if string(before) != string(after) {
		t.Error("resolution changed the prior checkpoint")
	}
	items := orderedItems(resolved)
	if len(items) != 1 || items[0].Outcome != model.OutcomeUnknown || !items[0].Complete || len(items[0].Relations) != 1 || items[0].Relations[0].Kind != model.RelationProgressOf {
		t.Error("external resolution invented approval or lost its scope")
	}
	for _, turn := range resolved.Turns {
		if turn.Outcome != model.OutcomeUnknown {
			t.Error("external resolution completed parent turn")
		}
	}
	replayed, batch, err := parseWireView(resolved, inputFor(resolution))
	if err != nil || len(batch.Diagnostics) != 0 || len(orderedItems(replayed)[0].Relations) != 2 || orderedItems(replayed)[0].Relations[1].Kind != model.RelationReplayOf {
		t.Error("repeated resolution lost replay evidence")
	}
	input := inputFor([]byte(`{"id":1,"result":{"decision":"decline"}}`))
	input.Source.Direction = "send"
	answered, batch, err := parseWireView(resolved, input)
	if err != nil || len(batch.Diagnostics) != 0 || orderedItems(answered)[0].Outcome != model.OutcomeDeclined {
		t.Error("later actual response did not retain the decision")
	}
	again, batch, err := parseWireView(answered, inputFor(resolution))
	if err != nil || len(batch.Diagnostics) != 0 || orderedItems(again)[0].Outcome != model.OutcomeDeclined {
		t.Error("resolution erased a known decision")
	}
	newRequest, batch, err := parseWireView(resolved, inputFor(request))
	if err != nil || len(batch.Diagnostics) != 0 || len(newRequest.Items) != 2 {
		t.Error("resolved request ID reuse became old request replay")
	}
}

func TestCodexOrphanExternalResolutionRemainsAnObservation(t *testing.T) {
	state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, "wire-serverrequest-resolved")))
	if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Observations) != 1 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unresolved_rpc_resolution" {
		t.Fatalf("orphan resolution: %v %+v", err, batch.Diagnostics)
	}
	var detail map[string]json.RawMessage
	if json.Unmarshal(batch.Observations[0].Detail, &detail) != nil || rawString(rawObject(detail, "params"), "requestId") != "item-example" {
		t.Error("native request reference was lost")
	}
}
