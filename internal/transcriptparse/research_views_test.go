package transcriptparse_test

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Original expected.json files remain the research oracle. A reviewed difference
// names the reason for a changed view; other semantic fields have focused tests.
type researchView struct {
	Type         model.ItemType `json:"type"`
	Role         model.Role     `json:"role"`
	Text         string         `json:"text"`
	Phase        string         `json:"phase"`
	Outcome      model.Outcome  `json:"outcome"`
	NativeTurnID *string        `json:"nativeTurnId"`
}

type researchViewChange struct {
	Difference string         `json:"difference"`
	Reason     string         `json:"reason"`
	Items      []researchView `json:"items"`
}

func TestResearchItemViews(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		root := provider + "/testdata/"
		var reviewed struct {
			Cases       int                           `json:"cases"`
			Scenarios   map[string]string             `json:"scenarios"`
			Differences map[string]researchViewChange `json:"differences"`
		}
		readResearchJSON(t, root+"item-views.json", &reviewed)
		entries, err := os.ReadDir(root + "research")
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			names[name] = true
			t.Run(provider+"/"+name, func(t *testing.T) {
				if scenario := reviewed.Scenarios[name]; scenario != "" {
					t.Log("scenario assertions: " + scenario)
					return
				}
				var draft struct {
					Items []struct {
						Kind             string     `json:"kind"`
						Role             model.Role `json:"role"`
						Text             string     `json:"text"`
						Phase            string     `json:"nativePhase"`
						ExecutionOutcome string     `json:"executionOutcome"`
						Outcome          string     `json:"outcome"`
						TurnRef          *struct {
							NativeTurnID *string `json:"nativeTurnId"`
						} `json:"turnRef"`
					} `json:"items"`
				}
				readResearchJSON(t, root+"research/"+name+"/expected.json", &draft)
				want := []researchView{}
				for _, item := range draft.Items {
					if item.ExecutionOutcome != "" {
						item.Outcome = item.ExecutionOutcome
					}
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
					view := researchView{Type: typ, Role: item.Role, Text: item.Text, Phase: item.Phase, Outcome: outcome}
					if item.TurnRef != nil {
						view.NativeTurnID = item.TurnRef.NativeTurnID
					}
					want = append(want, view)
				}
				if change, ok := reviewed.Differences[name]; ok {
					if change.Difference == "" || change.Reason == "" {
						t.Fatal("difference needs its name and reason")
					}
					want = append([]researchView{}, change.Items...)
					t.Log(change.Difference + ": " + change.Reason)
				}
				raw, err := os.ReadFile(root + "research/" + name + "/input.jsonl")
				if err != nil {
					t.Fatal(err)
				}
				mode := "file"
				if strings.HasPrefix(name, "wire-") || strings.HasPrefix(name, "request-") || strings.HasPrefix(name, "edge-wire-") || strings.HasPrefix(name, "edge-response-") {
					mode = "wire"
				}
				input := model.Input{Bytes: raw, Source: model.SourceRef{Label: name, Incarnation: "research"}, Closed: true}
				batch := parseFixture(t, provider, mode, input)
				byID := map[string]model.Item{}
				for _, change := range batch.Changes {
					if change.Item != nil {
						byID[change.Item.ID] = *change.Item
					}
				}
				items := make([]model.Item, 0, len(byID))
				for _, item := range byID {
					items = append(items, item)
				}
				sort.Slice(items, func(i, j int) bool { return items[i].SourceSeq < items[j].SourceSeq })
				got := []researchView{}
				for _, item := range items {
					got = append(got, researchView{item.Type, item.Role, item.Text, item.Phase, item.Outcome, item.Scope.NativeTurnID})
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("item view differs\nwant: %+v\ngot: %+v", want, got)
				}
			})
		}
		if len(names) != reviewed.Cases {
			t.Fatalf("%s research cases changed: %d", provider, len(names))
		}
		for name := range reviewed.Differences {
			if !names[name] {
				t.Fatalf("difference has no fixture: %s", name)
			}
		}
		for name := range reviewed.Scenarios {
			if !names[name] {
				t.Fatalf("scenario has no fixture: %s", name)
			}
		}
	}
}

func readResearchJSON(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
