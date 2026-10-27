package grok

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These are real merged saved rows from the exact keyed-block probe, with
// neutral text. The prompt alias and final reply event remain exact.
func TestGrokSavedSubmissionKeyAndLastChunk(t *testing.T) {
	raw, err := os.ReadFile("../../app/testdata/live_text/grok-turns.saved.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := parseFileView(parsedView{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{
		"11111111-aaaa-4aaa-8aaa-000000000001": false,
		"11111111-aaaa-4aaa-8aaa-000000000002": false,
		"11111111-aaaa-4aaa-8aaa-000000000003": false,
	}
	wantReplies := map[string]string{"First reply.": "f3fa6aa0-a137-5782-bc95-1dd0f1ea92b1-1284249", "Cut reply.": "f3fa6aa0-a137-5782-bc95-1dd0f1ea92b1-1284564", "Next reply.": "f3fa6aa0-a137-5782-bc95-1dd0f1ea92b1-1284568"}
	replies := 0
	for _, item := range state.Items {
		if item.Type == model.ItemUserMessage {
			found := false
			for _, a := range item.Aliases {
				var key string
				if a.Namespace == "nano.submissionId" && a.Authority == model.BasisNative && json.Unmarshal(a.Value, &key) == nil {
					if _, ok := wantKeys[key]; !ok || len(a.ObservationRefs) != 1 {
						t.Fatalf("unexpected submission alias %+v", a)
					}
					wantKeys[key] = true
					found = true
				}
			}
			if !found {
				t.Errorf("saved prompt %q has no exact Nano submission alias", item.Text)
			}
		}
		if item.Type == model.ItemAgentMessage {
			replies++
			if len(item.Observations) == 0 || item.Observations[0].NativeEventID != wantReplies[item.Text] {
				t.Errorf("merged reply %q does not carry its captured last-chunk event: %+v", item.Text, item.Observations)
			}
		}
	}
	for key, found := range wantKeys {
		if !found {
			t.Errorf("submission key %s not indexed", key)
		}
	}
	if replies != 3 {
		t.Fatalf("merged replies=%d want 3", replies)
	}
}
