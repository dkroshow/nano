package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
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
	thread := "session-example"
	return model.Input{Bytes: raw, Source: model.SourceRef{Label: "fixture", Incarnation: "capture-1"}, Scope: model.Scope{NativeThreadID: &thread}}
}
func orderedItems(state parsedView) []model.Item {
	items := make([]model.Item, 0, len(state.Items))
	for _, item := range state.Items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
	return items
}

func TestFileReaderKeepsInterleavedMessageBlocks(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "interleaved-message-ids")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 4 {
		t.Fatalf("items %d", len(items))
	}
	for i, text := range []string{"Inspect the example.", "First", "Second", "Third"} {
		if items[i].Text != text {
			t.Fatalf("item %d text %q", i, items[i].Text)
		}
	}
	if len(state.Turns) != 1 {
		t.Fatalf("API messages became human turns: %d", len(state.Turns))
	}
	for i, item := range items {
		if item.NativeItemID != []string{"", "m1:0", "m2:0", "m1:1"}[i] {
			t.Fatal("message/block identity lost its exact components")
		}
	}
}

// Claude saves a prompt that arrives mid-turn only as a queued_command
// attachment (Claude 2.1.285 line shapes, scrubbed). Read as metadata, those
// prompts were missing from saved transcripts. Each becomes a prompt row in
// the turn it arrived in, identified by the sender's source_uuid when there
// is one; Claude's own queued task notifications are not prompts.
func TestFileReaderReadsQueuedPromptsInTheirTurn(t *testing.T) {
	raw := []byte(`{"type":"user","uuid":"u1","sessionId":"session-example","timestamp":"2026-10-01T00:00:00.000Z","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"Write about rain."}]}}
{"type":"assistant","uuid":"a1","sessionId":"session-example","timestamp":"2026-10-01T00:00:01.000Z","isSidechain":false,"message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"}]}}
{"type":"attachment","uuid":"q1","parentUuid":"a1","sessionId":"session-example","timestamp":"2026-10-01T00:00:02.000Z","isSidechain":false,"entrypoint":"nano","renderedRole":"system","attachment":{"type":"queued_command","prompt":[{"text":"Also mention snow.","type":"text"}],"source_uuid":"sent-1","commandMode":"prompt","timestamp":"2026-10-01T00:00:02.000Z"}}
{"type":"attachment","uuid":"q2","parentUuid":"q1","sessionId":"session-example","timestamp":"2026-10-01T00:00:03.000Z","isSidechain":false,"entrypoint":"cli","attachment":{"type":"queued_command","prompt":"And hail.","commandMode":"prompt","timestamp":"2026-10-01T00:00:03.000Z"}}
{"type":"attachment","uuid":"q3","parentUuid":"q2","sessionId":"session-example","timestamp":"2026-10-01T00:00:04.000Z","isSidechain":false,"entrypoint":"nano","attachment":{"type":"queued_command","prompt":"<task-notification>done</task-notification>","source_uuid":"task-1","commandMode":"task-notification","origin":{"kind":"task"},"timestamp":"2026-10-01T00:00:04.000Z"}}
{"type":"assistant","uuid":"a2","parentUuid":"q3","sessionId":"session-example","timestamp":"2026-10-01T00:00:05.000Z","isSidechain":false,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"Rain, snow and hail."}]}}
`)
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	type prompt struct{ text, id string }
	var prompts []prompt
	turns := map[string]bool{}
	for _, item := range orderedItems(state) {
		turns[item.Scope.TurnKey] = true
		if item.Type == model.ItemUserMessage {
			prompts = append(prompts, prompt{item.Text, item.Observations[0].NativeEventID})
		}
	}
	want := []prompt{{"Write about rain.", "u1"}, {"Also mention snow.", "sent-1"}, {"And hail.", "q2"}}
	if !reflect.DeepEqual(prompts, want) {
		t.Fatalf("prompt rows = %+v, want %+v", prompts, want)
	}
	if len(turns) != 1 || len(state.Turns) != 1 {
		t.Fatalf("items span %d turns (%d emitted), want the queued prompts inside the one running turn", len(turns), len(state.Turns))
	}
}

// A saved turn is Claude's own turn: the rows that carry one promptId (line
// shapes from Claude 2.1.300 files, scrubbed). Grouping by the person's
// prompt rows instead folded a turn Claude started for its own task
// notification, with a prompt it absorbed there, into the previous turn, and
// split a turn at a prompt Claude took mid-turn as a plain row: one saved turn
// then held two answers, or a prompt's answer was an earlier message.
func TestFileReaderGroupsTurnsByClaudesPromptID(t *testing.T) {
	user := func(uuid, prompt, content string, extra string) string {
		return `{"type":"user","uuid":"` + uuid + `","promptId":"` + prompt + `","sessionId":"session-example","timestamp":"2026-10-01T00:00:00.000Z","isSidechain":false` + extra + `,"message":{"role":"user","content":` + content + `}}`
	}
	assistant := func(uuid, id, content string) string {
		return `{"type":"assistant","uuid":"` + uuid + `","sessionId":"session-example","timestamp":"2026-10-01T00:00:01.000Z","isSidechain":false,"message":{"id":"` + id + `","role":"assistant","content":` + content + `}}`
	}
	text := func(s string) string { return `[{"type":"text","text":"` + s + `"}]` }
	for _, tc := range []struct {
		name  string
		rows  []string
		turns [][]string // each turn's prompt event ids and assistant message ids, in order
		ends  []model.Outcome
	}{
		{"a task notification's turn absorbs a prompt", []string{
			user("sent-a", "p1", text("First prompt."), ""),
			assistant("a1", "m1", text("Answer A.")),
			user("tn-1", "p2", `"<task-notification>\n<task-id>t1</task-id>\n</task-notification>"`, `,"origin":{"kind":"task-notification"}`),
			assistant("a2", "m2", `[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/work/a"}}]`),
			user("r1", "p2", `[{"type":"tool_result","tool_use_id":"call-1","content":"ok"}]`, ""),
			`{"type":"attachment","uuid":"q1","parentUuid":"r1","sessionId":"session-example","timestamp":"2026-10-01T00:00:02.000Z","isSidechain":false,"entrypoint":"nano","attachment":{"type":"queued_command","prompt":[{"text":"Second prompt.","type":"text"}],"source_uuid":"sent-b","commandMode":"prompt","timestamp":"2026-10-01T00:00:02.000Z"}}`,
			assistant("a3", "m3", text("Answer B.")),
		}, [][]string{{"sent-a", "m1"}, {"m2", "sent-b", "m3"}}, nil},
		{"a prompt taken mid-turn stays in its turn", []string{
			user("sent-a", "p1", text("Start."), ""),
			assistant("a1", "m1", `[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/work/a"}}]`),
			user("r1", "p1", `[{"type":"tool_result","tool_use_id":"call-1","content":"ok"}]`, ""),
			user("sent-c", "p1", text("Also do X."), ""),
			assistant("a2", "m2", text("Done.")),
		}, [][]string{{"sent-a", "m1", "sent-c", "m2"}}, nil},
		{"an interruption marker settles the turn it ends", []string{
			user("sent-a", "p1", text("Start."), ""),
			assistant("a1", "m1", text("Partial.")),
			user("i1", "p2", text("[Request interrupted by user]"), ""),
			user("sent-d", "p2", text("Next."), ""),
			assistant("a2", "m2", text("Next answer.")),
		}, [][]string{{"sent-a", "m1"}, {"sent-d", "m2"}}, []model.Outcome{model.OutcomeInterrupted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte{}
			for _, row := range tc.rows {
				raw = append(append(raw, row...), '\n')
			}
			state, _, err := parseFileView(parsedView{}, inputFor(raw))
			if err != nil {
				t.Fatal(err)
			}
			var order []string
			members := map[string][]string{}
			for _, item := range orderedItems(state) {
				id := ""
				switch item.Type {
				case model.ItemUserMessage:
					id = item.Observations[0].NativeEventID
				case model.ItemAgentMessage, model.ItemToolCall:
					id, _, _ = strings.Cut(item.NativeItemID, ":")
				}
				if id == "" || slices.Contains(members[item.Scope.TurnKey], id) {
					continue
				}
				if _, seen := members[item.Scope.TurnKey]; !seen {
					order = append(order, item.Scope.TurnKey)
				}
				members[item.Scope.TurnKey] = append(members[item.Scope.TurnKey], id)
			}
			var got [][]string
			for _, key := range order {
				got = append(got, members[key])
			}
			if !reflect.DeepEqual(got, tc.turns) {
				t.Fatalf("turns = %v, want %v", got, tc.turns)
			}
			for i, want := range tc.ends {
				if outcome := state.Turns[order[i]].Outcome; outcome != want {
					t.Fatalf("turn %d outcome = %s, want %s", i, outcome, want)
				}
			}
		})
	}
}

// Enhanced hides every shell command, failed or not, with its result. A
// result row does not name its tool, so the reader tags a shell command's
// result; without the tag, a failed one in an interrupted turn showed.
func TestFileReaderTagsShellCommandResults(t *testing.T) {
	raw := []byte(`{"type":"user","uuid":"u1","sessionId":"session-example","timestamp":"2026-10-01T00:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"Check the build."}]}}
{"type":"assistant","uuid":"a1","sessionId":"session-example","timestamp":"2026-10-01T00:00:01.000Z","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"call-shell","name":"Bash","input":{"command":"make"}}]}}
{"type":"user","uuid":"r1","sessionId":"session-example","timestamp":"2026-10-01T00:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-shell","is_error":true,"content":"exit 2"}]}}
{"type":"assistant","uuid":"a2","sessionId":"session-example","timestamp":"2026-10-01T00:00:03.000Z","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"call-read","name":"Read","input":{"file_path":"/work/a"}}]}}
{"type":"user","uuid":"r2","sessionId":"session-example","timestamp":"2026-10-01T00:00:04.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","is_error":true,"content":"missing"}]}}
`)
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string][]string{}
	for _, item := range orderedItems(state) {
		if item.Type == model.ItemToolResult {
			tags[item.CallID] = item.FilterTags
		}
	}
	if !reflect.DeepEqual(tags["call-shell"], []string{"command"}) || len(tags["call-read"]) != 0 {
		t.Fatalf("result tags = %v, want only the shell command's result tagged command", tags)
	}
}

func TestWireInitialContentAndBlockStop(t *testing.T) {
	var state parsedView
	for _, line := range bytes.Split(bytes.TrimSpace(fixture(t, "wire-initial-content-at-start")), []byte{'\n'}) {
		input := inputFor(line)
		at := time.Unix(100, 0).UTC()
		input.ObservedAt = &at
		var err error
		state, _, err = parseWireView(state, input)
		if err != nil {
			t.Fatal(err)
		}
	}
	items := orderedItems(state)
	if len(items) != 1 || items[0].Text != "Initial tail" || !items[0].Complete || items[0].Outcome != model.OutcomeUnknown || items[0].CreatedAt != nil {
		t.Fatalf("initial/stop semantics: %+v", items)
	}
}

func TestFileToolResultDoesNotCompleteInvocation(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "tool-call-completeness-and-failure")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 {
		t.Fatalf("items %d", len(items))
	}
	if !items[0].Complete || items[0].Outcome != model.OutcomeUnknown || items[0].CallID != "call1" {
		t.Fatal("generated invocation became execution success")
	}
	if !items[1].Complete || items[1].Outcome != model.OutcomeFailed || len(items[1].Relations) != 1 || items[1].Relations[0].Kind != model.RelationResultOf || items[1].Relations[0].ToItem != items[0].ID {
		t.Fatal("result facet lost failure or linkage")
	}
	for _, turn := range state.Turns {
		if turn.Outcome == model.OutcomeFailed {
			t.Fatal("tool failure failed the parent turn")
		}
	}
}

func TestFailedToolResultKeepsToolResultCategoryAcrossFileAndWire(t *testing.T) {
	fileLine := []byte("{\"type\":\"user\",\"uuid\":\"result1\",\"session_id\":\"session-example\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"call1\",\"content\":\"failed\",\"is_error\":true}]}}\n")
	wireLine := []byte(`{"type":"user","uuid":"result1","session_id":"session-example","isApiErrorMessage":true,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"failed","is_error":true}]}}`)

	file, _, err := parseFileView(parsedView{}, inputFor(fileLine))
	if err != nil {
		t.Fatal(err)
	}
	wire, _, err := parseWireView(parsedView{}, inputFor(wireLine))
	if err != nil {
		t.Fatal(err)
	}
	fileItems, wireItems := orderedItems(file), orderedItems(wire)
	if len(fileItems) != 1 || len(wireItems) != 1 {
		t.Fatalf("file=%d wire=%d items", len(fileItems), len(wireItems))
	}
	if fileItems[0].Category != model.CategoryToolResult || wireItems[0].Category != model.CategoryToolResult {
		t.Fatalf("failed tool-result categories: file=%q wire=%q", fileItems[0].Category, wireItems[0].Category)
	}
	if fileItems[0].Outcome != model.OutcomeFailed || wireItems[0].Outcome != model.OutcomeFailed {
		t.Fatalf("failed tool-result outcomes: file=%q wire=%q", fileItems[0].Outcome, wireItems[0].Outcome)
	}
}

func TestPairedSplitSnapshotsDoNotDuplicateStream(t *testing.T) {
	file, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "paired-split-file")))
	if err != nil {
		t.Fatal(err)
	}
	var wire parsedView
	for _, line := range bytes.Split(bytes.TrimSpace(fixture(t, "paired-split-wire")), []byte{'\n'}) {
		wire, _, err = parseWireView(wire, inputFor(line))
		if err != nil {
			t.Fatal(err)
		}
	}
	fitems, witems := orderedItems(file), orderedItems(wire)
	if len(fitems) != 3 || len(witems) != 3 {
		t.Fatalf("file=%d wire=%d items", len(fitems), len(witems))
	}
	for i := range fitems {
		if fitems[i].Type != witems[i].Type || fitems[i].Text != witems[i].Text {
			t.Fatalf("item %d differs", i)
		}
	}
	for _, turn := range wire.Turns {
		if turn.Usage == nil || *turn.Usage.Input != 2 || *turn.Usage.Output != 4 || *turn.Usage.CacheRead != 10 {
			t.Fatalf("stream usage revisions: %+v", turn.Usage)
		}
	}
}

func TestClaudeCheckpointEverySplitAndReplay(t *testing.T) {
	raw := fixture(t, "bare-string-message")
	want, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Items) != 2 {
		t.Fatalf("items %d", len(want.Items))
	}
	for split := 0; split <= len(raw); split++ {
		first, _, err := parseFileView(parsedView{}, inputFor(raw[:split]))
		if err != nil {
			t.Fatal(err)
		}
		checkpoint, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		var restored parsedView
		if err := json.Unmarshal(checkpoint, &restored); err != nil {
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
			t.Fatalf("split %d mutated prior", split)
		}
	}
	replayed, _, err := parseFileView(want, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.Items) != 2 || len(replayed.Turns) != 1 {
		t.Fatal("UUID replay created new items or turns")
	}
}

func TestWireToolArgumentsAndAdverseTerminal(t *testing.T) {
	rows := []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"m1"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call1","name":"Bash","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"false\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`,
		`{"type":"user","uuid":"result1","message":{"content":[{"type":"tool_result","tool_use_id":"call1","content":"failed","is_error":true}]}}`,
		`{"type":"assistant","uuid":"answer1","message":{"id":"m2","content":[{"type":"text","text":"I could not run it."}],"stop_reason":"end_turn"}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true}`,
		`{"type":"system","subtype":"turn_duration","durationMs":42}`,
	}
	var state parsedView
	for _, row := range rows {
		var err error
		state, _, err = parseWireView(state, inputFor([]byte(row)))
		if err != nil {
			t.Fatal(err)
		}
	}
	items := orderedItems(state)
	if len(items) != 3 || items[0].Text != "false" || items[0].Parts[0].Text != `{"command":"false"}` || !items[0].Complete || items[0].Outcome != model.OutcomeUnknown {
		t.Fatal("stream tool argument fragments or generation/execution separation lost")
	}
	if items[1].Outcome != model.OutcomeFailed || len(items[1].Relations) != 1 {
		t.Fatal("failed tool result lost")
	}
	if items[2].Finality.State != model.FinalityRetracted || items[2].IsFinalAnswer {
		t.Fatal("adverse result retained candidate answer")
	}
	for _, turn := range state.Turns {
		if turn.Outcome != model.OutcomeFailed || len(turn.FinalItemRefs) != 0 || turn.DurationMS != 42 {
			t.Fatal("duration row changed failure or finality")
		}
	}
}

func TestClaudeObservationTimesKeepTheirOwnScope(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T00:00:00Z","message":{"id":"m1","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"stop_reason":"end_turn"}}` + "\n")
	state, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		for _, observation := range item.Observations {
			for _, stamp := range observation.Times {
				if !reflect.DeepEqual(stamp.Scope, observation.Scope) || stamp.ObservationRef != observation.ID {
					t.Fatal("block clock borrowed sibling scope")
				}
			}
		}
	}
	for _, turn := range state.Turns {
		for _, observation := range turn.Observations {
			for _, stamp := range observation.Times {
				if !reflect.DeepEqual(stamp.Scope, observation.Scope) || stamp.ObservationRef != observation.ID {
					t.Fatal("terminal clock borrowed block scope")
				}
			}
		}
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
