package main

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/interact"
)

type tokenUsage = interact.TokenUsage

// normalizeTokenUsage keeps only the provider counters needed by feature
// clients. Codex reports them under last; Claude and Grok use the direct shape.
func normalizeTokenUsage(raw json.RawMessage) *tokenUsage {
	var body map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &body) != nil {
		return nil
	}
	if nested := body["last"]; len(nested) > 0 {
		var last map[string]json.RawMessage
		if json.Unmarshal(nested, &last) == nil {
			body = last
		}
	}
	usage := &tokenUsage{
		InputTokens:       nonnegativeTokenCount(body["inputTokens"]),
		CachedInputTokens: nonnegativeTokenCount(body["cachedInputTokens"]),
		OutputTokens:      nonnegativeTokenCount(body["outputTokens"]),
	}
	if usage.InputTokens == nil && usage.CachedInputTokens == nil && usage.OutputTokens == nil {
		return nil
	}
	return usage
}

func nonnegativeTokenCount(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		return nil
	}
	var count int64
	if json.Unmarshal(raw, &count) != nil || count < 0 {
		return nil
	}
	return &count
}
