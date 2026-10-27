package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexResearchOversizeRecipeAdvancesSource(t *testing.T) {
	raw, err := os.ReadFile("testdata/research/edge-oversize-body-budget/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var recipe struct {
		Observations []struct {
			ReplaceTextWith struct {
				Repeat string `json:"repeat"`
				Count  int    `json:"count"`
			} `json:"replaceTextWith"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(raw, &recipe); err != nil || len(recipe.Observations) != 1 {
		t.Fatal("invalid size recipe")
	}
	repeat := recipe.Observations[0].ReplaceTextWith
	if repeat.Repeat != "x" || repeat.Count != defaultMaxRowBytes+1 {
		t.Fatal("size recipe changed; review its construction")
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(fixture(t, "edge-oversize-body-budget"), &row); err != nil {
		t.Fatal(err)
	}
	payload := rawObject(row, "payload")
	payload["content"] = mustJSON([]map[string]string{{"type": "output_text", "text": string(bytes.Repeat([]byte(repeat.Repeat), repeat.Count))}})
	row["payload"] = mustJSON(payload)
	large := append(mustJSON(row), '\n')
	state, batch, err := parseFileView(parsedView{}, inputFor(large))
	if err != nil || len(state.Items) != 0 || len(state.Turns) != 0 || len(batch.Records) != 1 || batch.Records[0].Capture != model.CaptureTruncated || len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "row_too_large" {
		t.Fatal("oversized native row was silently skipped or became semantic content")
	}
	if batch.Records[0].Source.End != int64(len(large)) {
		t.Fatal("oversized record did not advance source checkpoint")
	}
	checkpoint, err := json.Marshal(state)
	if err != nil || len(checkpoint) > 4096 {
		t.Fatal("checkpoint retained oversized body")
	}
	var restored parsedView
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	state, batch, err = parseFileView(restored, inputFor([]byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"After large row"}}`+"\n")))
	items := orderedItems(state)
	if err != nil || len(items) != 1 || items[0].Text != "After large row" || items[0].SourceLine != 2 || len(batch.Diagnostics) != 0 || batch.Records[0].Source.Start != int64(len(large)) {
		t.Fatal("reader did not recover after oversized record")
	}
}
