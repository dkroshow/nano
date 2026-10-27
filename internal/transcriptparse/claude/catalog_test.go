package claude

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeCatalogRemembersChangesAcrossRestoreAndNativeBinding(t *testing.T) {
	for _, wire := range []bool{false, true} {
		var state State
		parse := func(raw string) model.Metadata {
			t.Helper()
			before, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err = json.Unmarshal(before, &restored); err != nil {
				t.Fatal(err)
			}
			var batch model.Batch
			input := inputFor([]byte(raw + "\n"))
			input.Scope.NativeThreadID = nil
			if wire {
				state, batch, err = ParseWire(restored, input)
			} else {
				state, batch, err = ParseFile(restored, input)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(restored)
			if string(before) != string(after) {
				t.Fatal("catalog mutated prior state")
			}
			fields := model.Metadata{}
			for _, obs := range batch.Observations {
				for key, value := range obs.Metadata {
					fields[key] = value
				}
			}
			return fields
		}
		parse(`{"type":"custom-title","customTitle":"Early title","cwd":"/early"}`)
		parse(`{"type":"assistant","sessionId":"root","isSidechain":true,"message":{"role":"assistant","content":"Child evidence"}}`)
		fields := parse(`{"type":"ai-title","sessionId":"root","aiTitle":"Must not replace"}`)
		if _, changed := fields[model.MetadataTitle]; changed {
			t.Fatal("first native identity discarded title precedence")
		}
		if _, changed := fields[model.MetadataCWD]; changed {
			t.Fatal("first native identity reset known metadata")
		}
		if fields[model.MetadataNativeThreadID].Value != "root" {
			t.Fatal("native identity was not emitted")
		}
		fields = parse(`{"type":"system","sessionId":"root","cwd":"/early"}`)
		if len(fields) != 0 {
			t.Fatal("identical metadata was emitted again")
		}
		fields = parse(`{"type":"system","sessionId":"root","cwd":"/early","timestamp":0}`)
		if fields[model.MetadataCWD].Value != "/early" {
			t.Fatal("new native attribution was lost")
		}
		fields = parse(`{"type":"custom-title","sessionId":"root","agentId":"child","isSidechain":true,"customTitle":"Child title"}`)
		if fields[model.MetadataNativeThreadID].Value != "child" || fields[model.MetadataParentNativeID].Value != "root" {
			t.Fatal("child catalog lost native scope")
		}
		fields = parse(`{"type":"ai-title","sessionId":"root","aiTitle":"Must still not replace"}`)
		if _, changed := fields[model.MetadataTitle]; changed {
			t.Fatal("child title replaced root continuation")
		}
	}
}
