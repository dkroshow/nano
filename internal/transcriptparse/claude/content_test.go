package claude

import (
	"encoding/json"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func object(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestExtractedContentKeepsNativeBlockOrder(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		types     []string
	}{
		{"bare", `"prompt"`, []string{"text"}},
		{"mixed", `["one",{"type":"thinking","thinking":"two"},{"type":"tool_use","id":"c1","input":{"command":"true"}},"four",{"type":"future","payload":42}]`, []string{"text", "thinking", "tool_use", "text", "future"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := contentBlocks(json.RawMessage(tc.raw))
			if len(blocks) != len(tc.types) {
				t.Fatalf("blocks %d, want %d", len(blocks), len(tc.types))
			}
			for i, block := range blocks {
				var kind string
				_ = json.Unmarshal(block["type"], &kind)
				if kind != tc.types[i] {
					t.Fatalf("block %d kind %q", i, kind)
				}
			}
			if tc.name == "mixed" && string(blocks[4]["payload"]) != "42" {
				t.Fatal("unknown block lost native detail")
			}
		})
	}
}

func TestExtractedUsageKeepsCacheGrainsAndNullability(t *testing.T) {
	usage := messageUsage(object(t, `{"usage":{"input_tokens":0,"output_tokens":12,"cache_read_input_tokens":30,"cache_creation_input_tokens":40,"output_tokens_details":{"thinking_tokens":2},"cache_creation":{"ephemeral_5m_input_tokens":15,"ephemeral_1h_input_tokens":25}}}`))
	if usage == nil || usage.Input == nil || *usage.Input != 0 || *usage.Output != 12 || *usage.CacheRead != 30 || *usage.CacheWrite != 40 || *usage.Reasoning != 2 {
		t.Fatalf("usage %+v", usage)
	}
	if usage.Total != nil || usage.Cached != nil {
		t.Fatal("invented total or folded cache-write")
	}
	native := object(t, string(usage.Native))
	if string(native["cache_write_5m_tokens"]) != "15" || string(native["cache_write_1h_tokens"]) != "25" {
		t.Fatal("cache duration buckets lost")
	}
	partial := messageUsage(object(t, `{"usage":{"output_tokens":3}}`))
	if partial == nil || partial.Input != nil || *partial.Output != 3 {
		t.Fatal("missing usage changed to zero")
	}
}

func TestExtractedQuestionAnswersKeepProviderOrder(t *testing.T) {
	row := object(t, `{"toolUseResult":{"questions":[{"question":"Second?"},{"question":"First?"}],"answers":{"First?":"one","Second?":["two","three"]}}}`)
	if got := answerText(row); got != "Second?\n→ two, three\n\nFirst?\n→ one" {
		t.Fatalf("ordered answers %q", got)
	}
	row = object(t, `{"toolUseResult":{"answers":{"B":"two","A":"one"}}}`)
	if got := answerText(row); got != "A\n→ one\n\nB\n→ two" {
		t.Fatalf("fallback order %q", got)
	}
}

func TestExtractedToolResultSeparatesTextAndError(t *testing.T) {
	block := object(t, `{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"exit code 1"}],"is_error":true}`)
	if got := toolResultText(block); got != "exit code 1" {
		t.Fatalf("tool text %q", got)
	}
	if toolResultStatus(block) != model.ItemStatusFailed {
		t.Fatal("error result lost failure")
	}
	block = object(t, `{"type":"tool_result","content":"ok"}`)
	if toolResultStatus(block) != model.ItemStatusCompleted || toolResultText(block) != "ok" {
		t.Fatal("successful result not preserved")
	}
}
