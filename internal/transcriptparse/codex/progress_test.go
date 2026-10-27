package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexItemUpdatesPreserveTheirAddressedFacet(t *testing.T) {
	rows := []string{
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"plan","id":"plan","text":"Plan:"}}}`,
		`{"method":"item/plan/delta","params":{"threadId":"thread","turnId":"turn","itemId":"plan","delta":" "}}`,
		`{"method":"item/plan/delta","params":{"threadId":"thread","turnId":"turn","itemId":"plan","delta":"next"}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"fileChange","id":"patch","status":"inProgress","changes":[]}}}`,
		`{"method":"item/fileChange/outputDelta","params":{"threadId":"thread","turnId":"turn","itemId":"patch","delta":"first"}}`,
		`{"method":"item/fileChange/outputDelta","params":{"threadId":"thread","turnId":"turn","itemId":"patch","delta":" "}}`,
		`{"method":"item/fileChange/patchUpdated","params":{"threadId":"thread","turnId":"turn","itemId":"patch","changes":[{"path":"/example","diff":"+line"}]}}`,
		`{"method":"item/fileChange/outputDelta","params":{"threadId":"thread","turnId":"turn","itemId":"patch","delta":"last"}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"mcpToolCall","id":"mcp","tool":"lookup","arguments":{"query":"keep"},"status":"inProgress"}}}`,
		`{"method":"item/mcpToolCall/progress","params":{"threadId":"thread","turnId":"turn","itemId":"mcp","message":"looking up"}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"cmd","command":"read answer","status":"inProgress"}}}`,
		`{"method":"item/commandExecution/terminalInteraction","params":{"threadId":"thread","turnId":"turn","itemId":"cmd","processId":"process","stdin":"answer\n"}}`,
	}
	var state parsedView
	for _, row := range rows {
		before, _ := json.Marshal(state)
		next, batch, err := parseWireView(state, inputFor([]byte(row)))
		if err != nil || len(batch.Diagnostics) != 0 {
			t.Fatalf("update: %v %+v", err, batch.Diagnostics)
		}
		after, _ := json.Marshal(state)
		if string(before) != string(after) {
			t.Fatal("update mutated previous checkpoint")
		}
		raw, _ := json.Marshal(next)
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
	}
	items := orderedItems(state)
	if len(items) != 4 {
		t.Fatalf("updates duplicated items: %d", len(items))
	}
	if items[0].Type != model.ItemPlan || items[0].Text != "Plan: next" || items[0].Complete {
		t.Error("plan deltas lost text or claimed completion")
	}
	parts := func(item model.Item) map[string]model.Part {
		out := map[string]model.Part{}
		for _, part := range item.Parts {
			out[part.Type] = part
		}
		return out
	}
	patch := parts(items[1])
	if items[1].Type != model.ItemFileChange || items[1].Text != "" || items[1].Complete || patch["tool_output"].Text != "first last" || !strings.Contains(string(patch["changes"].Detail), "+line") {
		t.Error("patch/output updates replaced each other or the invocation")
	}
	if items[2].Text != "Tool call: lookup" || items[2].Complete || parts(items[2])["progress"].Text != "looking up" || !strings.Contains(string(items[2].Detail), `"query":"keep"`) {
		t.Error("MCP progress replaced arguments or claimed completion")
	}
	if items[3].Text != "read answer" || items[3].Complete || parts(items[3])["terminal_input"].Text != "answer\n" {
		t.Error("terminal input became command output or replaced invocation")
	}
	for _, index := range []int{1, 2, 3} {
		item := items[index]
		if len(item.Relations) != 1 || item.Relations[0].Kind != model.RelationProgressOf || item.Relations[0].ToItem != item.ID {
			t.Error("addressed update lost progress relationship")
		}
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeUnknown {
			t.Error("item update completed its parent turn")
		}
	}
	state, _, err := parseWireView(state, inputFor([]byte(`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"plan","id":"plan","text":"Final plan"}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	plan := orderedItems(state)[0]
	if !plan.Complete || plan.Text != "Final plan" || parts(plan)["text"].Text != "Final plan" {
		t.Error("confirming plan snapshot left stale delta parts")
	}
}

func TestCodexOrphanUpdatesKeepPartialItemAndNativeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind model.ItemType
	}{
		{"wire-item-plan-delta", model.ItemPlan},
		{"wire-item-filechange-outputdelta", model.ItemFileChange},
		{"wire-item-filechange-patchupdated", model.ItemFileChange},
		{"wire-item-mcptoolcall-progress", model.ItemToolCall},
		{"wire-item-commandexecution-terminalinteraction", model.ItemCommand},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, batch, err := parseWireView(parsedView{}, inputFor(fixture(t, tc.name)))
			if err != nil || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "item_update_without_start" {
				t.Fatalf("orphan update: %v %+v", err, batch.Diagnostics)
			}
			items := orderedItems(state)
			if len(items) != 1 || items[0].Type != tc.kind || items[0].Complete || items[0].NativeItemID != "item-example" || len(items[0].Observations) != 1 || len(items[0].Observations[0].Detail) == 0 {
				t.Fatal("orphan update disappeared or invented completion")
			}
		})
	}
}
