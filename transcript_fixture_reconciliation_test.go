package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The old hashes were captured before parser deletion. Changed expectations
// were reviewed against native fixtures and the named semantic assertions in
// docs/parsing/conformance.md. This test prevents an unreviewed new difference
// from joining the accepted set; it does not generate its own expectations.
func TestTranscriptFixtureReconciliation(t *testing.T) {
	var receipts []struct {
		Provider, Case, Input, Mode                      string
		SourceSHA256, LegacyViewSHA256, ModuleViewSHA256 string
		Rules                                            []string
	}
	raw, err := os.ReadFile("testdata/transcript-compatibility.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &receipts); err != nil {
		t.Fatal(err)
	}
	docs, err := os.ReadFile("docs/parsing/conformance.md")
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool)
	// Each fixture has private parser state and its own database. Limit concurrent
	// conversions so the full set does not serialize hundreds of schema setups or
	// open hundreds of databases at once on machines with many test workers.
	conversions := make(chan struct{}, 4)
	for _, receipt := range receipts {
		receipt := receipt // go.mod retains Go 1.21 loop-variable semantics.
		path := filepath.Join("internal/transcriptparse", receipt.Provider, "testdata", receipt.Case, receipt.Input)
		if covered[path] {
			t.Fatalf("duplicate fixture receipt: %s", path)
		}
		covered[path] = true
		t.Run(receipt.Provider+"/"+receipt.Case+"/"+receipt.Input, func(t *testing.T) {
			t.Parallel()
			conversions <- struct{}{}
			defer func() { <-conversions }()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Input == "input.recipe.json" {
				var recipe struct{ PayloadBytes int }
				if receipt.Provider != "grok" || receipt.Case != "research/oversized-record" || json.Unmarshal(raw, &recipe) != nil || recipe.PayloadBytes != 1048577 {
					t.Fatal("unrecognized size recipe")
				}
				raw, err = json.Marshal(map[string]any{"type": "agent_message_chunk", "text": strings.Repeat("x", recipe.PayloadBytes)})
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, '\n')
			}
			if fixtureDigest(raw) != receipt.SourceSHA256 || fixtureReaderMode(receipt.Provider, filepath.Base(receipt.Case), receipt.Input, raw) != receipt.Mode {
				t.Fatal("native input or reader route changed; review the comparison again")
			}
			changed := receipt.LegacyViewSHA256 != receipt.ModuleViewSHA256
			if changed != (len(receipt.Rules) != 0) {
				t.Fatal("each changed view requires named differences; unchanged views require none")
			}
			for _, name := range receipt.Rules {
				if !strings.Contains(string(docs), name) {
					t.Fatalf("difference %s has no conformance documentation", name)
				}
			}
			view := fixtureModuleView(t, receipt.Provider, receipt.Mode, raw)
			if receipt.Mode == "wire" {
				// A wire fixture must still parse, but no surface presents wire
				// rows any more: panes and agents read the saved file.
				return
			}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			// Object-key ordering makes the receipt independent of Go field order.
			var objects any
			if err := json.Unmarshal(encoded, &objects); err != nil {
				t.Fatal(err)
			}
			encoded, err = json.Marshal(objects)
			if err != nil {
				t.Fatal(err)
			}
			if fixtureDigest(encoded) != receipt.ModuleViewSHA256 {
				t.Fatalf("converted view changed outside reviewed rules %v", receipt.Rules)
			}
		})
	}
	for _, provider := range []string{"claude", "codex", "grok"} {
		root := filepath.Join("internal/transcriptparse", provider, "testdata")
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && fixtureNativeInput(provider, entry.Name()) && !covered[path] {
				t.Errorf("native fixture lacks a comparison receipt: %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
