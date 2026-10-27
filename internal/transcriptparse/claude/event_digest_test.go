package claude

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeEventDigestLetsConsumerCheckEvictedUUID(t *testing.T) {
	for _, wire := range []bool{false, true} {
		var state State
		parse := func(raw string) model.Batch {
			t.Helper()
			checkpoint, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err := json.Unmarshal(checkpoint, &restored); err != nil {
				t.Fatal(err)
			}
			input := inputFor([]byte(raw + "\n"))
			var batch model.Batch
			if wire {
				state, batch, err = ParseWire(restored, input)
			} else {
				state, batch, err = ParseFile(restored, input)
			}
			if err != nil {
				t.Fatal(err)
			}
			if batch.Versions.Checkpoint != state.Version {
				t.Fatal("consumer checkpoint version differs from serialized state")
			}
			return batch
		}
		first := parse(`{"type":"user","uuid":"u","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"Prompt"}}`)
		parse(`{"type":"assistant","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"Answer"}],"stop_reason":"end_turn"}}`)
		parse(`{"type":"user","uuid":"next","message":{"role":"user","content":"Next prompt"}}`)
		second := parse(`{"type":"user","uuid":"u","timestamp":"2026-01-01T01:00:00Z","message":{"role":"user","content":"Prompt"}}`)
		conflict := parse(`{"type":"user","uuid":"u","timestamp":"2026-01-01T02:00:00Z","message":{"role":"user","content":"Changed prompt"}}`)
		digest := func(batch model.Batch) string {
			t.Helper()
			for _, obs := range batch.Observations {
				if obs.NativeEventID != "u" {
					continue
				}
				value := obs.NativeEventDigest
				decoded, err := hex.DecodeString(value)
				if err != nil || len(decoded) != 32 {
					t.Fatal("consumer cannot check an evicted native event's payload identity")
				}
				return value
			}
			t.Fatal("native event observation missing")
			return ""
		}
		if digest(first) != digest(second) {
			t.Fatal("replay timestamp changed native event identity")
		}
		if digest(second) == digest(conflict) {
			t.Fatal("changed native content retained the same digest")
		}
		for _, batch := range []model.Batch{first, second} {
			for _, change := range batch.Changes {
				if change.Item == nil {
					continue
				}
				for _, obs := range change.Item.Observations {
					if obs.NativeEventID == "u" && obs.NativeEventDigest != digest(batch) {
						t.Fatal("item evidence lost its native event digest")
					}
				}
			}
		}
	}
}

func TestClaudeReplayRetainsChangedEnvelopeWithSameMessage(t *testing.T) {
	first := `{"type":"user","uuid":"result","promptId":"old-prompt","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read","content":"Original read result"}]},"toolUseResult":{"file":{"content":"First secondary file snapshot"}}}`
	second := `{"type":"user","uuid":"result","promptId":"new-prompt","slug":"resumed","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read","content":"Original read result"}]},"toolUseResult":{"file":{"content":"Later secondary file snapshot"}}}`
	for _, wire := range []bool{false, true} {
		parse := parseFileView
		suffix := "\n"
		if wire {
			parse = parseWireView
			suffix = ""
		}
		view, _, err := parse(parsedView{}, inputFor([]byte(first+suffix)))
		if err != nil {
			t.Fatal(err)
		}
		view, batch, err := parse(view, inputFor([]byte(second+suffix)))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range batch.Diagnostics {
			if d.Code == "native_event_conflict" {
				t.Fatal("changed ancillary envelope overrode identical native message identity")
			}
		}
		items := orderedItems(view)
		if len(items) != 1 || len(items[0].Observations) != 2 || items[0].Text != "Original read result" {
			t.Fatal("same message became a new row or repeated its text")
		}
		envelope := false
		alias := false
		for _, obs := range batch.Observations {
			var fields map[string]json.RawMessage
			if json.Unmarshal(obs.Detail, &fields) == nil && rawString(fields, "promptId") == "new-prompt" && rawString(rawObject(rawObject(fields, "toolUseResult"), "file"), "content") == "Later secondary file snapshot" {
				envelope = true
			}
		}
		for _, a := range items[0].Aliases {
			if a.Namespace == "claude.promptId" && string(a.Value) == `"new-prompt"` {
				alias = true
			}
		}
		if !envelope || !alias {
			t.Fatal("replay dropped changing envelope evidence or its prompt alias")
		}
	}
}

func TestClaudeMessageDigestKeepsStructuredAnswersAndNativeClassification(t *testing.T) {
	base := object(t, `{"type":"user","uuid":"answer","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"question","content":"Answered"}]},"toolUseResult":{"answers":{"Choose?":"First"}}}`)
	for _, changed := range []map[string]json.RawMessage{
		object(t, `{"toolUseResult":{"answers":{"Choose?":"Second"}}}`),
		object(t, `{"isMeta":true}`),
		object(t, `{"apiBlockIndex":1}`),
	} {
		row := copyMap(base)
		for k, v := range changed {
			row[k] = v
		}
		if rowDigest(base) == rowDigest(row) {
			t.Fatal("changed promoted message fact retained replay identity")
		}
	}
}
