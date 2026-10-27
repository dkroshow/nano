package claude

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Extracted from history's Claude metadata adapter. Source filename selection
// stays with the consumer; nested native facts keep their own observation scope.
func (state *State) catalog(batch *model.Batch, observation model.Observation, row map[string]json.RawMessage) {
	agent := rawString(row, "agentId", "agent_id")
	// A work journal's agentId names its actor, not a native Claude session.
	// Its started/result rows are operational records, not worker transcripts.
	if journalRow(row) {
		agent = ""
	}
	subagent := agent != "" || rawBool(row, "isSidechain")
	if subagent {
		parent := rawString(row, "sessionId", "session_id")
		observation.Scope.NativeThreadID = nil
		if agent != "" {
			observation.Scope.NativeThreadID = &agent
		}
		if parent != "" {
			observation.Scope.ParentNativeID = &parent
		}
	}
	key := catalogKey(observation.Scope)
	prior := state.Catalog[key]
	fields := model.Metadata{}
	if prior == nil {
		for _, field := range []model.MetadataField{model.MetadataCWD, model.MetadataModel, model.MetadataEffort, model.MetadataPermission, model.MetadataHistoricalBranch} {
			fields[field] = model.MetadataValue{State: model.MetadataUnknown}
		}
		fields[model.MetadataPriority] = model.MetadataValue{State: model.MetadataUnsupported}
	}
	known := func(field model.MetadataField, value string) { assemble.CatalogKnown(fields, field, value) }
	if subagent {
		known(model.MetadataSourceKind, "subagent")
		known(model.MetadataNativeThreadID, agent)
		known(model.MetadataParentNativeID, rawString(row, "sessionId", "session_id"))
	} else {
		known(model.MetadataNativeThreadID, rawString(row, "sessionId", "session_id"))
		if parent := rawString(row, "forkedFromId", "forked_from_id", "parentSessionId"); parent != "" {
			known(model.MetadataSourceKind, "fork")
			known(model.MetadataParentNativeID, parent)
		}
	}
	known(model.MetadataCWD, rawString(row, "cwd"))
	known(model.MetadataHistoricalBranch, rawString(row, "gitBranch", "git_branch"))
	known(model.MetadataProviderVersion, rawString(row, "version", "cliVersion"))
	effort := rawString(row, "effort")
	if effort == "" {
		effort = rawString(rawObject(row, "effort"), "level")
	}
	known(model.MetadataEffort, effort)
	known(model.MetadataPermission, rawString(row, "permissionMode"))
	known(model.MetadataOrigin, rawString(row, "entrypoint"))
	switch rawString(row, "type") {
	case "assistant":
		if name := rawString(rawObject(row, "message"), "model"); name != "<synthetic>" {
			known(model.MetadataModel, name)
		}
	case "permission-mode":
		known(model.MetadataPermission, rawString(row, "permissionMode", "mode"))
	case "custom-title":
		known(model.MetadataTitle, rawString(row, "customTitle", "title"))
	case "ai-title":
		if prior[model.MetadataTitle].Value.State != model.MetadataKnown {
			known(model.MetadataTitle, rawString(row, "aiTitle", "title"))
		}
	case "summary":
		if prior[model.MetadataTitle].Value.State != model.MetadataKnown {
			known(model.MetadataTitle, rawString(row, "summary"))
		}
	case "last-prompt":
		known(model.MetadataPreview, assemble.CatalogPreview(rawString(row, "lastPrompt")))
	case "user":
		message := rawObject(row, "message")
		if prior[model.MetadataPreview].Value.State != model.MetadataKnown && !rawBool(row, "isMeta") && (rawString(message, "role") == "" || rawString(message, "role") == "user") {
			preview := assemble.CatalogPreview(contentText(message["content"]))
			if !injectedCatalogPrompt(preview) {
				known(model.MetadataPreview, preview)
			}
		}
	}
	field, raw, stamp := assemble.CatalogTime(row, "timestamp", "createdAt", "updatedAt")
	next, changed := prior.Changes(fields, stamp)
	state.Catalog[key] = next
	if len(changed) != 0 || stamp != nil {
		obs := assemble.CatalogObservation(observation, changed, field, raw, stamp)
		meaning := model.TimeActivity
		if rawString(row, "type") == "assistant" || (rawString(row, "type") == "user" && claudeUserRowIsMessage(row)) {
			meaning = model.TimeConversation
		}
		assemble.CatalogActivity(&obs, meaning)
		batch.Observations = append(batch.Observations, obs)
	}
}

func catalogKey(scope model.Scope) string {
	scope.NativeTurnID = nil
	parent := ""
	if scope.ParentNativeID != nil {
		parent = *scope.ParentNativeID
	}
	return scopedKey(scope, "catalog", parent)
}

func injectedCatalogPrompt(text string) bool {
	text = strings.TrimSpace(strings.ToLower(text))
	for _, prefix := range []string{"<command-name>", "<command-message>", "<local-command", "<bash-stdout", "<bash-stderr", "<system-reminder>", "<task-notification>", "caveat:"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// User envelopes also carry task notifications, injected context and tool
// results. They must not make a quiet conversation look newly active.
func claudeUserRowIsMessage(row map[string]json.RawMessage) bool {
	if len(rawObject(row, "origin")) > 0 {
		return false
	}
	if rawBool(row, "isMeta") {
		return false
	}
	return !claudeContentIsToolResultOnly(rawObject(row, "message")["content"])
}

func claudeContentIsToolResultOnly(content json.RawMessage) bool {
	var blocks []map[string]json.RawMessage
	if len(content) == 0 || json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if rawString(block, "type") != "tool_result" {
			return false
		}
	}
	return true
}
