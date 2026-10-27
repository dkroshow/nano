package codex

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexEightRequestResponseFixtures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome model.Outcome
		tool    bool
	}{
		{"applypatchapproval", model.OutcomeDeclined, false},
		{"execcommandapproval", model.OutcomeDeclined, false},
		{"item-commandexecution-requestapproval", model.OutcomeDeclined, false},
		{"item-filechange-requestapproval", model.OutcomeDeclined, false},
		{"item-permissions-requestapproval", model.OutcomeCompleted, false},
		{"item-tool-requestuserinput", model.OutcomeCompleted, false},
		{"mcpserver-elicitation-request", model.OutcomeDeclined, false},
		{"item-tool-call", model.OutcomeFailed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state parsedView
			var reply []byte
			for _, raw := range bytes.Split(bytes.TrimSpace(fixture(t, "edge-response-"+tc.name)), []byte{'\n'}) {
				before, _ := json.Marshal(state)
				var restored parsedView
				if err := json.Unmarshal(before, &restored); err != nil {
					t.Fatal(err)
				}
				next, batch, err := parseWireView(restored, inputFor(raw))
				if err != nil || len(batch.Diagnostics) != 0 {
					t.Fatalf("pair: %v %+v", err, batch.Diagnostics)
				}
				after, _ := json.Marshal(restored)
				if !bytes.Equal(before, after) {
					t.Error("reply mutated prior checkpoint")
				}
				state, reply = next, raw
			}
			items := orderedItems(state)
			want := 1
			if tc.tool {
				want = 2
			}
			if len(items) != want {
				t.Fatalf("facets %d want %d", len(items), want)
			}
			answered := items[len(items)-1]
			if !answered.Complete || answered.Outcome != tc.outcome || len(answered.Relations) != 1 || answered.Relations[0].Kind != model.RelationResultOf {
				t.Error("reply lost decision/outcome or request relation")
			}
			if tc.tool && (items[0].Outcome != model.OutcomeUnknown || answered.Type != model.ItemToolResult || answered.Text != "Example tool failure." || answered.Relations[0].ToItem != items[0].ID) {
				t.Error("dynamic result replaced invocation or lost typed output")
			}
			for _, turn := range state.Turns {
				if turn.Outcome != model.OutcomeUnknown {
					t.Error("control/tool response settled parent turn")
				}
			}
			replayed, batch, err := parseWireView(state, inputFor(reply))
			if err != nil || len(batch.Diagnostics) != 0 {
				t.Fatalf("reply replay: %v %+v", err, batch.Diagnostics)
			}
			again := orderedItems(replayed)
			if len(again) != want || again[want-1].ID != answered.ID || again[want-1].Outcome != tc.outcome || len(again[want-1].Relations) != 2 || again[want-1].Relations[1].Kind != model.RelationReplayOf {
				t.Error("repeated response created a new result or lost replay evidence")
			}
		})
	}
}

func TestCodexRPCNamespacesAndCompletedIDReuse(t *testing.T) {
	rows := []struct{ dir, raw string }{
		{"send", `{"id":1,"method":"thread/read","params":{"threadId":"thread"}}`},
		{"recv", `{"id":1,"method":"item/fileChange/requestApproval","params":{"threadId":"thread","itemId":"patch"}}`},
		{"send", `{"id":1,"result":{"decision":"decline"}}`},
		{"recv", `{"id":1,"result":{"accessToken":"SECRET_VALUE","thread":{"id":"thread","turns":[]}}}`},
		{"recv", `{"id":1,"method":"item/fileChange/requestApproval","params":{"threadId":"thread","itemId":"patch"}}`},
		{"send", `{"id":1,"result":{"decision":"accept"}}`},
	}
	var state parsedView
	for _, row := range rows {
		input := inputFor([]byte(row.raw))
		input.Source.Direction = row.dir
		next, batch, err := parseWireView(state, input)
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("RPC: %v %+v", err, batch.Diagnostics)
		}
		raw, _ := json.Marshal([]any{next, batch})
		if strings.Contains(string(raw), "SECRET_VALUE") {
			t.Fatal("generic RPC response leaked credentials")
		}
		state = next
	}
	items := orderedItems(state)
	if len(items) != 2 || items[0].Outcome != model.OutcomeDeclined || items[1].Outcome != model.OutcomeCompleted || items[0].ID == items[1].ID {
		t.Fatal("completed RPC ID reuse was confused with pending replay or crossed directions")
	}
}

func TestCodexRPCReplyRequiresUnambiguousTypedScope(t *testing.T) {
	var state parsedView
	for _, row := range []struct{ dir, raw string }{
		{"send", `{"id":1,"method":"thread/read","params":{}}`},
		{"recv", `{"id":1,"method":"item/fileChange/requestApproval","params":{"threadId":"thread","itemId":"patch"}}`},
	} {
		input := inputFor([]byte(row.raw))
		input.Source.Direction = row.dir
		state, _, _ = parseWireView(state, input)
	}
	_, batch, err := parseWireView(state, inputFor([]byte(`{"id":1,"result":{"decision":"decline"}}`)))
	if err != nil || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "ambiguous_rpc_namespace" {
		t.Fatalf("directionless collision: %v %+v", err, batch.Diagnostics)
	}
	input := inputFor([]byte(`{"id":"1","result":{"decision":"decline"}}`))
	input.Source.Direction = "send"
	_, batch, err = parseWireView(state, input)
	if err != nil || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unresolved_rpc_response" {
		t.Fatalf("typed ID: %v %+v", err, batch.Diagnostics)
	}
	input.Bytes = []byte(`{"id":1,"result":{"decision":"decline"}}`)
	other := "other-thread"
	input.Scope.NativeThreadID = &other
	_, batch, err = parseWireView(state, input)
	if err != nil || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "rpc_scope_conflict" {
		t.Fatalf("scope conflict: %v %+v", err, batch.Diagnostics)
	}
}

func TestCodexSensitiveRepliesAndConflictingDecisions(t *testing.T) {
	for _, method := range []string{"account/chatgptAuthTokens/refresh", "attestation/generate"} {
		state, _, err := parseWireView(parsedView{}, inputFor([]byte(`{"id":1,"method":"`+method+`","params":{}}`)))
		if err != nil {
			t.Fatal(err)
		}
		state, batch, err := parseWireView(state, inputFor([]byte(`{"id":1,"result":{"accessToken":"SECRET_VALUE","signature":"SECRET_VALUE"}}`)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("sensitive reply: %v %+v", err, batch.Diagnostics)
		}
		raw, _ := json.Marshal([]any{state, batch})
		if strings.Contains(string(raw), "SECRET_VALUE") || len(state.Items) != 0 || len(state.Turns) != 0 || batch.Records[0].Disposition != model.RecordNonContent {
			t.Error("sensitive RPC result became conversation content or retained opaque material")
		}
	}
	state := wireState(t, fixture(t, "edge-response-item-filechange-requestapproval"))
	input := inputFor([]byte(`{"id":"item-example","result":{"decision":"accept"}}`))
	input.Source.Direction = "send"
	next, batch, err := parseWireView(state, input)
	if err != nil || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "rpc_response_conflict" || orderedItems(next)[0].Outcome != model.OutcomeDeclined {
		t.Error("conflicting decision overwrote the accepted response")
	}
}

func TestCodexNullSuccessDoesNotBecomeFailedExecution(t *testing.T) {
	for _, raw := range []string{"null", " null ", `"false"`, "0"} {
		payload := map[string]json.RawMessage{"success": json.RawMessage(raw)}
		if nativeOutcome(payload) != model.OutcomeUnknown || replyOutcome("item/tool/call", payload) != model.OutcomeUnknown {
			t.Errorf("invalid or missing success %s became execution evidence", raw)
		}
	}
}
