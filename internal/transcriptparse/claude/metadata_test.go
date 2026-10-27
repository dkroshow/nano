package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestClaudeAttachmentFixturesRetainNativeContext(t *testing.T) {
	entries, err := os.ReadDir("testdata/research")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "capture-attachment-") {
			continue
		}
		count++
		t.Run(entry.Name(), func(t *testing.T) {
			raw := fixture(t, entry.Name())
			state, batch, err := parseFileView(parsedView{}, inputFor(raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Turns) != 0 || len(batch.Diagnostics) != 0 {
				t.Fatal("context attachment created a turn or lost recognized schema")
			}
			var source map[string]json.RawMessage
			if json.Unmarshal(bytes.TrimSpace(raw), &source) != nil {
				t.Fatal("fixture input is not one row")
			}
			matched := false
			for _, observation := range batch.Observations {
				var detail map[string]json.RawMessage
				if json.Unmarshal(observation.Detail, &detail) == nil && rowDigest(detail) == rowDigest(source) {
					matched = true
					if observation.NativeEventID != rawString(source, "uuid") || observation.Source.Line != 1 || observation.Scope.NativeThreadID == nil || observation.Scope.Kind != model.ScopeThread {
						t.Fatal("attachment lost native identity, source or scope")
					}
				}
			}
			if !matched {
				t.Fatal("attachment payload was replaced with an envelope-only placeholder")
			}
		})
	}
	if count != 23 {
		t.Fatalf("attachment fixture inventory changed: %d", count)
	}
}

func TestClaudeMetadataFieldsAndUnknownPayloads(t *testing.T) {
	for _, name := range []string{"capture-row-custom-title", "capture-row-ai-title", "capture-row-last-prompt", "capture-row-agent-name", "capture-row-agent-setting", "capture-row-atis-latch", "capture-row-bridge-session", "capture-row-cost-state", "capture-row-file-history-snapshot", "capture-row-file-history-delta", "capture-row-queue-operation", "capture-row-mode", "capture-row-permission-mode", "wire-rate-limit-event"} {
		t.Run(name, func(t *testing.T) {
			raw := fixture(t, name)
			state, batch, err := parseFileView(parsedView{}, inputFor(raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Turns) != 0 {
				t.Fatal("source metadata invented a turn")
			}
			var source map[string]json.RawMessage
			_ = json.Unmarshal(raw, &source)
			found := false
			for _, observation := range batch.Observations {
				var detail map[string]json.RawMessage
				if json.Unmarshal(observation.Detail, &detail) == nil && rowDigest(detail) == rowDigest(source) {
					found = true
				}
			}
			if !found {
				t.Fatal("native metadata fields lost")
			}
		})
	}
}

func TestClaudeJournalResultDoesNotCompleteConversation(t *testing.T) {
	raw := []byte(`{"type":"user","message":{"content":"Parent"}}` + "\n")
	raw = append(raw, fixture(t, "capture-row-started")...)
	raw = append(raw, fixture(t, "capture-row-result")...)
	state, batch, err := parseFileView(parsedView{}, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Turns) != 1 {
		t.Fatal("journal created a conversation turn")
	}
	for _, turn := range state.Turns {
		if len(turn.Completion) != 0 || turn.Outcome != model.OutcomeUnknown {
			t.Fatal("journal result entered conversation completion evidence")
		}
	}
	if len(batch.Diagnostics) != 0 {
		t.Fatal("recognized journal envelope lost")
	}
	for _, obs := range batch.Observations {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(obs.Detail, &fields)
		if rawString(fields, "key") != "" && (obs.Scope.NativeThreadID != nil || obs.Scope.TurnKey != "" || obs.Scope.Kind != model.ScopeConnection) {
			t.Fatal("journal task was assigned conversation identity")
		}
	}
}

func TestClaudeCompactionRetainsTextWithoutPromptBoundary(t *testing.T) {
	state, batch, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"summary","summary":"Retained summary","leafUuid":"leaf"}`+"\n"+
		`{"type":"user","uuid":"compact","isCompactSummary":true,"message":{"content":"Retained context"}}`+"\n")))
	if err != nil {
		t.Fatal(err)
	}
	items := orderedItems(state)
	if len(items) != 2 || len(state.Turns) != 0 || len(batch.Diagnostics) != 0 || items[0].Type != model.ItemCompaction || items[1].Type != model.ItemCompaction || items[0].Text != "Retained summary" || items[1].Text != "Retained context" {
		t.Fatal("compaction content or independent scope lost")
	}
}

func TestClaudeCostStateRetainsNativeStartWithoutTurnUsage(t *testing.T) {
	for _, value := range []string{"1788303347922", "null", "9223372036854775807"} {
		state, batch, err := parseFileView(parsedView{}, inputFor([]byte(`{"type":"cost-state","startTime":`+value+`,"modelUsage":{},"totalCostUSD":10}`+"\n")))
		if err != nil {
			t.Fatal(err)
		}
		if len(state.Turns) != 0 {
			t.Fatal("session cost became turn usage")
		}
		found := false
		for _, obs := range batch.Observations {
			for _, stamp := range obs.Times {
				if stamp.SourceField != "startTime" {
					continue
				}
				found = true
				if stamp.Class != model.TimeNative || stamp.Meaning != model.TimeStart || string(stamp.Raw) != value {
					t.Fatal("native start timestamp lost its meaning")
				}
				if value == "1788303347922" && (stamp.Value == nil || stamp.Value.UnixMilli() != 1788303347922) {
					t.Fatal("native millisecond timestamp changed units")
				}
				if value != "1788303347922" && stamp.Value != nil {
					t.Fatal("null or unrepresentable native time became a date")
				}
			}
		}
		if !found {
			t.Fatal("native cost start time not retained")
		}
		if _, err := json.Marshal(batch); err != nil {
			t.Fatal(err)
		}
	}
}
