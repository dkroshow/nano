package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexSettingsApplyToNextTaskWithoutContext(t *testing.T) {
	rows := []string{
		`{"type":"session_meta","payload":{"id":"session"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"first"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"first"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"second"}}`,
		`{"type":"turn_context","payload":{"turn_id":"second","service_tier":"default"}}`,
	}
	var state State
	turns := map[string]model.Turn{}
	for _, row := range rows {
		var batch model.Batch
		var err error
		state, batch, err = ParseFile(state, inputFor([]byte(row+"\n")))
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range batch.Changes {
			if change.Turn != nil {
				turns[change.Turn.NativeTurnID] = *change.Turn
			}
		}
		checkpoint, _ := json.Marshal(state)
		state = State{}
		if err := json.Unmarshal(checkpoint, &state); err != nil {
			t.Fatal(err)
		}
	}
	if turns["first"].ServiceTier != "priority" || turns["second"].ServiceTier != "default" {
		t.Fatalf("pending setting lost or leaked: first=%q second=%q", turns["first"].ServiceTier, turns["second"].ServiceTier)
	}
	if len(state.PendingSettings) != 0 {
		t.Fatal("applied setting remained pending")
	}
}

func TestFileExplicitServiceTierSurvivesContextAndRestore(t *testing.T) {
	raw, err := os.ReadFile("testdata/effective-service-tier.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var state State
	for i, row := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		state, _, err = ParseFile(state, inputFor(append(append([]byte(nil), row...), '\n')))
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 || i == 4 || i == 5 {
			id, want := "first", "priority"
			if i >= 4 {
				id = "second"
			}
			if i == 4 {
				want = "default"
			}
			found := false
			for _, turn := range state.Turns {
				if turn.NativeTurnID == id {
					found = true
					if turn.ServiceTier != want {
						t.Fatalf("row %d: tier=%s want=%s", i+1, turn.ServiceTier, want)
					}
				}
			}
			if !found {
				t.Fatal("native turn missing")
			}
		}
		saved, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		state = State{}
		if err := json.Unmarshal(saved, &state); err != nil {
			t.Fatal(err)
		}
	}
}
