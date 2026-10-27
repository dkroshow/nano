package codex

import (
	"fmt"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexResearchReasoningUsesNativePartOrder(t *testing.T) {
	state := wireState(t, fixture(t, "edge-wire-reasoning-indexed-parts"))
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "First.\n\nSecond.\n\nSeparate raw content." || len(items[0].Parts) != 3 {
		t.Fatal("reasoning display followed arrival order instead of native part indices")
	}
	item := items[0]
	if *item.Parts[0].Index != 0 || *item.Parts[1].Index != 1 || item.Parts[2].Type != "reasoning" || *item.Parts[2].Index != 0 {
		t.Fatal("summary/raw indices collided")
	}
	if len(item.Observations) != 4 || item.Observations[1].Source.Frame != 2 || item.Observations[2].Source.Frame != 3 {
		t.Fatal("native part ordering reordered evidence")
	}
}

func TestCodexInvalidReasoningIndexDoesNotAddressPartZero(t *testing.T) {
	for _, index := range []string{"null", "-1", "1.5", `"0"`, "9223372036854775808"} {
		raw := fmt.Sprintf(`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"th","turnId":"t","itemId":"r","summaryIndex":%s,"delta":"unbound"}}`, index)
		state, batch, err := parseWireView(parsedView{}, inputFor([]byte(raw)))
		if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "invalid_part_index" || len(batch.Observations) != 1 || batch.Observations[0].Kind != model.ObservationUnknown {
			t.Fatalf("invalid index %s became part zero or lost evidence", index)
		}
	}
}
