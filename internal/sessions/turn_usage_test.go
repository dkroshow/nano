package sessions

import (
	"encoding/json"
	"testing"
)

func TestNormalizeTokenUsageUsesPerTurnCounters(t *testing.T) {
	usage := normalizeTokenUsage(json.RawMessage(`{"last":{"inputTokens":13,"cachedInputTokens":5,"outputTokens":8},"total":{"inputTokens":999}}`))
	if usage == nil || usage.InputTokens == nil || *usage.InputTokens != 13 || usage.CachedInputTokens == nil || *usage.CachedInputTokens != 5 || usage.OutputTokens == nil || *usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v", usage)
	}
	if got := normalizeTokenUsage(json.RawMessage(`{"total":{"inputTokens":999}}`)); got != nil {
		t.Fatalf("cumulative counters became per-turn usage: %+v", got)
	}
}
