package transcriptparse_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These checks exercise the model contract on every research input. They do not
// replace each fixture's native semantic assertions or the corpus differential.
func TestResearchModelConsistency(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "grok"} {
		root := os.DirFS(provider + "/testdata/research")
		entries, err := fs.ReadDir(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			fixture, err := loadResearchCase(root, entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			inputs := fixture.Inputs
			limit := 32 << 20
			if len(fixture.Recipes) > 0 {
				var recipe struct {
					PayloadBytes int `json:"payloadBytes"`
				}
				if provider != "grok" || entry.Name() != "oversized-record" || json.Unmarshal(fixture.Recipes["input.recipe.json"], &recipe) != nil || recipe.PayloadBytes != 1048577 {
					t.Fatal("new recipe needs an explicit constructor")
				}
				limit = 1 << 20
				row, _ := json.Marshal(map[string]any{"type": "agent_message_chunk", "text": strings.Repeat("x", recipe.PayloadBytes)})
				inputs["recipe.jsonl"] = append(row, '\n')
			}
			names := make([]string, 0, len(inputs))
			for name := range inputs {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				whole := strings.HasSuffix(name, ".json")
				// Pairing and mutation descriptions are fixture instructions, not
				// native inputs. Grok's JSON documents are summaries/sidecars.
				if !strings.HasSuffix(name, ".jsonl") && !(provider == "grok" && whole) {
					continue
				}
				modes := []string{"file", "wire"}
				if whole {
					modes = []string{"file"}
				}
				for _, mode := range modes {
					t.Run(provider+"/"+entry.Name()+"/"+name+"/"+mode, func(t *testing.T) {
						captured, observed := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
						input := model.Input{Bytes: inputs[name], Source: model.SourceRef{Label: entry.Name() + "/" + name, Incarnation: "fixture"}, Scope: model.Scope{ConnectionID: "fixture-connection"}, MaxRowBytes: limit, Closed: true, CaptureAt: &captured, ObservedAt: &observed}
						if whole {
							input.Format = "json"
						}
						checkModelConsistency(t, parseFixture(t, provider, mode, input))
					})
				}
			}
		}
	}
}

func parseFixture(t *testing.T, provider, mode string, input model.Input) model.Batch {
	t.Helper()
	var claudeState claude.State
	var codexState codex.State
	var grokState grok.State
	var combined model.Batch
	chunks := [][]byte{input.Bytes}
	if mode == "wire" {
		chunks = bytes.Split(bytes.TrimSuffix(input.Bytes, []byte{'\n'}), []byte{'\n'})
	}
	for _, chunk := range chunks {
		if mode == "wire" && len(bytes.TrimSpace(chunk)) == 0 {
			continue
		}
		input.Bytes = chunk
		var batch model.Batch
		var err error
		switch provider {
		case "claude":
			if mode == "file" {
				claudeState, batch, err = claude.ParseFile(claudeState, input)
			} else {
				claudeState, batch, err = claude.ParseWire(claudeState, input)
			}
		case "codex":
			if mode == "file" {
				codexState, batch, err = codex.ParseFile(codexState, input)
			} else {
				codexState, batch, err = codex.ParseWire(codexState, input)
			}
		case "grok":
			if mode == "file" {
				grokState, batch, err = grok.ParseFile(grokState, input)
			} else {
				grokState, batch, err = grok.ParseWire(grokState, input)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if combined.Versions.Schema != "" && combined.Versions != batch.Versions {
			t.Fatal("reader versions changed within a source")
		}
		combined.Versions = batch.Versions
		combined.Changes = append(combined.Changes, batch.Changes...)
		combined.Observations = append(combined.Observations, batch.Observations...)
		combined.Records = append(combined.Records, batch.Records...)
		combined.Diagnostics = append(combined.Diagnostics, batch.Diagnostics...)
	}
	for _, state := range []any{claudeState, codexState, grokState, combined} {
		if _, err := json.Marshal(state); err != nil {
			t.Fatalf("result/checkpoint cannot serialize: %v", err)
		}
	}
	return combined
}

func checkModelConsistency(t *testing.T, batch model.Batch) {
	t.Helper()
	observations := map[string]model.Observation{}
	items := map[string]model.Item{}
	turns := map[string]model.Turn{}
	positions := map[int64]string{}
	physical := map[model.SourceRef]bool{}
	rowSource := func(source model.SourceRef) model.SourceRef {
		source.BlockPath, source.SHA256, source.Direction = "", "", ""
		return source
	}
	var relations []model.Relation
	for _, obs := range batch.Observations {
		if obs.ID == "" {
			t.Fatal("observation has no identity")
		}
		if _, exists := observations[obs.ID]; exists {
			t.Fatalf("observation ID emitted more than once: %s", obs.ID)
		}
		observations[obs.ID] = obs
		physical[rowSource(obs.Source)] = true
	}
	for _, record := range batch.Records {
		if record.Disposition == "" || record.Capture == "" {
			t.Fatal("source record lacks its disposition or capture state")
		}
		if record.Disposition == model.RecordSemantic && !physical[rowSource(record.Source)] {
			t.Fatal("semantic record has no emitted source evidence")
		}
	}
	for _, change := range batch.Changes {
		if item := change.Item; item != nil {
			if item.ID == "" || item.SourceSeq <= 0 || item.Scope.ItemID != item.ID || item.Scope.TurnKey != item.TurnKey {
				t.Fatal("item identity/scope/position is inconsistent")
			}
			if prior, ok := items[item.ID]; ok && prior.SourceSeq != item.SourceSeq {
				t.Fatal("an item revision moved its source position")
			}
			if id, exists := positions[item.SourceSeq]; exists && id != item.ID {
				t.Fatal("different items share a source position")
			}
			positions[item.SourceSeq], items[item.ID] = item.ID, *item
		}
		if turn := change.Turn; turn != nil {
			if turn.Key == "" || turn.Scope.TurnKey != turn.Key || turn.Ordinal <= 0 {
				t.Fatal("turn identity/scope/ordinal is inconsistent")
			}
			turns[turn.Key] = *turn
		}
		if change.Relation != nil {
			relations = append(relations, *change.Relation)
		}
	}
	refs := func(values []string) {
		t.Helper()
		for _, ref := range values {
			if _, ok := observations[ref]; !ok {
				t.Fatalf("reference has no emitted observation: %s", ref)
			}
		}
	}
	clocks := func(times []model.TimeObservation, scope model.Scope) {
		t.Helper()
		for _, stamp := range times {
			if !reflect.DeepEqual(stamp.Scope, scope) {
				t.Fatal("clock scope differs from its observation")
			}
			refs([]string{stamp.ObservationRef})
		}
	}
	for _, obs := range observations {
		if obs.Scope.Provider == "" || obs.Scope.Source == "" || obs.Scope.Incarnation == "" || obs.Scope.Kind == "" {
			t.Fatal("observation scope is incomplete")
		}
		clocks(obs.Times, obs.Scope)
		if obs.Scope.ItemID != "" {
			if _, ok := items[obs.Scope.ItemID]; !ok {
				t.Fatal("observation owner item was not emitted")
			}
		}
		if obs.Scope.TurnKey != "" {
			if _, ok := turns[obs.Scope.TurnKey]; !ok {
				t.Fatal("observation owner turn was not emitted")
			}
		}
	}
	owned := func(values []model.Observation) {
		t.Helper()
		for _, obs := range values {
			if emitted, ok := observations[obs.ID]; !ok || !reflect.DeepEqual(emitted, obs) {
				t.Fatal("stored observation differs from emitted evidence")
			}
		}
	}
	for _, item := range items {
		if item.TurnKey != "" {
			if _, ok := turns[item.TurnKey]; !ok {
				t.Fatal("item turn was not emitted")
			}
		}
		owned(item.Observations)
		refs(item.Finality.ObservationRefs)
		for _, alias := range item.Aliases {
			refs(alias.ObservationRefs)
		}
		for _, stamp := range item.Times {
			refs([]string{stamp.ObservationRef})
		}
		if item.IsFinalAnswer != (item.Finality.State == model.FinalityFinal) {
			t.Fatal("final-answer convenience flag disagrees with finality")
		}
		relations = append(relations, item.Relations...)
	}
	for _, turn := range turns {
		owned(turn.Observations)
		for _, fact := range turn.Completion {
			refs(fact.ObservationRefs)
		}
		for _, usage := range turn.UsageObservations {
			refs(usage.ObservationRefs)
		}
		for _, stamp := range turn.Times {
			refs([]string{stamp.ObservationRef})
		}
		for _, duration := range turn.Durations {
			refs([]string{duration.ObservationRef})
		}
		for _, ref := range turn.FinalItemRefs {
			if _, ok := items[ref]; !ok {
				t.Fatal("final item was not emitted")
			}
		}
	}
	for _, relation := range relations {
		refs(relation.ObservationRefs)
		if relation.FromObservation != "" {
			refs([]string{relation.FromObservation})
		}
		if relation.ToObservation != "" {
			refs([]string{relation.ToObservation})
		}
		if relation.FromItem != "" {
			if _, ok := items[relation.FromItem]; !ok {
				t.Fatal("relation source item was not emitted")
			}
		}
		if relation.ToItem != "" {
			if _, ok := items[relation.ToItem]; !ok {
				t.Fatal("relation target item was not emitted")
			}
		}
	}
}
