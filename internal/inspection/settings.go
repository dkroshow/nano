package inspection

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type ClaudePermissions struct {
	DefaultMode string   `json:"defaultMode,omitempty"`
	Allow       []string `json:"allow"`
	Ask         []string `json:"ask"`
	Deny        []string `json:"deny"`
}

type ClaudeSettings struct {
	Model               string            `json:"model,omitempty"`
	Effort              string            `json:"effort,omitempty"`
	OutputStyle         string            `json:"outputStyle,omitempty"`
	AutoMemoryEnabled   *bool             `json:"autoMemoryEnabled,omitempty"`
	AutoMemoryDirectory string            `json:"autoMemoryDirectory,omitempty"`
	ClaudeMDExcludes    []string          `json:"claudeMdExcludes"`
	EnabledPlugins      map[string]bool   `json:"enabledPlugins"`
	Permissions         ClaudePermissions `json:"permissions"`
}

type claudeSettingsDocument struct {
	Model               string          `json:"model"`
	EffortLevel         string          `json:"effortLevel"`
	Effort              string          `json:"effort"`
	OutputStyle         string          `json:"outputStyle"`
	AutoMemoryEnabled   *bool           `json:"autoMemoryEnabled"`
	AutoMemoryDirectory string          `json:"autoMemoryDirectory"`
	ClaudeMDExcludes    []string        `json:"claudeMdExcludes"`
	EnabledPlugins      map[string]bool `json:"enabledPlugins"`
	Permissions         struct {
		DefaultMode string   `json:"defaultMode"`
		Allow       []string `json:"allow"`
		Ask         []string `json:"ask"`
		Deny        []string `json:"deny"`
	} `json:"permissions"`
}

// ReadClaudeSettings merges only the explicit fixture or production paths the
// caller supplies. It never discovers a home directory on its own.
func ReadClaudeSettings(paths []string) (ClaudeSettings, []string) {
	settings := ClaudeSettings{
		ClaudeMDExcludes: []string{}, EnabledPlugins: map[string]bool{},
		Permissions: ClaudePermissions{Allow: []string{}, Ask: []string{}, Deny: []string{}},
	}
	var diagnostics []string
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("Read Claude settings %s: %v", path, err))
			continue
		}
		var document claudeSettingsDocument
		if err := json.Unmarshal(content, &document); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("Parse Claude settings %s: %v", path, err))
			continue
		}
		if document.Model != "" {
			settings.Model = document.Model
		}
		if effort := firstNonempty(document.EffortLevel, document.Effort); effort != "" {
			settings.Effort = effort
		}
		if document.OutputStyle != "" {
			settings.OutputStyle = document.OutputStyle
		}
		if document.AutoMemoryEnabled != nil {
			value := *document.AutoMemoryEnabled
			settings.AutoMemoryEnabled = &value
		}
		if document.AutoMemoryDirectory != "" {
			settings.AutoMemoryDirectory = document.AutoMemoryDirectory
		}
		if document.Permissions.DefaultMode != "" {
			settings.Permissions.DefaultMode = document.Permissions.DefaultMode
		}
		settings.ClaudeMDExcludes = unique(append(settings.ClaudeMDExcludes, document.ClaudeMDExcludes...))
		settings.Permissions.Allow = unique(append(settings.Permissions.Allow, document.Permissions.Allow...))
		settings.Permissions.Ask = unique(append(settings.Permissions.Ask, document.Permissions.Ask...))
		settings.Permissions.Deny = unique(append(settings.Permissions.Deny, document.Permissions.Deny...))
		for name, enabled := range document.EnabledPlugins {
			settings.EnabledPlugins[name] = enabled
		}
	}
	return settings, diagnostics
}

func KnownMetadataValue(state string, value *string) string {
	if state == "known" && value != nil {
		return strings.TrimSpace(*value)
	}
	return ""
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
