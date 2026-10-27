package transcriptparse_test

import (
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestReusedNativeCallIDCannotChooseLastInvocation(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			raw, err := os.ReadFile(provider + "/testdata/ambiguous-call-id/input.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			input := model.Input{Bytes: raw, Source: model.SourceRef{Label: "reused-call", Incarnation: "one"}, Closed: true}
			batch := parseFixture(t, provider, "file", input)
			items := map[string]model.Item{}
			for _, change := range batch.Changes {
				if change.Item != nil {
					items[change.Item.ID] = *change.Item
				}
			}
			calls, results := 0, 0
			for _, item := range items {
				if item.CallID != "c" {
					continue
				}
				if item.Type != model.ItemToolResult {
					calls++
					continue
				}
				results++
				if len(item.Relations) != 0 || item.Text != "Execution failed" {
					t.Fatal("ambiguous result guessed an invocation or lost text")
				}
			}
			if calls != 2 || results != 1 {
				t.Fatal("native call/result evidence collapsed")
			}
			ambiguous, unresolved := false, false
			for _, d := range batch.Diagnostics {
				ambiguous = ambiguous || d.Code == "ambiguous_call_id"
				unresolved = unresolved || d.Code == "unresolved_tool_result"
			}
			if !ambiguous || !unresolved {
				t.Fatal("ambiguous identity and unresolved result need explicit diagnostics")
			}
			checkModelConsistency(t, batch)
		})
	}
}
