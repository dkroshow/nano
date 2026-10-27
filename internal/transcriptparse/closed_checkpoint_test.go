package transcriptparse_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClosedFileCheckpointRetainsPositionWithoutOutput(t *testing.T) {
	text := strings.Repeat("closed-source-content-", 4096)
	encoded, _ := json.Marshal(text)
	for _, provider := range []string{"claude", "codex", "grok"} {
		t.Run(provider, func(t *testing.T) {
			parse := closedCheckpointReader(provider)
			rows := map[string]string{
				"claude": `{"type":"user","uuid":"event","message":{"content":` + string(encoded) + `}}`,
				"codex":  `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(encoded) + `}]}}`,
				"grok":   `{"method":"session/update","params":{"sessionId":"session","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":` + string(encoded) + `}},"_meta":{"eventId":"event"}}}`,
			}
			input := model.Input{Source: model.SourceRef{Label: "source", Incarnation: "1"}, Bytes: []byte(rows[provider] + "\n")}
			batch, _, err := parse(input)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, change := range batch.Changes {
				if change.Item != nil && change.Item.Text == text {
					found = true
				}
			}
			if !found {
				t.Fatal("source content was not emitted before closing")
			}
			position := len(input.Bytes)
			input.Bytes, input.Closed = nil, true
			closed, checkpoint, err := parse(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(checkpoint) > 4096 || bytes.Contains(checkpoint, []byte("closed-source-content")) {
				t.Fatalf("closed checkpoint retains output (%d bytes)", len(checkpoint))
			}
			if len(closed.Changes) != 0 {
				t.Fatal("closing a byte source invented content or completion")
			}
			var header struct {
				Framing struct {
					Checkpoint int
					Closed     bool
				}
			}
			if json.Unmarshal(checkpoint, &header) != nil || !header.Framing.Closed || header.Framing.Checkpoint != position {
				t.Fatal("closed source position was lost")
			}
			input.Bytes, input.Closed = []byte("\n"), false
			if _, _, err = parse(input); err == nil {
				t.Fatal("closed incarnation accepted appended bytes")
			}
		})
	}
}

func TestClosedGrokDocumentCheckpointDoesNotRetainCompactionBody(t *testing.T) {
	text, _ := json.Marshal(strings.Repeat("retained-in-output-", 4096))
	input := model.Input{Source: model.SourceRef{Label: "document", Incarnation: "1"}, Format: "json", Closed: true, Bytes: []byte(`{"schema_version":2,"request_id":"request","trigger":"manual","chat_history":[],"summary":` + string(text) + `}`)}
	batch, checkpoint, err := closedCheckpointReader("grok")(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Changes) == 0 {
		t.Fatal("compaction output was not emitted")
	}
	if len(checkpoint) > 4096 || bytes.Contains(checkpoint, []byte("retained-in-output")) {
		t.Fatalf("closed document retains output (%d bytes)", len(checkpoint))
	}
}

// Each call restores only the reader checkpoint. Output remains in the test's
// batches and never travels back through this continuation boundary.
func closedCheckpointReader(provider string) func(model.Input) (model.Batch, []byte, error) {
	return checkpointReader(provider, false)
}

func checkpointReader(provider string, wire bool) func(model.Input) (model.Batch, []byte, error) {
	var saved []byte
	return func(input model.Input) (model.Batch, []byte, error) {
		var batch model.Batch
		var state any
		var err error
		switch provider {
		case "claude":
			var prior claude.State
			if saved != nil {
				if err = json.Unmarshal(saved, &prior); err != nil {
					return batch, nil, err
				}
			}
			var next claude.State
			parse := claude.ParseFile
			if wire {
				parse = claude.ParseWire
			}
			next, batch, err = parse(prior, input)
			state = next
		case "codex":
			var prior codex.State
			if saved != nil {
				if err = json.Unmarshal(saved, &prior); err != nil {
					return batch, nil, err
				}
			}
			var next codex.State
			parse := codex.ParseFile
			if wire {
				parse = codex.ParseWire
			}
			next, batch, err = parse(prior, input)
			state = next
		case "grok":
			var prior grok.State
			if saved != nil {
				if err = json.Unmarshal(saved, &prior); err != nil {
					return batch, nil, err
				}
			}
			var next grok.State
			parse := grok.ParseFile
			if wire {
				parse = grok.ParseWire
			}
			next, batch, err = parse(prior, input)
			state = next
		}
		if err != nil {
			return batch, nil, err
		}
		saved, err = json.Marshal(state)
		return batch, saved, err
	}
}
