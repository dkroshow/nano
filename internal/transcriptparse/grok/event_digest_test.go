package grok

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestGrokEventDigestSurvivesEvictionAndClockChange(t *testing.T) {
	raw, err := os.ReadFile("testdata/late-replay-after-eviction.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []bool{false, true} {
		var state State
		var firstDigest, firstID string
		for i, frame := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
			input := model.Input{Bytes: frame, Source: model.SourceRef{Label: "fixture", Incarnation: "1"}}
			var batch model.Batch
			if wire {
				state, batch, err = ParseWire(state, input)
			} else {
				input.Bytes = append(append([]byte(nil), frame...), '\n')
				state, batch, err = ParseFile(state, input)
			}
			if err != nil {
				t.Fatal(err)
			}
			if state.Version != batch.Versions.Checkpoint {
				t.Fatal("advertised and serialized checkpoint versions differ")
			}
			for _, change := range batch.Changes {
				if change.Item == nil {
					continue
				}
				for _, observation := range change.Item.Observations {
					if observation.NativeEventID != "e2" {
						continue
					}
					if firstDigest == "" {
						firstDigest, firstID = observation.NativeEventDigest, change.Item.ID
					}
					decoded, err := hex.DecodeString(observation.NativeEventDigest)
					if err != nil || len(decoded) != 32 || firstDigest != observation.NativeEventDigest {
						t.Fatal("native replay has no stable content digest")
					}
					if i == 4 && (change.Item.ID == firstID || len(change.Item.Relations) != 0) {
						t.Fatal("reader claimed an evicted event without retained evidence")
					}
				}
			}
			if i == 3 {
				checkpoint, err := json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
				state = State{}
				if err := json.Unmarshal(checkpoint, &state); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestGrokEventDigestKeepsExactPayloadAndClassification(t *testing.T) {
	base := decodedUpdate{Kind: kindAgentMessage, Text: "A", Payload: []byte(`{"a":9007199254740992,"b":"A"}`)}
	changed := base
	changed.Payload = []byte(`{"b":"A","a":9007199254740992}`)
	if updateEventDigest(base) != updateEventDigest(changed) {
		t.Fatal("JSON member order changed event equality")
	}
	changed.Payload = []byte(`{"a":9007199254740993,"b":"A"}`)
	if updateEventDigest(base) == updateEventDigest(changed) {
		t.Fatal("event digest rounded a native integer")
	}
	changed = base
	changed.Status = "failed"
	if updateEventDigest(base) == updateEventDigest(changed) {
		t.Fatal("native status change retained event equality")
	}
}
