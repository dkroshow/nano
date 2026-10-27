package grok

import (
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"testing"
)

func TestGrokNonTextAndNestedToolContent(t *testing.T) {
	for _, test := range []struct {
		name, part string
		kind       model.ItemType
	}{
		{"unknown-coordination-and-message-image", "image", model.ItemAgentMessage},
		{"capture-tool-nested-image", "image", model.ItemToolResult},
		{"capture-tool-content-diff", "diff", model.ItemToolCall},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, _, err := parseFileView(parsedView{}, inputBytes(fixtureBytes(t, test.name)))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range state.Items {
				if item.Type == test.kind {
					for _, part := range item.Parts {
						if part.Type == test.part && len(part.Detail) > 0 {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatalf("%s lost typed %s content", test.name, test.part)
			}
		})
	}
}
func TestGrokTaskOutputTruncationIsIndependentOfExecution(t *testing.T) {
	state, batch, err := parseFileView(parsedView{}, inputBytes(fixtureBytes(t, "task-truncated-output-after-turn")))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Items) != 1 || len(state.Turns) != 0 {
		t.Fatal("unbound task did not remain a thread-scoped command")
	}
	if len(batch.Diagnostics) != 0 {
		t.Fatalf("task diagnostics: %+v", batch.Diagnostics)
	}
	for _, item := range state.Items {
		if item.Type != model.ItemCommand || item.Outcome != model.OutcomeCompleted || !item.Complete || item.TurnKey != "" || item.Scope.NativeTurnID != nil {
			t.Fatal("task completion became a prompt terminal")
		}
		if len(item.Parts) != 1 || item.Parts[0].Text != "Short output." || item.Parts[0].Availability != model.AvailableTruncated || item.Parts[0].OriginalBytes == nil || *item.Parts[0].OriginalBytes != 10000 {
			t.Fatal("task output truncation lost")
		}
	}
}
func TestGrokPlanKeepsOrderedEntries(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputBytes(fixtureBytes(t, "capture-session-update-plan")))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Type == model.ItemPlan {
			if len(item.Parts) != 3 || !item.Complete || *item.Parts[0].Index != 0 || *item.Parts[2].Index != 2 {
				t.Fatal("plan entry order or completeness lost")
			}
			return
		}
	}
	t.Fatal("plan not emitted")
}

func TestGrokNativeClockRegressionDoesNotReorderContent(t *testing.T) {
	raw := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"A"}},"_meta":{"eventId":"e1","promptId":"p1","agentTimestampMs":2000}}}` + "\n" +
		`{"method":"session/update","params":{"sessionId":"child","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Child"}},"_meta":{"eventId":"e2","promptId":"p2","agentTimestampMs":500}}}` + "\n" +
		`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"B"}},"_meta":{"eventId":"e3","promptId":"p1","agentTimestampMs":1000}}}` + "\n")
	state, batch, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "native_time_regression" || batch.Diagnostics[0].Source.Line != 3 {
		t.Fatal("clock comparison crossed sessions or hid regression")
	}
	for _, item := range state.Items {
		if item.Scope.NativeThreadID != nil && *item.Scope.NativeThreadID == "s1" && item.Text != "AB" {
			t.Fatal("native clock changed source content order")
		}
	}
}
