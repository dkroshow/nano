package claude

import (
	"bytes"
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestStreamToolSnapshotUsesItsKnownBlock(t *testing.T) {
	raw, err := os.ReadFile("testdata/stream-tool-snapshot.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var state State
	items := map[string]model.Item{}
	for _, frame := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var batch model.Batch
		state, batch, err = ParseWire(state, inputFor(frame))
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range batch.Changes {
			if change.Item != nil {
				items[change.Item.NativeItemID] = *change.Item
			}
		}
	}
	text, call := items["message:0"], items["message:1"]
	if len(items) != 2 || text.Text != "Hello" || text.Type != model.ItemAgentMessage || call.CallID != "call" || call.ToolName != "Read" || call.Parts[0].Text != `{"path":"README.md"}` {
		t.Fatal("tool-only snapshot overwrote an earlier text block or lost its invocation")
	}
}

func TestStreamToolSnapshotRejectsAmbiguousBlockIdentity(t *testing.T) {
	var state State
	for _, frame := range []string{
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"message"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"same","name":"Read","input":{"path":"a"}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"same","name":"Read","input":{"path":"b"}}}}`,
	} {
		var err error
		state, _, err = ParseWire(state, inputFor([]byte(frame)))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, batch, err := ParseWire(state, inputFor([]byte(`{"type":"assistant","message":{"id":"message","content":[{"type":"tool_use","id":"same","name":"Read","input":{"path":"unproven"}}]}}`)))
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range batch.Changes {
		if change.Item != nil {
			t.Fatal("ambiguous call ID revised an arbitrary streamed block")
		}
	}
	for _, diagnostic := range batch.Diagnostics {
		if diagnostic.Code == "snapshot_block_identity_conflict" {
			return
		}
	}
	t.Fatal("ambiguous call identity was not diagnosed")
}
