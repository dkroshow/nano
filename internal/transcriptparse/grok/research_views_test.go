package grok

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These six-field assertions are one conformance layer. Detailed provider
// tests cover native clocks, parts, replay, usage and source relationships.
type researchItemView struct {
	Type         model.ItemType
	Role         model.Role
	Text         string
	Phase        string
	Outcome      model.Outcome
	NativeTurnID string
}

type researchViewDifference struct {
	Name  string
	Views []researchItemView
}

func TestGrokResearchItemViews(t *testing.T) {
	entries, err := os.ReadDir("testdata/research")
	if err != nil {
		t.Fatal(err)
	}
	scenarios := map[string]string{
		"oversized-record":                  "TestFileReaderTailMalformedAndOversizeRecover",
		"paired-repo-parent-child":          "TestGrokGenuineWrappedCaptureRetainsScopedEvidence",
		"paired-segments-tools-late-prompt": "TestPairedLatePromptAndCallBinding",
		"summary-rewrite-lineage-conflict":  "TestGrokSnapshotLineageConflictsStayExplicit",
	}
	differences := grokResearchViewDifferences()
	tested := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if scenario := scenarios[name]; scenario != "" {
			t.Logf("%s uses scenario assertions in %s", name, scenario)
			continue
		}
		tested++
		t.Run(name, func(t *testing.T) {
			var draft struct {
				Items []struct {
					Kind    string     `json:"kind"`
					Role    model.Role `json:"role"`
					Text    string     `json:"text"`
					Phase   string     `json:"nativePhase"`
					Outcome string     `json:"executionOutcome"`
					TurnRef struct {
						NativeTurnID string `json:"nativeTurnId"`
					} `json:"turnRef"`
				}
			}
			readJSON(t, "testdata/research/"+name+"/expected.json", &draft)
			want := []researchItemView{}
			for _, item := range draft.Items {
				typ := model.ItemType(item.Kind)
				switch item.Kind {
				case "message":
					typ = model.ItemAgentMessage
					if item.Role == model.RoleUser {
						typ = model.ItemUserMessage
					}
				case "system":
					typ = model.ItemNotice
				case "unknown":
					typ = model.ItemProviderEvent
				}
				outcome := model.Outcome(item.Outcome)
				switch item.Outcome {
				case "", "unknown":
					outcome = model.OutcomeUnknown
				case "success":
					outcome = model.OutcomeCompleted
				case "failure":
					outcome = model.OutcomeFailed
				}
				want = append(want, researchItemView{typ, item.Role, item.Text, item.Phase, outcome, item.TurnRef.NativeTurnID})
			}
			if difference, exists := differences[name]; exists {
				if difference.Name == "" {
					t.Fatal("expected change has no named difference")
				}
				want = append([]researchItemView{}, difference.Views...)
				t.Log(difference.Name)
			}
			raw, err := os.ReadFile("testdata/research/" + name + "/input.jsonl")
			format := "jsonl"
			if os.IsNotExist(err) {
				raw, err = os.ReadFile("testdata/research/" + name + "/input.json")
				format = "json"
			}
			if err != nil {
				t.Fatal(err)
			}
			input := inputBytes(raw)
			input.Format, input.Closed = format, true
			state, _, err := parseFileView(parsedView{}, input)
			if err != nil {
				t.Fatal(err)
			}
			got := []researchItemView{}
			for _, item := range controlItems(state) {
				turn := ""
				if item.Scope.NativeTurnID != nil {
					turn = *item.Scope.NativeTurnID
				}
				got = append(got, researchItemView{item.Type, item.Role, item.Text, item.Phase, item.Outcome, turn})
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("item view differs\nwant: %+v\ngot:  %+v", want, got)
			}
		})
	}
	if tested != 52 {
		t.Fatalf("review fixture coverage: got %d single-input cases", tested)
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func grokResearchViewDifferences() map[string]researchViewDifference {
	view := func(typ model.ItemType, role model.Role, text string, outcome model.Outcome, turn string) researchItemView {
		return researchItemView{typ, role, text, "", outcome, turn}
	}
	d := map[string]researchViewDifference{}
	// These exact case lists migrate empty draft placeholders to observations.
	for _, name := range []string{"billing-variants", "capture-events", "capture-hidden-user", "capture-session-update-current-mode-update", "capture-x-ai-session-update-hook-execution", "capture-x-ai-session-update-retry-state", "capture-terminal-cancelled", "capture-x-ai-session-update-turn-completed", "handshake-queue-responses-errors", "incomplete-usage", "partial-line-and-malformed"} {
		d[name] = researchViewDifference{"GROK-OBSERVATION-1", nil}
	}
	for _, name := range []string{"terminal-cancelled", "terminal-error", "terminal-interrupted", "terminal-missing-reason"} {
		d[name] = researchViewDifference{"GROK-OBSERVATION-1", []researchItemView{view(model.ItemUserMessage, model.RoleUser, "Check.", model.OutcomeUnknown, "p1")}}
	}
	d["unknown-and-nontext"] = researchViewDifference{"GROK-OBSERVATION-1", []researchItemView{view(model.ItemAgentMessage, model.RoleAssistant, "", model.OutcomeUnknown, "p1")}}
	d["unknown-coordination-and-message-image"] = d["unknown-and-nontext"]
	d["capture-chat-history"] = researchViewDifference{"GROK-AUX-1", []researchItemView{
		view(model.ItemNotice, model.RoleSystem, "[text]", model.OutcomeUnknown, ""), view(model.ItemUserMessage, model.RoleUser, "[text]", model.OutcomeUnknown, ""), view(model.ItemReasoning, model.RoleAssistant, "[text]", model.OutcomeUnknown, ""), view(model.ItemAgentMessage, model.RoleAssistant, "[text]", model.OutcomeUnknown, ""),
		view(model.ItemToolCall, model.RoleAssistant, "[text]", model.OutcomeUnknown, ""), view(model.ItemToolCall, model.RoleAssistant, "[text]", model.OutcomeUnknown, ""), view(model.ItemToolCall, model.RoleAssistant, "[text]", model.OutcomeUnknown, ""), view(model.ItemToolResult, model.RoleTool, "[text]", model.OutcomeUnknown, ""), view(model.ItemToolCall, model.RoleAssistant, "", model.OutcomeCompleted, ""),
	}}
	d["capture-compaction-checkpoints"] = researchViewDifference{"GROK-COMPACTION-1", []researchItemView{{model.ItemCompaction, model.RoleSystem, "", "checkpoint_saved", model.OutcomeUnknown, ""}}}
	d["capture-compaction-requests"] = researchViewDifference{"GROK-COMPACTION-1", []researchItemView{{model.ItemCompaction, model.RoleSystem, "[text]", "requested", model.OutcomeUnknown, ""}}}
	d["capture-session-update-plan"] = researchViewDifference{"GROK-CONTENT-1", []researchItemView{view(model.ItemPlan, model.RoleAssistant, "[text]\n[text]\n[text]", model.OutcomeUnknown, "id-4")}}
	d["capture-session-update-tool-call"] = researchViewDifference{"GROK-TOOL-1", []researchItemView{view(model.ItemToolCall, model.RoleAssistant, "[text]", model.OutcomeRunning, "id-4")}}
	d["capture-session-update-tool-call-update"] = researchViewDifference{"GROK-TOOL-1", []researchItemView{view(model.ItemToolCall, model.RoleAssistant, "[text]", model.OutcomeUnknown, "id-4")}}
	d["capture-tool-content-diff"] = researchViewDifference{"GROK-CONTENT-1", []researchItemView{view(model.ItemToolCall, model.RoleAssistant, "[text]", model.OutcomeUnknown, "id-848")}}
	d["capture-x-ai-session-update-subagent-spawned"] = researchViewDifference{"GROK-COORDINATION-1", []researchItemView{view(model.ItemNotice, model.RoleSystem, "[text]", model.OutcomeUnknown, "")}}
	d["capture-x-ai-session-update-subagent-finished"] = researchViewDifference{"GROK-COORDINATION-1", []researchItemView{view(model.ItemNotice, model.RoleSystem, "", model.OutcomeUnknown, "")}}
	d["capture-x-ai-session-update-task-backgrounded"] = researchViewDifference{"GROK-TASK-1", []researchItemView{view(model.ItemCommand, model.RoleSystem, "[text]", model.OutcomeUnknown, "")}}
	for _, name := range []string{"capture-x-ai-session-update-task-completed", "task-truncated-output-after-turn"} {
		d[name] = researchViewDifference{"GROK-TASK-1", []researchItemView{view(model.ItemCommand, model.RoleSystem, "[text]", model.OutcomeCompleted, "")}}
	}
	d["missing-identity-and-time"] = researchViewDifference{"GROK-SEGMENT-1", []researchItemView{view(model.ItemAgentMessage, model.RoleAssistant, "No native clock.Second precision only.", model.OutcomeUnknown, "")}}
	d["replay-straddles-load"] = researchViewDifference{"GROK-REPLAY-2", nil}
	d["reverse-requests"] = researchViewDifference{"GROK-INTERACTION-1", []researchItemView{view(model.ItemInteraction, model.RoleSystem, "Read example", model.OutcomeCompleted, ""), view(model.ItemInteraction, model.RoleSystem, "Read and report.", model.OutcomeUnknown, ""), view(model.ItemInteraction, model.RoleSystem, "Choose a color", model.OutcomeCompleted, "")}}
	d["synthetic-continuation-scope"] = researchViewDifference{"GROK-SCOPE-2", []researchItemView{view(model.ItemUserMessage, model.RoleUser, "Operator request.", model.OutcomeUnknown, "p1"), view(model.ItemAgentMessage, model.RoleAssistant, "Worker finished.", model.OutcomeUnknown, "subagent-completed-child1"), view(model.ItemAgentMessage, model.RoleAssistant, "Operator answer.", model.OutcomeUnknown, "p1")}}
	d["tool-status-synonyms"] = researchViewDifference{"GROK-END-1", []researchItemView{view(model.ItemToolResult, model.RoleTool, "", model.OutcomeCompleted, "p1"), view(model.ItemToolResult, model.RoleTool, "", model.OutcomeFailed, "p1"), view(model.ItemToolResult, model.RoleTool, "", model.OutcomeCancelled, "p1"), view(model.ItemToolResult, model.RoleTool, "", model.OutcomeInterrupted, "p1")}}
	return d
}
