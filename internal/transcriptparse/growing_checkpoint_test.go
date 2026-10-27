package transcriptparse_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A settled turn kept for trailing records (the file's latest) retains only
// the items the reader can still re-emit: its final answer, which a reopening
// tool call retracts. Its prompt, tool results and reasoning belong to the
// consumer's rows. Before this rule, every file kept its whole latest turn:
// 7 GB of the 9.3 GB parser state on the live store.
func TestGrowingCheckpointsRetireSettledTurns(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "grok"} {
		for _, wire := range []bool{false, true} {
			if provider == "claude" && wire {
				continue
			}
			t.Run(fmt.Sprintf("%s/wire=%t", provider, wire), func(t *testing.T) {
				parse := checkpointReader(provider, wire)
				items := map[string]model.Item{}
				var saved []byte
				for turn := 0; turn < 25; turn++ {
					for _, row := range checkpointTurnRows(provider, wire, turn) {
						if !wire {
							row += "\n"
						}
						batch, checkpoint, err := parse(model.Input{Source: model.SourceRef{Label: "growing", Incarnation: "1"}, Bytes: []byte(row)})
						if err != nil {
							t.Fatal(err)
						}
						saved = checkpoint
						for _, change := range batch.Changes {
							if change.Item != nil {
								items[change.Item.ID] = *change.Item
							}
						}
					}
				}
				var state struct {
					Items map[string]model.Item
					Turns map[string]model.Turn
				}
				if err := json.Unmarshal(saved, &state); err != nil {
					t.Fatal(err)
				}
				if len(state.Turns) != 1 || len(saved) > 64<<10 {
					t.Fatalf("closed output retained: turns=%d checkpoint=%d", len(state.Turns), len(saved))
				}
				// The wire keeps the whole turn for the pane's live rows; Grok's
				// file reader keeps the prompt too, since a replayed event binds to it.
				want := map[string]bool{"Answer 24": true, "Prompt 24": wire || provider == "grok"}
				retained := map[string]bool{}
				for id, item := range state.Items {
					if !want[item.Text] {
						t.Fatalf("settled latest turn retained %s %q; only what a later record can re-emit may stay", id, item.Text)
					}
					retained[item.Text] = true
				}
				for text, expected := range want {
					if expected && !retained[text] {
						t.Fatalf("settled latest turn lost %q", text)
					}
				}
				texts := map[string]bool{}
				for _, item := range items {
					texts[item.Text] = true
				}
				for turn := 0; turn < 25; turn++ {
					for _, role := range []string{"Prompt", "Answer"} {
						if !texts[fmt.Sprintf("%s %d", role, turn)] {
							t.Fatal("eviction removed emitted source content")
						}
					}
				}
			})
		}
	}
}

func TestCodexAndGrokOversizedOpenContinuationIsDiagnosed(t *testing.T) {
	for _, provider := range []string{"codex", "grok"} {
		t.Run(provider, func(t *testing.T) {
			parse := checkpointReader(provider, false)
			rows := checkpointTurnRows(provider, false, 0)
			var saved []byte
			var batch model.Batch
			for _, row := range rows[:len(rows)-1] {
				row = strings.ReplaceAll(row, "Answer 0", strings.Repeat("unfinished", 100000))
				var err error
				batch, saved, err = parse(model.Input{Source: model.SourceRef{Label: "large", Incarnation: "1"}, Bytes: []byte(row + "\n")})
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(saved) <= 4<<20 {
				t.Fatal("test input did not exceed continuation ceiling")
			}
			found := false
			for _, d := range batch.Diagnostics {
				found = found || d.Code == "continuation_limit_exceeded"
			}
			if !found {
				t.Fatal("large open state was not diagnosed")
			}
		})
	}
}

func checkpointTurnRows(provider string, wire bool, n int) []string {
	if provider == "claude" {
		return []string{
			fmt.Sprintf(`{"type":"user","uuid":"u%d","sessionId":"s","message":{"role":"user","content":[{"type":"text","text":"Prompt %d"}]}}`, n, n),
			fmt.Sprintf(`{"type":"assistant","uuid":"a%d","sessionId":"s","message":{"id":"m%d","role":"assistant","content":[{"type":"text","text":"Answer %d"}],"stop_reason":"end_turn"}}`, n, n, n),
		}
	}
	if provider == "grok" {
		return []string{
			fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Prompt %d"}},"_meta":{"promptId":"p%d","eventId":"u%d"}}}`, n, n, n),
			fmt.Sprintf(`{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Answer %d"}},"_meta":{"promptId":"p%d","eventId":"a%d"}}}`, n, n, n),
			fmt.Sprintf(`{"method":"_x.ai/session/update","params":{"sessionId":"s","update":{"sessionUpdate":"turn_completed","prompt_id":"p%d","stop_reason":"end_turn"},"_meta":{"eventId":"end%d"}}}`, n, n),
		}
	}
	if wire {
		return []string{
			fmt.Sprintf(`{"method":"turn/started","params":{"threadId":"s","turn":{"id":"p%d","status":"inProgress"}}}`, n),
			fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"s","turnId":"p%d","item":{"id":"u%d","type":"userMessage","content":[{"type":"text","text":"Prompt %d"}]}}}`, n, n, n),
			fmt.Sprintf(`{"method":"item/completed","params":{"threadId":"s","turnId":"p%d","item":{"id":"a%d","type":"agentMessage","text":"Answer %d"}}}`, n, n, n),
			fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"s","turn":{"id":"p%d","status":"completed"}}}`, n),
		}
	}
	return []string{
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"p%d"}}`, n),
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"user_message","message":"Prompt %d"}}`, n),
		fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":"u%d","role":"user","content":"Prompt %d"}}`, n, n),
		fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","id":"a%d","role":"assistant","content":"Answer %d"}}`, n, n),
		fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"p%d"}}`, n),
	}
}
