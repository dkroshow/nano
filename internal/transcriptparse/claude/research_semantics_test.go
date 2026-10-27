package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeResearchSourceReplacementAndBudget(t *testing.T) {
	var recipe struct {
		Operations []struct {
			Operation    string            `json:"operation"`
			AfterRows    []json.RawMessage `json:"afterRows"`
			MaxLineBytes int               `json:"maxLineBytes"`
		} `json:"operations"`
	}
	raw := fixture(t, "rewrite-and-size-budget")
	readResearchRecipe(t, "rewrite-and-size-budget/mutations.json", &recipe)
	before, _, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	old := orderedItems(before)[0]
	for _, op := range recipe.Operations {
		switch op.Operation {
		case "replace_source":
			var rows []byte
			for _, row := range op.AfterRows {
				var compact bytes.Buffer
				if err := json.Compact(&compact, row); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, compact.Bytes()...)
				rows = append(rows, '\n')
			}
			input := inputFor(rows)
			input.Source.Incarnation = "replacement"
			if _, _, err := parseFileView(before, input); err == nil {
				t.Fatal("replacement silently reused old checkpoint")
			}
			after, _, err := parseFileView(parsedView{}, input)
			if err != nil {
				t.Fatal(err)
			}
			current := orderedItems(after)[0]
			if old.ID == current.ID || current.SourceLine != 2 || current.Text != old.Text {
				t.Fatal("replacement reused source identity or lost content")
			}
			for _, item := range []model.Item{old, current} {
				found := false
				for _, alias := range item.Aliases {
					if alias.Namespace == "claude.rowUUID" && string(alias.Value) == `"u1"` {
						found = true
					}
				}
				if !found {
					t.Fatal("native row alias did not survive replacement")
				}
			}
		case "apply_budget":
			input := inputFor(raw)
			input.MaxRowBytes = op.MaxLineBytes
			state, batch, err := parseFileView(parsedView{}, input)
			if err != nil || len(state.Items) != 0 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "row_too_large" || batch.Records[0].Capture != model.CaptureTruncated {
				t.Fatal("source budget lost explicit truncation")
			}
		default:
			t.Fatalf("new mutation needs assertions: %s", op.Operation)
		}
	}
}

func TestClaudeResearchOrphanEvidenceSurvives(t *testing.T) {
	var state parsedView
	var observations []model.Observation
	for _, row := range bytes.Split(bytes.TrimSpace(fixture(t, "wire-orphan-delta-and-truncation")), []byte{'\n'}) {
		var batch model.Batch
		var err error
		state, batch, err = parseWireView(state, inputFor(row))
		if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, envelopeObservations(batch)...)
	}
	if len(state.Items) != 0 || len(state.Turns) != 0 || len(observations) != 2 {
		t.Fatal("orphan text or stop/usage evidence vanished or acquired a guessed owner")
	}
	for _, obs := range observations {
		if len(obs.Detail) == 0 || obs.Scope.TurnKey != "" {
			t.Fatal("orphan native detail/scope lost")
		}
	}
}

func TestClaudeResearchTerminalOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome model.Outcome
	}{{"terminal-completed", model.OutcomeCompleted}, {"terminal-failed", model.OutcomeFailed}, {"terminal-interrupted", model.OutcomeInterrupted}, {"max-tokens-then-duration", model.OutcomeTruncated}} {
		state, _, err := parseFileView(parsedView{}, inputFor(fixture(t, tc.name)))
		if err != nil || len(state.Turns) != 1 {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, turn := range state.Turns {
			if turn.Outcome != tc.outcome {
				t.Fatalf("%s: %s", tc.name, turn.Outcome)
			}
			if tc.outcome != model.OutcomeCompleted && len(turn.FinalItemRefs) != 0 {
				t.Fatal("adverse terminal designated a final answer")
			}
		}
	}
}

func TestClaudeResearchUnknownServerResultStaysUnverified(t *testing.T) {
	state, batch, err := parseFileView(parsedView{}, inputFor(fixture(t, "redacted-thinking-server-tool")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 3 || items[0].Parts[0].Availability != model.AvailableRedacted || items[0].Text != "" || items[2].Type != model.ItemProviderEvent || items[2].Outcome != model.OutcomeUnknown || len(items[2].Relations) != 0 || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "unknown_content_block" {
		t.Fatal("unobserved server result acquired speculative execution or linkage")
	}
	var detail map[string]json.RawMessage
	if json.Unmarshal(items[2].Parts[0].Detail, &detail) != nil || !reflect.DeepEqual(detail["tool_use_id"], json.RawMessage(`"server-call"`)) {
		t.Fatal("unknown native call detail lost")
	}
}

func readResearchRecipe(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/research/" + path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
