package grok

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestExplicitReplayAfterEvictionKeepsNativeEndpoint(t *testing.T) {
	first := `{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"earlier"}},"_meta":{"promptId":"old","eventId":"event"}}}`
	terminal := `{"method":"_x.ai/session/update","params":{"sessionId":"s","update":{"sessionUpdate":"turn_completed","prompt_id":"old","stop_reason":"end_turn"},"_meta":{"eventId":"terminal"}}}`
	next := `{"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"next"}},"_meta":{"promptId":"new","eventId":"prompt"}}}`
	for _, wire := range []bool{false, true} {
		parse, suffix := parseFileView, "\n"
		if wire {
			parse, suffix = parseWireView, ""
		}
		var view parsedView
		for _, row := range []string{first, terminal, next} {
			var err error
			view, _, err = parse(view, inputBytes([]byte(row+suffix)))
			if err != nil {
				t.Fatal(err)
			}
		}
		before, _ := json.Marshal(view.Continuation.Turns)
		replay := strings.Replace(first, `"eventId":"event"`, `"eventId":"event","isReplay":true`, 1)
		after, batch, err := parse(view, inputBytes([]byte(replay+suffix)))
		if err != nil {
			t.Fatal(err)
		}
		turns, _ := json.Marshal(after.Continuation.Turns)
		if !bytes.Equal(before, turns) || len(after.Items) != len(view.Items) {
			t.Fatal("explicit replay reopened settled output")
		}
		found := false
		for _, change := range batch.Changes {
			if change.Item != nil || change.Turn != nil {
				t.Fatal("explicit replay emitted replacement assembly")
			}
			if r := change.Relation; r != nil && r.Kind == model.RelationReplayOf && r.ToNativeObservation != nil {
				ref := r.ToNativeObservation
				found = ref.Namespace == "native.eventId" && string(ref.Value) == `"event"` && ref.Scope.NativeTurnID != nil && *ref.Scope.NativeTurnID == "old" && ref.Scope.TurnKey == "" && ref.Scope.ItemID == ""
			}
		}
		if !found {
			t.Fatal("late replay lost its exact native endpoint")
		}
	}
}

func TestReplayOnlyCapturePreservesStandaloneEvidence(t *testing.T) {
	raw := fixtureBytes(t, "replay-straddles-load")
	for _, wire := range []bool{false, true} {
		parse, suffix := parseFileView, "\n"
		if wire {
			parse, suffix = parseWireView, ""
		}
		var view parsedView
		var texts []string
		var targets []string
		for _, row := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
			var batch model.Batch
			var err error
			view, batch, err = parse(view, inputBytes(append(append([]byte(nil), row...), suffix...)))
			if err != nil {
				t.Fatal(err)
			}
			for _, obs := range batch.Observations {
				if obs.Text != "" {
					texts = append(texts, obs.Text)
				}
			}
			for _, change := range batch.Changes {
				if r := change.Relation; r != nil && r.ToNativeObservation != nil {
					targets = append(targets, string(r.ToNativeObservation.Value))
				}
			}
		}
		if strings.Join(texts, "|") != "Old answer.|Older answer." || strings.Join(targets, "|") != `"e1"|"e2"` || len(view.Items) != 0 || len(view.Turns) != 0 {
			t.Fatal("replay-only capture lost evidence or invented target assembly")
		}
	}
}
