package codex

import (
	"bytes"
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/research/" + name + "/input.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func inputFor(raw []byte) model.Input {
	return model.Input{Bytes: raw, Source: model.SourceRef{Label: "fixture", Incarnation: "capture-1"}}
}

func TestCodexNullUsageRemainsThreadMetadata(t *testing.T) {
	for _, mode := range []string{"file", "wire"} {
		var state parsedView
		var batch model.Batch
		var err error
		if mode == "file" {
			state, batch, err = parseFileView(parsedView{}, inputFor(fixture(t, "file-event-msg-token-count")))
		} else {
			state, batch, err = parseWireView(parsedView{}, inputFor([]byte(`{"method":"thread/tokenUsage/updated","params":{"threadId":"native","tokenUsage":null}}`)))
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Turns) != 0 || len(envelopeObservations(batch)) != 1 || len(batch.Diagnostics) != 0 || envelopeObservations(batch)[0].Scope.Kind != model.ScopeThread || envelopeObservations(batch)[0].Scope.TurnKey != "" {
			t.Fatal("null usage created an unsupported turn")
		}
	}
}
func orderedItems(state parsedView) []model.Item {
	items := make([]model.Item, 0, len(state.Items))
	for _, item := range state.Items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
	return items
}
func fileState(t *testing.T, name string) parsedView {
	t.Helper()
	s, _, err := parseFileView(parsedView{}, inputFor(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func wireState(t *testing.T, raw []byte) parsedView {
	t.Helper()
	var s parsedView
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var err error
		s, _, err = parseWireView(s, inputFor(line))
		if err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func TestCodexThreeUsageGrains(t *testing.T) {
	s := fileState(t, "edge-usage-three-grains")
	if len(s.Turns) != 1 {
		t.Fatalf("turns %d", len(s.Turns))
	}
	for _, turn := range s.Turns {
		if turn.Usage == nil || turn.Usage.Input == nil || *turn.Usage.Input != 10 || turn.UsageBasis != "native_turn_snapshot" || len(turn.UsageObservations) != 3 {
			t.Fatalf("usage %+v samples %d", turn.Usage, len(turn.UsageObservations))
		}
	}
}
func TestCodexLateTerminalTargetsNativeTurn(t *testing.T) {
	s := fileState(t, "edge-late-terminal-native-turn")
	if len(s.Turns) != 2 {
		t.Fatalf("turns %d", len(s.Turns))
	}
	for _, turn := range s.Turns {
		if turn.Scope.NativeTurnID == nil {
			t.Fatal("native turn lost")
		}
		switch *turn.Scope.NativeTurnID {
		case "turn-a":
			if turn.Outcome != model.OutcomeCompleted {
				t.Fatal("late terminal missed turn-a")
			}
		case "turn-b":
			if turn.Outcome == model.OutcomeCompleted {
				t.Fatal("late terminal closed turn-b")
			}
		default:
			t.Fatalf("unexpected native turn %q", *turn.Scope.NativeTurnID)
		}
	}
}
func TestCodexPairedFirstTurn(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/paired-first-turn/file.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	file, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	wire := wireState(t, fixture(t, "paired-first-turn"))
	selectItems := func(s parsedView) []model.Item {
		var out []model.Item
		for _, item := range orderedItems(s) {
			if item.Category == model.CategoryMessages || item.Type == model.ItemReasoning {
				out = append(out, item)
			}
		}
		return out
	}
	f, w := selectItems(file), selectItems(wire)
	if len(f) != 3 || len(w) != 3 {
		t.Fatalf("file=%d wire=%d", len(f), len(w))
	}
	for i := range f {
		if f[i].Text != w[i].Text || f[i].Type != w[i].Type {
			t.Fatalf("item %d differs: %q %q", i, f[i].Text, w[i].Text)
		}
	}
	if f[2].Phase != "final_answer" || w[2].Phase != "final_answer" || f[2].Finality.State != model.FinalityFinal {
		t.Fatal("native final phase lost")
	}
	for _, s := range []parsedView{file, wire} {
		if len(s.Turns) != 1 {
			t.Fatalf("turn count %d", len(s.Turns))
		}
		for _, turn := range s.Turns {
			if turn.Outcome != model.OutcomeCompleted || turn.Usage == nil || *turn.Usage.Input != 12165 {
				t.Fatal("terminal or usage lost")
			}
		}
	}
}
func TestCodexWireTimeAndFailedCompletedItem(t *testing.T) {
	rows := []string{
		`{"method":"item/started","params":{"threadId":"thread-a","turnId":"turn-a","item":{"type":"commandExecution","id":"cmd","command":"false","status":"inProgress"}}}`,
		`{"method":"item/commandExecution/outputDelta","params":{"threadId":"thread-a","turnId":"turn-a","itemId":"cmd","delta":"aGVsbG8="}}`,
		`{"method":"item/completed","params":{"threadId":"thread-a","turnId":"turn-a","startedAtMs":1000,"completedAtMs":2000,"item":{"type":"commandExecution","id":"cmd","command":"false","aggregatedOutput":"aGVsbG8=","status":"failed","exitCode":1}}}`,
	}
	var s parsedView
	observed := time.Unix(500, 0).UTC()
	for i, row := range rows {
		input := inputFor([]byte(row))
		input.ObservedAt = &observed
		var err error
		s, _, err = parseWireView(s, input)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			for _, item := range s.Items {
				if item.CreatedAt != nil {
					t.Fatal("receipt clock became native time")
				}
			}
		}
	}
	items := orderedItems(s)
	if len(items) != 1 || !items[0].Complete || items[0].Outcome != model.OutcomeFailed || items[0].NativeItemID != "cmd" {
		t.Fatal("item completion erased failure or identity")
	}
	if len(items[0].Parts) == 0 || items[0].Parts[len(items[0].Parts)-1].Text != "aGVsbG8=" {
		t.Fatal("plain output was decoded as base64")
	}
	if items[0].CreatedAt == nil || items[0].CreatedAt.UnixMilli() != 1000 {
		t.Fatal("native item timing lost")
	}
}
func TestCodexCheckpointEverySplit(t *testing.T) {
	raw := fixture(t, "edge-usage-three-grains")
	want, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	for split := 0; split <= len(raw); split++ {
		first, _, err := parseFileView(parsedView{}, inputFor(raw[:split]))
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, _ := json.Marshal(first)
		var restored parsedView
		if err = json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		got, _, err := parseFileView(restored, inputFor(raw[split:]))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d differs", split)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(after, checkpoint) {
			t.Fatal("prior checkpoint mutated")
		}
	}
}

func TestCodexTwinEvidenceKeepsRealRepeat(t *testing.T) {
	s := fileState(t, "edge-nonadjacent-twins-and-real-repeat")
	items := orderedItems(s)
	if len(items) != 2 || len(items[0].Observations) != 2 || len(items[0].Relations) != 1 || items[0].Relations[0].Kind != model.RelationSerializationTwin || items[0].NativeItemID != "answer-1" || items[1].NativeItemID != "answer-2" {
		t.Fatal("serialization evidence merged a later real message or lost its twin")
	}
	if items[0].TurnKey == items[1].TurnKey {
		t.Fatal("identical text crossed a native turn boundary")
	}
}
func TestCodexCommandResultLinksWithoutCompletingCall(t *testing.T) {
	raw := []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"false\"}"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"failed","exit_code":1}}` + "\n")
	s, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(s)
	if len(items) != 2 || items[0].Type != model.ItemCommand || !items[0].Complete || items[0].Outcome != model.OutcomeUnknown {
		t.Fatal("call generation is not command success")
	}
	if items[1].Outcome != model.OutcomeFailed || len(items[1].Relations) != 1 || items[1].Relations[0].ToItem != items[0].ID {
		t.Fatal("command result lost scoped call relationship")
	}
}
func TestCodexReasoningPartIndicesRemainSeparate(t *testing.T) {
	raw := []byte(`{"method":"item/started","params":{"threadId":"th","turnId":"t","item":{"id":"r","type":"reasoning","summary":["First","Second"],"content":[]}}}`)
	s := wireState(t, raw)
	item := orderedItems(s)[0]
	if len(item.Parts) != 2 || *item.Parts[0].Index != 0 || *item.Parts[1].Index != 1 {
		t.Fatal("reasoning indices alias the final loop index")
	}
	next := []byte(`{"method":"item/reasoning/summaryTextDelta","params":{"threadId":"th","turnId":"t","itemId":"r","summaryIndex":0,"delta":" tail"}}`)
	s, _, err := parseWireView(s, inputFor(next))
	if err != nil {
		t.Fatal(err)
	}
	item = orderedItems(s)[0]
	if item.Parts[0].Text != "First tail" || item.Parts[1].Text != "Second" || item.Text != "First tail\n\nSecond" {
		t.Fatal("summary delta was appended to the wrong part")
	}
}
func TestCodexCheckpointBetweenPairedRows(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/paired-first-turn/file.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	var state parsedView
	for _, line := range bytes.SplitAfter(raw, []byte{'\n'}) {
		checkpoint, _ := json.Marshal(state)
		var restored parsedView
		if err = json.Unmarshal(checkpoint, &restored); err != nil {
			t.Fatal(err)
		}
		state, _, err = parseFileView(restored, inputFor(line))
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(restored)
		if !bytes.Equal(checkpoint, after) {
			t.Fatal("prior row checkpoint mutated")
		}
	}
	if !reflect.DeepEqual(state, want) {
		t.Fatal("row checkpoint restore differs from one-shot")
	}
}

func TestCodexLegacyUserTurnsKeepTwinsAndRealPrompts(t *testing.T) {
	raw := []byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":"Again"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"Again"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"First"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"Again"}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Turns) != 2 || len(state.Items) != 3 {
		t.Fatalf("turns=%d items=%d", len(state.Turns), len(state.Items))
	}
}

// Catalog observations have their own assertions; these tests inspect the
// native envelope evidence that existed before typed catalog extraction.
func envelopeObservations(batch model.Batch) []model.Observation {
	var observations []model.Observation
	for _, obs := range batch.Observations {
		catalog := len(obs.Metadata) != 0
		for _, clock := range obs.Times {
			catalog = catalog || clock.Meaning == model.TimeActivity || clock.Meaning == model.TimeConversation
		}
		if !catalog {
			observations = append(observations, obs)
		}
	}
	return observations
}
