package codex

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"strings"
)

// Extracted from history's native rollout metadata adapter. This runs on the
// payload already decoded by the file reader, before transcript assembly.
func (state *State) catalog(batch *model.Batch, obs model.Observation, row, payload map[string]json.RawMessage) {
	key := threadKey(obs.Scope)
	prior := state.Catalog[key]
	fields := model.Metadata{}
	if prior == nil {
		for _, field := range []model.MetadataField{model.MetadataCWD, model.MetadataModel, model.MetadataEffort, model.MetadataPriority, model.MetadataPermission, model.MetadataHistoricalBranch} {
			fields[field] = model.MetadataValue{State: model.MetadataUnknown}
		}
	}
	known := func(field model.MetadataField, value string) { assemble.CatalogKnown(fields, field, value) }
	switch rawString(row, "type") {
	case "session_meta":
		known(model.MetadataNativeThreadID, rawString(payload, "id", "thread_id", "threadId"))
		known(model.MetadataCWD, rawString(payload, "cwd"))
		known(model.MetadataOrigin, rawString(payload, "originator"))
		known(model.MetadataProviderVersion, rawString(payload, "cli_version", "cliVersion", "version"))
		known(model.MetadataTitle, rawString(payload, "name", "title"))
		known(model.MetadataPreview, assemble.CatalogPreview(rawString(payload, "preview")))
		known(model.MetadataHistoricalBranch, firstRawString([]map[string]json.RawMessage{payload, rawObject(payload, "git"), rawObject(payload, "gitInfo")}, "git_branch", "gitBranch", "branch"))
		threadSource := rawString(payload, "thread_source", "threadSource")
		source := rawObject(payload, "source")
		kind := ""
		if codexSourceIsGuardian(source) {
			kind = "guardian"
		} else if strings.EqualFold(threadSource, "subagent") || codexSourceIsSubagent(source) {
			kind = "subagent"
		} else if strings.EqualFold(threadSource, "fork") || codexSourceIsFork(source) {
			kind = "fork"
		}
		if kind != "" {
			known(model.MetadataSourceKind, kind)
			known(model.MetadataParentNativeID, codexParentThreadID(payload, source))
		}
	case "turn_context":
		settings := codexCollaborationSettings(payload)
		known(model.MetadataCWD, rawString(payload, "cwd"))
		known(model.MetadataModel, firstRawString([]map[string]json.RawMessage{payload, settings}, "model"))
		known(model.MetadataEffort, firstRawString([]map[string]json.RawMessage{payload, settings}, "effort", "reasoning_effort", "reasoningEffort"))
		known(model.MetadataPriority, firstRawString([]map[string]json.RawMessage{payload, settings}, "service_tier", "serviceTier", "priority"))
		permission := rawString(payload, "approval_policy", "approvalPolicy", "permission")
		if permission == "" {
			sandbox := rawObject(payload, "sandbox_policy")
			if len(sandbox) == 0 {
				sandbox = rawObject(payload, "sandboxPolicy")
			}
			permission = rawString(sandbox, "type", "mode")
		}
		known(model.MetadataPermission, permission)
	case "event_msg":
		if prior[model.MetadataPreview].Value.State != model.MetadataKnown && rawString(payload, "type") == "user_message" {
			text := rawString(payload, "message", "text")
			if !isCodexSyntheticPrompt(text) {
				known(model.MetadataPreview, assemble.CatalogPreview(text))
			}
		}
	case "response_item":
		if prior[model.MetadataPreview].Value.State != model.MetadataKnown && rawString(payload, "type") == "message" && rawString(payload, "role") == "user" {
			if text, conversation := codexResponseUserText(payload); conversation {
				known(model.MetadataPreview, assemble.CatalogPreview(text))
			}
		}
	}
	field, raw, stamp := assemble.CatalogTime(row, "timestamp", "ts")
	if stamp == nil {
		field, raw, stamp = assemble.CatalogTime(payload, "timestamp", "created_at", "createdAt", "updated_at", "updatedAt")
		if field != "" {
			field = "payload." + field
		}
	}
	next, changed := prior.Changes(fields, stamp)
	state.Catalog[key] = next
	kind := rawString(row, "type")
	activity := kind == "session_meta" || kind == "turn_context" || kind == "event_msg" || kind == "response_item" || kind == "compacted" || kind == "world_state" || kind == "inter_agent_communication_metadata"
	if len(changed) != 0 || (activity && stamp != nil) {
		catalog := assemble.CatalogObservation(obs, changed, field, raw, stamp)
		if activity {
			meaning := model.TimeActivity
			if (kind == "event_msg" && codexEventIsMessage(payload)) || (kind == "response_item" && codexResponseItemIsMessage(payload)) {
				meaning = model.TimeConversation
			}
			assemble.CatalogActivity(&catalog, meaning)
		}
		batch.Observations = append(batch.Observations, catalog)
	}
}

func codexSourceIsSubagent(source map[string]json.RawMessage) bool {
	if rawString(source, "type") == "subagent" {
		return true
	}
	return len(rawObject(source, "subagent")) > 0
}

func codexSourceIsGuardian(source map[string]json.RawMessage) bool {
	return rawString(rawObject(source, "subagent"), "other") == "guardian"
}

func codexSourceIsFork(source map[string]json.RawMessage) bool {
	if rawString(source, "type") == "fork" {
		return true
	}
	return len(rawObject(source, "fork")) > 0
}

func codexParentThreadID(payload, source map[string]json.RawMessage) string {
	if parent := rawString(payload, "parent_thread_id", "parentThreadId", "forked_from_id", "forkedFromId"); parent != "" {
		return parent
	}
	for _, kind := range []string{"subagent", "fork"} {
		kindObject := rawObject(source, kind)
		if parent := rawString(kindObject, "parent_thread_id", "parentThreadId", "forked_from_id", "forkedFromId"); parent != "" {
			return parent
		}
		spawn := rawObject(kindObject, "thread_spawn")
		if len(spawn) == 0 {
			spawn = rawObject(kindObject, "threadSpawn")
		}
		if parent := rawString(spawn, "parent_thread_id", "parentThreadId"); parent != "" {
			return parent
		}
	}
	return ""
}

// Telemetry and trusted harness input must not advance conversation activity.
// A native user-role envelope alone does not establish authorship.
func codexEventIsMessage(payload map[string]json.RawMessage) bool {
	switch rawString(payload, "type") {
	case "agent_message":
		return true
	case "user_message":
		return !isCodexSyntheticPrompt(rawString(payload, "message", "text"))
	default:
		return false
	}
}

func codexResponseItemIsMessage(payload map[string]json.RawMessage) bool {
	if rawString(payload, "type") != "message" {
		return false
	}
	switch rawString(payload, "role") {
	case "assistant":
		return true
	case "user":
		_, conversation := codexResponseUserText(payload)
		return conversation
	default:
		return false
	}
}
