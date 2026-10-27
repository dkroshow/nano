package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Reader v36 serialization-twin pairs: a
// completed AgentMessage with its response message, a legacy agent_reasoning
// event with its response reasoning, and a completed Reasoning with its
// response reasoning. The real rollout fixtures are shared with the root
// duplicate gate.

func twinFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "duplicate_gate", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type twinRow struct {
	Type    model.ItemType
	Text    string
	Native  string
	Aliases int
}

func twinRows(view parsedView, types ...model.ItemType) []twinRow {
	keep := map[model.ItemType]bool{}
	for _, itemType := range types {
		keep[itemType] = true
	}
	var rows []twinRow
	for _, item := range orderedItems(view) {
		if keep[item.Type] {
			rows = append(rows, twinRow{Type: item.Type, Text: item.Text, Native: item.ExactNativeItemID(), Aliases: len(item.Aliases)})
		}
	}
	return rows
}

// TestCodexSerializationTwinPairsJoinOnceAcrossEveryCheckpoint catches a
// pairing rule that joins a whole-file parse but loses the pair when the file
// is read in chunks. Boundary: the Codex file reader's checkpoint, restored
// after every line of each real fixture, compared with a one-shot parse. It
// also pins each joined row's native id to the event copy's id, which is the
// id the live wire uses, and keeps both copies' ids as aliases.
func TestCodexSerializationTwinPairsJoinOnceAcrossEveryCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		fixture  string
		itemType model.ItemType
		want     []twinRow
	}{
		{"codex-agent-message-local-id.jsonl", model.ItemAgentMessage, []twinRow{
			{model.ItemAgentMessage, "Yes—it finished.", "item-16", 2},
		}},
		{"codex-agent-reasoning-event.jsonl", model.ItemReasoning, []twinRow{
			{model.ItemReasoning, "**Planning parallel task inspection**", "rs_07d03d6335b937ba016ab30c681dbc87d2b8028c9a3039384d", 1},
			{model.ItemReasoning, "**Verifying hosted-threads endpoint usage**", "rs_07d03d6335b937ba016ab30c8351ac87d2b97accd0735a8c47", 1},
		}},
		{"codex-completed-reasoning-local-id.jsonl", model.ItemReasoning, []twinRow{
			{model.ItemReasoning, "**Planning repository inspection and role review**\n\n**Optimizing file reading for commit checks**", "item-2", 2},
			{model.ItemReasoning, "**Planning heartbeat feature inspection**", "item-4", 2},
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			raw := twinFixture(t, tc.fixture)
			want, _, err := parseFileView(parsedView{}, inputFor(raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := twinRows(want, tc.itemType); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("one-shot %s rows = %+v, want %+v", tc.itemType, got, tc.want)
			}
			for _, item := range orderedItems(want) {
				if item.Type != tc.itemType {
					continue
				}
				if len(item.Observations) < 2 || len(item.Relations) == 0 {
					t.Fatalf("%q kept %d observations and %d relations; both copies must survive as twin evidence", item.Text, len(item.Observations), len(item.Relations))
				}
			}
			var state parsedView
			for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
				checkpoint, _ := json.Marshal(state)
				var restored parsedView
				if err := json.Unmarshal(checkpoint, &restored); err != nil {
					t.Fatal(err)
				}
				if state, _, err = parseFileView(restored, inputFor(line)); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(state, want) {
				t.Fatal("checkpoint restore between rows differs from the one-shot parse")
			}
		})
	}
}

func completedAgent(id, turn, text string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":%q,"item":{"type":"AgentMessage","id":%q,"content":[{"type":"Text","text":%q}]}}}`+"\n", turn, id, text)
}
func responseAgent(id, text string) string {
	return fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":%q,"role":"assistant","content":[{"type":"output_text","text":%q}]}}`+"\n", id, text)
}
func eventReasoning(text string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"agent_reasoning","text":%q}}`+"\n", text)
}
func responseReasoning(id string, parts ...string) string {
	summary := make([]map[string]string, len(parts))
	for i, part := range parts {
		summary[i] = map[string]string{"type": "summary_text", "text": part}
	}
	encoded, _ := json.Marshal(summary)
	return fmt.Sprintf(`{"type":"response_item","payload":{"type":"reasoning","id":%q,"summary":%s,"encrypted_content":"gAAAA-fixture-redacted"}}`+"\n", id, encoded)
}
func completedReasoning(id, turn string, parts ...string) string {
	encoded, _ := json.Marshal(parts)
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"item_completed","turn_id":%q,"item":{"type":"Reasoning","id":%q,"summary_text":%s,"raw_content":[]}}}`+"\n", turn, id, encoded)
}
func taskStarted(turn string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q}}`+"\n", turn)
}

// TestCodexSerializationTwinRuleStaysNarrow is the positive control for the
// v36 pairs. Boundary: the Codex file reader's ordered items. It catches a
// rule that merges beyond the three one-to-one pairs: repeats of one
// representation, ambiguous candidates, different item types with equal
// text, reasoning outside a native turn, and several summary events against
// one combined response (kept separate). It also catches an equal text of
// another type blocking a pair; a lone copy joining a later item with equal
// text after another item of its type, which merged two items in a long turn;
// and a joined row's native id changing after it was first set, which history
// refuses (natives: every row's native id, in order).
func TestCodexSerializationTwinRuleStaysNarrow(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int
		natives   []string
	}{
		{"each pair joins in either order", taskStarted("t") + completedAgent("item-1", "t", "Done.") + responseAgent("msg_1", "Done.") +
			responseReasoning("rs_1", "Plan") + eventReasoning("Plan") + responseReasoning("rs_2", "Look") + completedReasoning("item-2", "t", "Look"), 3, nil},
		{"equal replies with their own twins", taskStarted("t") + completedAgent("msg_a", "t", "OK") + responseAgent("msg_a", "OK") +
			completedAgent("msg_b", "t", "OK") + responseAgent("msg_b", "OK"), 2, nil},
		{"local ids pair in order", taskStarted("t") + completedAgent("item-1", "t", "OK") + responseAgent("msg_a", "OK") +
			completedAgent("item-2", "t", "OK") + responseAgent("msg_b", "OK"), 2, nil},
		{"same representation repeats", taskStarted("t") + responseReasoning("rs_1", "Plan") + responseReasoning("rs_2", "Plan") +
			completedAgent("item-1", "t", "OK") + completedAgent("item-2", "t", "OK"), 4, nil},
		{"ambiguous candidates", taskStarted("t") + completedAgent("item-1", "t", "OK") + completedAgent("item-2", "t", "OK") + responseAgent("msg_a", "OK"), 3, nil},
		{"different types with equal text", taskStarted("t") + eventReasoning("Done.") + responseAgent("msg_a", "Done."), 2, nil},
		{"another type's equal text does not block a pair", taskStarted("t") + eventReasoning("OK") + completedAgent("item-1", "t", "OK") + responseAgent("msg_1", "OK"), 2, nil},
		{"a repeated response keeps the event id", taskStarted("t") + completedAgent("item-1", "t", "OK") + responseAgent("msg_1", "OK") + responseAgent("msg_1", "OK"), 1, []string{"item-1"}},
		{"a response first keeps its own id", taskStarted("t") + responseAgent("msg_1", "OK") + completedAgent("item-1", "t", "OK") + responseAgent("msg_1", "OK"), 1, []string{"msg_1"}},
		{"a lone response copy does not join a later equal text", taskStarted("t") + responseReasoning("rs_1", "Wait") + responseReasoning("rs_2", "Other") +
			completedReasoning("item-9", "t", "Wait") + responseReasoning("rs_3", "Wait"), 4, []string{"rs_1", "rs_2", "item-9", "rs_3"}},
		{"a lone event copy does not absorb a later equal text", taskStarted("t") + completedReasoning("item-1", "t", "Wait") + completedReasoning("item-2", "t", "Other") +
			responseReasoning("rs_9", "Wait"), 3, []string{"item-1", "item-2", "rs_9"}},
		{"reasoning outside a native turn", eventReasoning("Plan") + responseReasoning("rs_1", "Plan"), 2, nil},
		{"summary events against one combined response", taskStarted("t") + eventReasoning("Plan") + eventReasoning("Look") + responseReasoning("rs_1", "Plan", "Look"), 3, nil},
		{"attachment on a completed agent message", taskStarted("t") +
			`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","item":{"type":"AgentMessage","id":"item-1","content":[{"type":"Text","text":"See"},{"type":"Image","url":"file:///x.png"}]}}}` + "\n" +
			responseAgent("msg_a", "See"), 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, _, err := parseFileView(parsedView{}, inputFor([]byte(tc.raw)))
			if err != nil {
				t.Fatal(err)
			}
			got := twinRows(view, model.ItemReasoning, model.ItemAgentMessage)
			if len(got) != tc.want {
				t.Fatalf("got %d rows %+v, want %d", len(got), got, tc.want)
			}
			if tc.natives != nil {
				var natives []string
				for _, row := range got {
					natives = append(natives, row.Native)
				}
				if !reflect.DeepEqual(natives, tc.natives) {
					t.Fatalf("row native ids = %q, want %q", natives, tc.natives)
				}
			}
		})
	}
}
