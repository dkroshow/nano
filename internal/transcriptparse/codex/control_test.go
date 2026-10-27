package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexAllTenServerRequestShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind model.ItemType
	}{
		{"account-chatgptauthtokens-refresh", ""},
		{"attestation-generate", ""},
		{"applypatchapproval", model.ItemInteraction},
		{"execcommandapproval", model.ItemInteraction},
		{"item-commandexecution-requestapproval", model.ItemInteraction},
		{"item-filechange-requestapproval", model.ItemInteraction},
		{"item-permissions-requestapproval", model.ItemInteraction},
		{"item-tool-call", model.ItemToolCall},
		{"item-tool-requestuserinput", model.ItemInteraction},
		{"mcpserver-elicitation-request", model.ItemInteraction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, "request-"+tc.name)))
			if err != nil || len(batch.Diagnostics) != 0 || len(batch.Observations) != 1 {
				t.Fatalf("request: %v %+v", err, batch.Diagnostics)
			}
			if tc.kind == "" {
				if len(state.Items) != 0 || len(state.Turns) != 0 || batch.Observations[0].Scope.Kind != model.ScopeConnection {
					t.Error("authentication or attestation request became conversation content")
				}
				return
			}
			items := orderedItems(state)
			if len(items) != 1 {
				t.Fatalf("request items %d", len(items))
			}
			item := items[0]
			wantOutcome := model.OutcomeRunning
			if tc.kind == model.ItemToolCall {
				wantOutcome = model.OutcomeUnknown
			}
			if item.Type != tc.kind || !item.Complete || item.Outcome != wantOutcome || item.IsFinalAnswer || item.NativeItemID != "" {
				t.Error("request generation was confused with its decision or native item identity")
			}
			if item.Scope.NativeThreadID == nil || *item.Scope.NativeThreadID != "thread-example" {
				t.Error("request native thread/conversation identity lost")
			}
			if len(item.Aliases) != 1 || item.Aliases[0].Namespace != "codex.serverRequestId" || string(item.Aliases[0].Value) != `"item-example"` {
				t.Error("typed server request alias lost")
			}
			if tc.name == "item-tool-call" && (item.ToolName != "example" || item.CallID != "item-example" || len(item.Parts) != 1 || string(item.Parts[0].Detail) != `{}`) {
				t.Error("dynamic call arguments or identity lost")
			}
			for _, turn := range state.Turns {
				if turn.Outcome != model.OutcomeUnknown {
					t.Error("request settled its parent turn")
				}
			}
		})
	}
}

func TestCodexServerRequestIDsKeepScalarType(t *testing.T) {
	state := wireState(t, fixture(t, "edge-wire-request-id-types"))
	items := orderedItems(state)
	if len(items) != 2 || items[0].ID == items[1].ID || string(items[0].Aliases[0].Value) != "1" || string(items[1].Aliases[0].Value) != `"1"` {
		t.Fatal("numeric/string request IDs collided")
	}
}

func TestCodexSensitiveServerRequestsKeepOnlySafeFields(t *testing.T) {
	for _, method := range []string{"account/chatgptAuthTokens/refresh", "attestation/generate"} {
		row := map[string]any{"id": 1, "method": method, "params": map[string]any{"reason": "unauthorized", "accessToken": "SECRET_VALUE", "challenge": "SECRET_VALUE"}}
		raw, _ := json.Marshal(row)
		state, batch, err := parseWireView(parsedView{}, inputFor(raw))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("sensitive request: %v %+v", err, batch.Diagnostics)
		}
		out, _ := json.Marshal([]any{state, batch})
		if strings.Contains(string(out), "SECRET_VALUE") {
			t.Fatal("opaque authentication or attestation material was retained")
		}
	}
}

func TestCodexPendingRequestReplayAndConflictKeepFirstEvidence(t *testing.T) {
	raw := fixture(t, "request-item-commandexecution-requestapproval")
	first, batch, err := parseWireView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	item := orderedItems(first)[0]
	startFound := false
	for _, stamp := range item.Observations[0].Times {
		if stamp.SourceField == "startedAtMs" && stamp.Class == model.TimeNative && stamp.Meaning == model.TimeStart && stamp.Value != nil {
			startFound = true
		}
	}
	if !startFound {
		t.Error("native request start clock was lost")
	}
	checkpoint, _ := json.Marshal(first)
	var restored parsedView
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	replayed, batch, err := parseWireView(restored, inputFor(raw))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("replay: %v %+v", err, batch.Diagnostics)
	}
	after, _ := json.Marshal(restored)
	if string(checkpoint) != string(after) {
		t.Error("request replay mutated its prior checkpoint")
	}
	items := orderedItems(replayed)
	startFound = false
	for _, stamp := range items[0].Observations[len(items[0].Observations)-1].Times {
		if stamp.SourceField == "startedAtMs" && stamp.Value != nil {
			startFound = true
		}
	}
	if !startFound {
		t.Error("repeated request lost its own native clock")
	}
	if len(items) != 1 || items[0].ID != item.ID || items[0].SourceSeq != item.SourceSeq || items[0].Outcome != model.OutcomeRunning || len(items[0].Relations) != 1 || items[0].Relations[0].Kind != model.RelationReplayOf {
		t.Error("pending request replay changed semantic request")
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	msg := rawObject(row, "msg")
	params := rawObject(msg, "params")
	params["itemId"] = mustJSON("conflicting-target")
	msg["params"] = mustJSON(params)
	row["msg"] = mustJSON(msg)
	conflicted, batch, err := parseWireView(replayed, inputFor(mustJSON(row)))
	if err != nil || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "rpc_request_conflict" {
		t.Fatalf("conflict: %v %+v", err, batch.Diagnostics)
	}
	if len(conflicted.Items) != 1 || len(orderedItems(conflicted)[0].Observations) != 2 {
		t.Error("conflicting payload replaced the pending request")
	}
}
