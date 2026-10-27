package codex

import (
	"os"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestCodexFileReasoningSummaryFixtures(t *testing.T) {
	for _, tc := range []struct{ name, text string }{{"file-response-item-reasoning", "[redacted]"}, {"schema-response-item-reasoning", "Example summary."}} {
		raw, err := os.ReadFile("testdata/research/" + tc.name + "/input.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		input := inputFor(raw)
		input.Closed = true
		state, batch, err := parseFileView(parsedView{}, input)
		if err != nil || len(batch.Diagnostics) != 0 || len(state.Items) != 1 {
			t.Fatalf("reasoning fixture: %v %+v", err, batch.Diagnostics)
		}
		for _, item := range state.Items {
			if item.Type != model.ItemReasoning || item.Text != tc.text || len(item.Parts) == 0 || item.Parts[0].Text != tc.text || item.IsFinalAnswer {
				t.Errorf("%s lost readable summary", tc.name)
			}
		}
	}
}

func TestCodexFileReasoningKeepsSummaryAndRawPartsInOrder(t *testing.T) {
	input := inputFor([]byte(`{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"  summary  "}],"content":[{"type":"reasoning_text","text":"raw one"},{"type":"reasoning_text","text":"raw two"}],"encrypted_content":"ciphertext"}}` + "\n"))
	input.Closed = true
	state, _, err := parseFileView(parsedView{}, input)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Items {
		if item.Text != "  summary  \n\nraw one\n\nraw two" || len(item.Parts) != 4 || item.Parts[3].Text != "" {
			t.Error("summary hid raw content, trimmed text or exposed ciphertext")
		}
	}
}
