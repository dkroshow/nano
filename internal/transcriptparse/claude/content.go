// Native content helpers extracted from history/transcript_claude.go,
// projection.go, claude_adapter.go and usage.go at 115207e.
package claude

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func contentBlocks(raw json.RawMessage) []map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if text := stringTextBlock(raw); text != nil {
		return []map[string]json.RawMessage{text}
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return []map[string]json.RawMessage{invalidContent(raw)}
	}
	blocks := make([]map[string]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		var block map[string]json.RawMessage
		if json.Unmarshal(entry, &block) == nil && block != nil {
			blocks = append(blocks, block)
			continue
		}
		if text := stringTextBlock(entry); text != nil {
			blocks = append(blocks, text)
		} else {
			blocks = append(blocks, invalidContent(entry))
		}
	}
	return blocks
}

// stringTextBlock wraps a raw JSON string value as a synthetic text block,
// or returns nil when the value is not a string. Empty native strings retain
// their position; otherwise every following block could inherit the wrong index.
func stringTextBlock(raw json.RawMessage) map[string]json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return nil
	}
	return map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": raw}
}

func invalidContent(raw json.RawMessage) map[string]json.RawMessage {
	return map[string]json.RawMessage{"type": json.RawMessage(`"invalid"`), "nativeValue": raw}
}

func rawText(row map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(row[key], &value)
	return value
}

// messageUsage lifts assistant message.usage into the per-turn envelope.
// Cache read and cache write stay separate. Total is not derived. thinking_tokens
// map to reasoning and are a subset of output, not additive with it.
func messageUsage(message map[string]json.RawMessage) *model.TokenUsage {
	usage := rawObject(message, "usage")
	if len(usage) == 0 {
		return nil
	}
	input, hasInput := usageInt(usage, "input_tokens")
	output, hasOutput := usageInt(usage, "output_tokens")
	cacheRead, hasRead := usageInt(usage, "cache_read_input_tokens")
	cacheCreate, hasCreate := usageInt(usage, "cache_creation_input_tokens")
	details := rawObject(usage, "output_tokens_details")
	thinking, hasThinking := usageInt(details, "thinking_tokens")
	if !hasInput && !hasOutput && !hasRead && !hasCreate && !hasThinking {
		return nil
	}
	result := &model.TokenUsage{Contract: model.UsageContractPerTurnV1}
	if hasInput {
		result.Input = int64Pointer(input)
	}
	if hasOutput {
		result.Output = int64Pointer(output)
	}
	if hasThinking {
		result.Reasoning = int64Pointer(thinking)
	}
	if hasRead {
		result.CacheRead = int64Pointer(cacheRead)
	}
	if hasCreate {
		result.CacheWrite = int64Pointer(cacheCreate)
	}
	native := map[string]any{}
	creation := rawObject(usage, "cache_creation")
	if fiveM, ok := usageInt(creation, "ephemeral_5m_input_tokens"); ok {
		native["cache_write_5m_tokens"] = fiveM
	}
	if oneH, ok := usageInt(creation, "ephemeral_1h_input_tokens"); ok {
		native["cache_write_1h_tokens"] = oneH
	}
	if len(native) > 0 {
		result.Native = marshalUsageNative(native)
	}
	return result
}

func usageInt(usage map[string]json.RawMessage, key string) (int64, bool) {
	value, _ := nativeCounter(usage[key])
	if value == nil {
		return 0, false
	}
	return *value, true
}

func answerText(row map[string]json.RawMessage) string {
	result := rawObject(row, "toolUseResult")
	if len(result) == 0 {
		return ""
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal(result["answers"], &answers) != nil || len(answers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(answers))
	for _, question := range answerOrder(result, answers) {
		answer := answerValue(answers[question])
		if answer == "" {
			continue
		}
		if question = strings.TrimSpace(question); question != "" {
			parts = append(parts, question+"\n→ "+answer)
		} else {
			parts = append(parts, answer)
		}
	}
	return strings.Join(parts, "\n\n")
}

// answerOrder returns the answered questions in the order they appear in
// toolUseResult.questions, falling back to sorted keys so the projection is
// deterministic when that ordering is unavailable.
func answerOrder(result map[string]json.RawMessage, answers map[string]json.RawMessage) []string {
	var questions []struct {
		Question string `json:"question"`
	}
	ordered := make([]string, 0, len(answers))
	if json.Unmarshal(result["questions"], &questions) == nil {
		for _, question := range questions {
			if _, ok := answers[question.Question]; ok {
				ordered = append(ordered, question.Question)
			}
		}
	}
	if len(ordered) == len(answers) {
		return ordered
	}
	ordered = ordered[:0]
	for question := range answers {
		ordered = append(ordered, question)
	}
	sort.Strings(ordered)
	return ordered
}

// answerValue coerces an answer, tolerating a plain string or the
// array shape a multi-select answer could carry.
func answerValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.TrimSpace(strings.Join(list, ", "))
	}
	return ""
}

func textBlocks(blocks []map[string]json.RawMessage) string {
	var parts []string
	for _, block := range blocks {
		if rawString(block, "type") == "text" {
			if text := rawString(block, "text"); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func blocksInjected(blocks []map[string]json.RawMessage) bool {
	text := textBlocks(blocks)
	return isInjectedClaudePrompt(text)
}

func blocksInterrupted(blocks []map[string]json.RawMessage) bool {
	text := strings.ToLower(strings.TrimSpace(textBlocks(blocks)))
	return strings.Contains(text, "<turn_aborted>") || text == "[request interrupted by user]"
}

func syntheticSummary(blocks []map[string]json.RawMessage) string {
	text := strings.ToLower(textBlocks(blocks))
	switch {
	case strings.Contains(text, "<turn_aborted>") || strings.Contains(text, "interrupted"):
		return "Turn interrupted"
	case strings.Contains(text, "<command-name>") || strings.Contains(text, "<command-message>"):
		return "Injected command context"
	case strings.Contains(text, "<system-reminder>"):
		return "Injected system reminder"
	default:
		return "Injected provider context"
	}
}

func toolResultText(block map[string]json.RawMessage) string {
	if text := contentText(block["content"]); text != "" {
		return text
	}
	return rawString(block, "content", "text")
}

func toolResultStatus(block map[string]json.RawMessage) model.ItemStatus {
	if rawBool(block, "is_error") || rawBool(block, "isError") {
		return model.ItemStatusFailed
	}
	return model.ItemStatusCompleted
}

func int64Pointer(value int64) *int64 {
	copy := value
	return &copy
}

func rawInt64(row map[string]json.RawMessage, keys ...string) int64 {
	for _, key := range keys {
		var value int64
		if json.Unmarshal(row[key], &value) == nil {
			return value
		}
		var number float64
		if json.Unmarshal(row[key], &number) == nil {
			return int64(number)
		}
	}
	return 0
}

func rawString(row map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := row[key]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func rawBool(row map[string]json.RawMessage, key string) bool {
	var value bool
	_ = json.Unmarshal(row[key], &value)
	return value
}

func rawObject(row map[string]json.RawMessage, key string) map[string]json.RawMessage {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(row[key], &value)
	return value
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		blockType := rawString(block, "type")
		if blockType != "" && blockType != "text" && blockType != "input_text" && blockType != "output_text" {
			continue
		}
		if value := rawString(block, "text", "input_text"); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

func marshalUsageNative(fields map[string]any) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return raw
}

func isInjectedClaudePrompt(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	for _, prefix := range []string{
		"<command-name>", "<command-message>", "<local-command", "<bash-stdout",
		"<bash-stderr", "<system-reminder>", "<task-notification>", "caveat:",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}
