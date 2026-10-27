package codex

// Helpers extracted from history/transcript_codex.go and codex_adapter.go at
// 115207e. Reader assembly records evidence separately from display reductions.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"regexp"
	"strings"
)

func codexTurnConfig(payload map[string]json.RawMessage) json.RawMessage {
	settings := codexCollaborationSettings(payload)
	model := firstRawString([]map[string]json.RawMessage{payload, settings}, "model")
	effort := firstRawString([]map[string]json.RawMessage{payload, settings}, "effort", "reasoning_effort", "reasoningEffort")
	return mustJSON(map[string]string{
		"model":          model,
		"cwd":            rawString(payload, "cwd"),
		"effort":         effort,
		"approvalPolicy": rawString(payload, "approval_policy", "approvalPolicy"),
		"serviceTier":    codexTurnServiceTier(payload),
	})
}

func codexTurnServiceTier(payload map[string]json.RawMessage) string {
	return firstRawString([]map[string]json.RawMessage{payload, codexCollaborationSettings(payload)}, "service_tier", "serviceTier")
}

func codexExplicitSettingsServiceTier(payload map[string]json.RawMessage) string {
	return firstRawString([]map[string]json.RawMessage{codexThreadSettings(payload), payload}, "service_tier", "serviceTier")
}

func codexThreadSettings(payload map[string]json.RawMessage) map[string]json.RawMessage {
	settings := rawObject(payload, "thread_settings")
	if len(settings) == 0 {
		settings = rawObject(payload, "threadSettings")
	}
	return settings
}

// codexCollaborationSettings returns collaboration_mode.settings, where 0.144+
// threads relocated model/effort/service_tier from the turn_context top level.
func codexCollaborationSettings(payload map[string]json.RawMessage) map[string]json.RawMessage {
	collaboration := rawObject(payload, "collaboration_mode")
	if len(collaboration) == 0 {
		collaboration = rawObject(payload, "collaborationMode")
	}
	return rawObject(collaboration, "settings")
}

func codexItemStatus(payload map[string]json.RawMessage) model.ItemStatus {
	switch strings.ToLower(rawString(payload, "status")) {
	case "in_progress", "running":
		return model.ItemStatusInProgress
	case "failed", "error":
		return model.ItemStatusFailed
	case "interrupted", "cancelled", "canceled":
		return model.ItemStatusInterrupted
	case "", "completed", "success":
		return model.ItemStatusCompleted
	default:
		return model.ItemStatusUnknown
	}
}

func isCommandTool(name, kind string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return kind == "local_shell_call" || name == "bash" || strings.Contains(name, "exec_command") || strings.Contains(name, "shell") || strings.Contains(name, "command")
}

func codexToolSummary(name string, payload map[string]json.RawMessage) string {
	if command := rawString(payload, "command", "cmd"); command != "" {
		return command
	}
	return fmt.Sprintf("Tool call: %s", name)
}

func codexToolDetail(payload map[string]json.RawMessage) map[string]string {
	return map[string]string{
		"name":   rawString(payload, "name"),
		"callId": rawString(payload, "call_id", "callId"),
		"status": rawString(payload, "status"),
	}
}

func codexSettingsDetail(payload map[string]json.RawMessage) map[string]string {
	settings := codexThreadSettings(payload)
	return map[string]string{
		"model":       firstRawString([]map[string]json.RawMessage{settings, payload}, "model"),
		"effort":      firstRawString([]map[string]json.RawMessage{settings, payload}, "effort", "reasoning_effort", "reasoningEffort"),
		"serviceTier": codexExplicitSettingsServiceTier(payload),
	}
}

func codexSubagentSummary(payload map[string]json.RawMessage) string {
	kind := rawString(payload, "kind")
	if kind == "" {
		return "Sub-agent activity"
	}
	return "Sub-agent " + strings.ReplaceAll(kind, "_", " ")
}

func codexSubagentDetail(payload map[string]json.RawMessage) map[string]string {
	return map[string]string{
		"kind":          rawString(payload, "kind"),
		"agentPath":     rawString(payload, "agent_path", "agentPath"),
		"agentThreadId": rawString(payload, "agent_thread_id", "agentThreadId", "thread_id", "threadId"),
	}
}

func codexCommandDisplay(payload map[string]json.RawMessage) (string, map[string]string) {
	arguments := codexArguments(payload)
	command := codexCommandLine(arguments, payload)
	detail := codexToolDetail(payload)
	if workdir := firstRawString([]map[string]json.RawMessage{arguments, payload}, "workdir", "cwd"); workdir != "" {
		detail["workdir"] = workdir
	}
	return command, detail
}

// codexArguments decodes a tool call's `arguments`, accepting either a native
// JSON object or a JSON-encoded string wrapping one.
func codexArguments(payload map[string]json.RawMessage) map[string]json.RawMessage {
	raw, ok := payload["arguments"]
	if !ok {
		return nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		return object
	}
	var encoded string
	if json.Unmarshal(raw, &encoded) == nil {
		if json.Unmarshal([]byte(strings.TrimSpace(encoded)), &object) == nil {
			return object
		}
	}
	return nil
}

func codexCommandLine(arguments, payload map[string]json.RawMessage) string {
	if arguments != nil {
		if command := codexCommandFromArray(arguments["command"]); command != "" {
			return command
		}
	}
	if command := codexCommandFromArray(payload["command"]); command != "" {
		return command
	}
	command := firstRawString([]map[string]json.RawMessage{arguments, payload}, "cmd", "command")
	return codexStripShellWrapper(command)
}

// codexCommandFromArray reads a command given as argv (e.g. ["bash","-lc","…"]),
// returning the script body after a `-lc` flag or the space-joined argv.
func codexCommandFromArray(raw json.RawMessage) string {
	var argv []string
	if json.Unmarshal(raw, &argv) != nil || len(argv) == 0 {
		return ""
	}
	for index, arg := range argv {
		if arg == "-lc" && index+1 < len(argv) {
			return argv[index+1]
		}
	}
	return strings.Join(argv, " ")
}

var codexShellWrapper = regexp.MustCompile(`^(?:/bin/)?(?:zsh|bash|sh)\s+-lc\s+(['"])`)

func codexStripShellWrapper(command string) string {
	text := strings.TrimSpace(command)
	loc := codexShellWrapper.FindStringSubmatchIndex(text)
	if loc == nil {
		return text
	}
	quote := text[loc[2]]
	bodyStart := loc[1]
	if len(text) <= bodyStart || text[len(text)-1] != quote {
		return text
	}
	body := text[bodyStart : len(text)-1]
	body = strings.ReplaceAll(body, `\"`, `"`)
	body = strings.ReplaceAll(body, `\'`, `'`)
	return body
}

func codexTextOrJSON(payload map[string]json.RawMessage, keys ...string) string {
	if text := nativeText(payload, keys...); text != "" {
		return text
	}
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok || len(raw) == 0 {
			continue
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			var compact bytes.Buffer
			if json.Compact(&compact, trimmed) == nil {
				return compact.String()
			}
		}
	}
	return ""
}

func codexWorldStateDetail(row map[string]json.RawMessage) map[string]any {
	result := map[string]any{"kind": "world_state"}
	for _, key := range []string{"agents", "threads"} {
		var values []json.RawMessage
		if json.Unmarshal(row[key], &values) == nil {
			result[key+"Count"] = len(values)
		}
	}
	return result
}

func codexCommunicationDetail(row map[string]json.RawMessage) map[string]string {
	payload := rawObject(row, "payload")
	if len(payload) == 0 {
		payload = row
	}
	return map[string]string{
		"messageId":        rawString(payload, "message_id", "messageId"),
		"senderThreadId":   rawString(payload, "sender_thread_id", "senderThreadId"),
		"receiverThreadId": rawString(payload, "receiver_thread_id", "receiverThreadId"),
		"correlationId":    rawString(payload, "correlation_id", "correlationId"),
	}
}

func collaborationTag(enabled bool) []string {
	if enabled {
		return []string{"collaboration"}
	}
	return nil
}

// codexResponseUserText returns semantic user-authored text from a Codex
// response-item message. Codex persists trusted harness context in user-role
// envelopes, so role alone is not enough to establish conversation authorship.
func codexResponseUserText(payload map[string]json.RawMessage) (string, bool) {
	if content, exists := payload["content"]; exists && len(content) > 0 {
		return codexUserContentText(content)
	}
	text := rawString(payload, "message", "text")
	if strings.TrimSpace(text) == "" || isCodexSyntheticPrompt(text) {
		return "", false
	}
	return text, true
}

func codexUserContentText(raw json.RawMessage) (string, bool) {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return codexClassifyWholeUserText(plain)
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		blockType := rawString(block, "type")
		if blockType != "" && blockType != "text" && blockType != "input_text" && blockType != "output_text" {
			continue
		}
		if text := rawString(block, "text", "input_text"); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	joined := strings.Join(parts, "\n")
	if !hasCodexRecommendedPluginsHarnessSignature(joined) {
		return codexClassifyWholeUserText(joined)
	}
	if len(parts) == 1 {
		// One block can combine trusted context with a real request. Its contents
		// are structurally ambiguous, even when they end in a harness tag.
		return codexClassifyWholeUserText(joined)
	}
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if !isCodexInjectedHarnessBlock(part) {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return "", false
	}
	return strings.Join(kept, "\n"), true
}

func codexClassifyWholeUserText(text string) (string, bool) {
	if strings.TrimSpace(text) == "" || isCodexSyntheticPrompt(text) {
		return "", false
	}
	return text, true
}

func hasCodexRecommendedPluginsHarnessSignature(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "<recommended_plugins>") &&
		strings.Contains(text, "</recommended_plugins>") &&
		strings.Contains(text, "# AGENTS.md instructions for ") &&
		(strings.Contains(text, "<environment_context>") || strings.Contains(text, "<permissions instructions>") || strings.Contains(text, "<skills_instructions>"))
}

func isCodexInjectedHarnessBlock(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "<recommended_plugins>") && strings.Contains(text, "</recommended_plugins>") {
		return true
	}
	if strings.HasPrefix(text, "# AGENTS.md instructions") {
		return true
	}
	for _, prefix := range []string{"<environment_context>", "<permissions instructions>", "<skills_instructions>"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func isCodexSyntheticPrompt(text string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{
		"<skill>", "# AGENTS.md instructions", "<environment_context>",
		"<permissions instructions>", "<collaboration_mode>", "<skills_instructions>",
		"<subagent_notification>", "<turn_aborted>",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
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

// Native text can be an empty string or a whitespace-only stream fragment.
// Identity/discriminator readers use rawString; content readers preserve bytes.
func nativeText(row map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw := bytes.TrimSpace(row[key])
		if len(raw) == 0 || raw[0] != '"' {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
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

func firstRawString(objects []map[string]json.RawMessage, keys ...string) string {
	for _, object := range objects {
		if value := rawString(object, keys...); value != "" {
			return value
		}
	}
	return ""
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
		blockType := strings.ToLower(rawString(block, "type"))
		if blockType != "" && blockType != "text" && blockType != "input_text" && blockType != "output_text" {
			continue
		}
		if value := nativeText(block, "text", "input_text"); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

func mustJSON(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }

func nativeBool(raw json.RawMessage) (bool, bool) {
	var value *bool
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false, false
	}
	return *value, true
}
