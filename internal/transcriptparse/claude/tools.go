package claude

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func toolInvocation(item model.Item) bool {
	switch item.Type {
	case model.ItemToolCall, model.ItemCommand, model.ItemFileChange, model.ItemInteraction, model.ItemPlan:
		return true
	}
	return false
}

// Specialization follows the historical Claude tool taxonomy, retaining the
// native argument stream in the first part. Display text never feeds subsequent
// JSON argument deltas. A plan/edit invocation describes intent, not approval or
// execution; its separate result supplies execution evidence.
func specializeTool(item *model.Item) {
	if !toolInvocation(*item) || len(item.Parts) == 0 {
		return
	}
	var input map[string]json.RawMessage
	if json.Unmarshal([]byte(item.Parts[0].Text), &input) != nil || input == nil {
		return
	}
	name := strings.ToLower(strings.TrimSpace(item.ToolName))
	switch {
	case name == "bash" || strings.Contains(name, "exec_command") || strings.Contains(name, "shell") || strings.Contains(name, "command"):
		item.Type, item.Category = model.ItemCommand, model.CategoryToolCall
		if command := rawText(input, "command"); command != "" {
			item.Text = command
		} else if command := rawText(input, "cmd"); command != "" {
			item.Text = command
		}
	case name == "edit" || name == "multiedit" || name == "write":
		if path := rawText(input, "file_path"); path != "" {
			item.Type, item.Category, item.Text = model.ItemFileChange, model.CategoryToolCall, path
		}
	case name == "exitplanmode" || name == "enterplanmode":
		if plan := rawText(input, "plan"); plan != "" {
			item.Type, item.Category, item.Text = model.ItemPlan, model.CategorySystem, plan
		}
	}
}
