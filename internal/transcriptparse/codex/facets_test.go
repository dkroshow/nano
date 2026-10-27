package codex

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexTurnAggregatesKeepOneFacetPerNativeTurn(t *testing.T) {
	var state parsedView
	for _, name := range []string{"wire-turn-diff-updated", "wire-turn-plan-updated"} {
		var err error
		var batch model.Batch
		state, batch, err = parseWireView(state, inputFor(fixture(t, name)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("aggregate: %v %+v", err, batch.Diagnostics)
		}
	}
	before, _ := json.Marshal(state)
	var restored parsedView
	if err := json.Unmarshal(before, &restored); err != nil {
		t.Fatal(err)
	}
	next, batch, err := parseWireView(restored, inputFor([]byte(`{"method":"turn/diff/updated","params":{"threadId":"thread-example","turnId":"turn-example","diff":"replacement"}}`)))
	if err != nil || len(batch.Diagnostics) != 0 {
		t.Fatalf("replacement: %v %+v", err, batch.Diagnostics)
	}
	after, _ := json.Marshal(restored)
	if !bytes.Equal(before, after) {
		t.Error("aggregate changed prior checkpoint")
	}
	items := orderedItems(next)
	if len(items) != 2 || items[0].Text != "replacement" || items[0].Type != model.ItemFileChange || items[1].Type != model.ItemPlan {
		t.Fatal("aggregate update created duplicates or crossed plan/diff facets")
	}
	if items[0].NativeItemID != "" || len(items[0].Aliases) != 0 || items[0].SourceSeq != 1 || len(items[0].Relations) != 2 {
		t.Error("turn aggregate fabricated native item identity or lost progress evidence")
	}
	for _, turn := range next.Turns {
		if turn.Outcome != model.OutcomeUnknown {
			t.Error("aggregate completed turn")
		}
	}
	next, _, err = parseWireView(next, inputFor([]byte(`{"method":"turn/diff/updated","params":{"threadId":"thread-example","turnId":"different","diff":"separate"}}`)))
	if err != nil || len(next.Items) != 3 {
		t.Error("aggregate crossed native turn")
	}
}

func TestCodexApprovalReviewStaysSeparateFromItsTarget(t *testing.T) {
	var state parsedView
	for _, name := range []string{"wire-item-autoapprovalreview-started", "wire-item-autoapprovalreview-completed"} {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputFor(fixture(t, name)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("review: %v %+v", err, batch.Diagnostics)
		}
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Type != model.ItemInteraction || !items[0].Complete || items[0].Outcome != model.OutcomeRunning || items[0].NativeItemID != "" {
		t.Fatal("review completion invented action completion or native target identity")
	}
	if len(items[0].Aliases) != 1 || items[0].Aliases[0].Namespace != "codex.reviewId" || len(items[0].Observations) != 2 {
		t.Error("review lifetime lost identity or observations")
	}
	for _, stamp := range items[0].Times {
		if stamp.Value != nil && stamp.Class == model.TimeNative && stamp.Precision != "millisecond" {
			t.Error("review native clocks changed units")
		}
	}
	state, _, err := parseWireView(state, inputFor([]byte(`{"method":"item/completed","params":{"threadId":"thread-example","turnId":"turn-example","item":{"id":"target","type":"commandExecution","command":"actual","status":"inProgress"}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"approved", "denied", "timedOut", "aborted"} {
		input := inputFor([]byte(`{"method":"item/autoApprovalReview/completed","params":{"threadId":"thread-example","turnId":"turn-example","reviewId":"example","targetItemId":"target","review":{"status":"` + status + `"},"action":{"type":"command","command":"reviewed"}}}`))
		next, batch, err := parseWireView(state, input)
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("decision: %v %+v", err, batch.Diagnostics)
		}
		items := orderedItems(next)
		want := map[string]model.Outcome{"approved": model.OutcomeCompleted, "denied": model.OutcomeDeclined, "timedOut": model.OutcomeFailed, "aborted": model.OutcomeCancelled}[status]
		if len(items) != 2 || items[0].Outcome != want || items[1].Outcome != model.OutcomeRunning || items[1].Text != "actual" {
			t.Error("review decision changed execution evidence")
		}
		for _, turn := range next.Turns {
			if turn.Outcome != model.OutcomeUnknown {
				t.Error("review completed parent")
			}
		}
	}
}
