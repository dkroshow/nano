package claude

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeUsageRevisionsRetainCounterCoverage(t *testing.T) {
	state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, "usage-message-dedup-and-missing-fields")))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if turn.Usage == nil || *turn.Usage.Input != 5 || *turn.Usage.Output != 6 || *turn.Usage.CacheRead != 10 || *turn.Usage.CacheWrite != 5 || *turn.Usage.Reasoning != 1 || turn.Usage.Total != nil {
			t.Fatal("message usage snapshots added repeatedly or invented totals")
		}
		if len(turn.UsageObservations) != 3 || len(turn.UsageRefs) != 2 || turn.UsageCoverage["cacheRead"] != (model.CounterCoverage{Reported: 1, Samples: 2}) {
			t.Fatal("partial native counter coverage lost")
		}
	}
}

func TestClaudeUsageReferencesHaveScopedEvidence(t *testing.T) {
	state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, "usage-message-dedup-and-missing-fields")))
	if err != nil {
		t.Fatal(err)
	}
	evidence := map[string]model.Observation{}
	for _, obs := range batch.Observations {
		evidence[obs.ID] = obs
	}
	for _, turn := range state.Turns {
		for _, usage := range turn.UsageObservations {
			for _, ref := range usage.ObservationRefs {
				obs, exists := evidence[ref]
				if !exists || obs.Scope.TurnKey != turn.Key {
					t.Fatal("usage refers to missing or unscoped evidence")
				}
				for _, stamp := range obs.Times {
					if stamp.Scope.TurnKey != turn.Key || stamp.ObservationRef != obs.ID {
						t.Fatal("usage clock borrowed another observation scope")
					}
				}
			}
		}
	}
}

func TestClaudeUsageInvalidCountersRemainUnknown(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"a","message":{"id":"m","content":[{"type":"text","text":"A"}],"usage":{"input_tokens":null,"output_tokens":1.5,"cache_read_input_tokens":-1,"cache_creation_input_tokens":"3","output_tokens_details":{"thinking_tokens":9223372036854775808}}}}
`)
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range batch.Diagnostics {
		if d.Code == "invalid_usage_counter" {
			found = true
		}
	}
	if !found {
		t.Fatal("invalid native counters accepted silently")
	}
	for _, turn := range state.Turns {
		if len(turn.UsageObservations) != 1 {
			t.Fatal("invalid usage evidence dropped")
		}
		values := turn.UsageObservations[0].Values
		if values.Input != nil || values.Output != nil || values.CacheRead != nil || values.CacheWrite != nil || values.Reasoning != nil || len(values.Native) == 0 {
			t.Fatal("invalid counters became values or lost native evidence")
		}
	}
}

func TestClaudeResultUsageKeepsModelBucketsSeparate(t *testing.T) {
	state, _, err := parseWireView(parsedView{}, inputFor(fixture(t, "wire-result-usage-grain")))
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range state.Turns {
		if turn.Usage != nil || len(turn.UsageObservations) != 1 {
			t.Fatal("result aggregate became an additive message contribution")
		}
		obs := turn.UsageObservations[0]
		bucket := obs.ModelUsage["claude-example"]
		if obs.Grain != model.UsageTurn || obs.Semantics != "result_aggregate" || obs.Values.Input == nil || *obs.Values.Input != 5 || bucket.Input == nil || *bucket.Input != 5 || bucket.Output == nil || *bucket.Output != 6 {
			t.Fatal("native result/model usage lost")
		}
		var native map[string]json.RawMessage
		if json.Unmarshal(obs.Values.Native, &native) != nil {
			t.Fatal("native result counters not retained")
		}
	}
}
