package claude

import (
	"os"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudePendingControlSnapshotStaysUnverified(t *testing.T) {
	raw := []byte(`{"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}` + "\n" +
		`{"type":"control_response","response":{"subtype":"success","request_id":"init","response":{"pending_permission_requests":[{"unverifiedShape":"retained"}],"pending_user_dialog_requests":[]}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unverified_pending_controls" {
		t.Fatal("pending snapshot hydrated guessed requests or lacked diagnostic")
	}
	found := false
	for _, obs := range batch.Observations {
		found = found || strings.Contains(string(obs.Detail), `"unverifiedShape":"retained"`)
	}
	if !found {
		t.Fatal("pending control detail lost")
	}
}

func TestClaudeControlSuccessNeedsNativeDecision(t *testing.T) {
	raw, err := os.ReadFile("testdata/unknown-control-decision/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Outcome != model.OutcomeUnknown || len(items[0].Relations) != 1 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unverified_control_decision" || len(state.Turns) != 0 {
		t.Fatal("successful RPC envelope invented a permission decision")
	}
}

func TestClaudeControlDecisionVocabularyBelongsToRequest(t *testing.T) {
	for _, tc := range []struct {
		subtype, body string
		outcome       model.Outcome
	}{
		{"can_use_tool", `{}`, model.OutcomeUnknown},
		{"can_use_tool", `{"behavior":"future"}`, model.OutcomeUnknown},
		{"can_use_tool", `{"action":"accept"}`, model.OutcomeUnknown},
		{"can_use_tool", `{"behavior":"allow"}`, model.OutcomeCompleted},
		{"can_use_tool", `{"behavior":"deny"}`, model.OutcomeDeclined},
		{"elicitation", `{"behavior":"allow"}`, model.OutcomeUnknown},
		{"elicitation", `{"action":"accept","content":{}}`, model.OutcomeCompleted},
		{"elicitation", `{"action":"decline"}`, model.OutcomeDeclined},
		{"elicitation", `{"action":"cancel"}`, model.OutcomeCancelled},
	} {
		raw := []byte(`{"type":"control_request","request_id":"r","request":{"subtype":"` + tc.subtype + `"}}` + "\n" + `{"type":"control_response","response":{"subtype":"success","request_id":"r","response":` + tc.body + `}}` + "\n")
		state, batch, err := parseFileView(parsedView{}, inputFor(raw))
		if err != nil {
			t.Fatal(err)
		}
		items := orderedItems(state)
		if len(items) != 1 || items[0].Outcome != tc.outcome {
			t.Fatalf("%s %s: decision interpreted without its native vocabulary", tc.subtype, tc.body)
		}
		if (len(batch.Diagnostics) > 0) != (tc.outcome == model.OutcomeUnknown) {
			t.Fatal("unknown decision lacks explicit diagnostic")
		}
	}
}
