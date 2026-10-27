package codex

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func metadataNotification(method string) bool {
	switch method {
	case "account/login/completed", "account/rateLimits/updated", "account/updated",
		"app/list/updated", "externalAgentConfig/import/completed", "externalAgentConfig/import/progress",
		"fs/changed", "fuzzyFileSearch/sessionCompleted", "fuzzyFileSearch/sessionUpdated",
		"hook/started", "hook/completed", "mcpServer/oauthLogin/completed", "mcpServer/startupStatus/updated",
		"model/rerouted", "model/safetyBuffering/updated", "model/verification", "remoteControl/status/changed",
		"skills/changed", "thread/archived", "thread/closed", "thread/deleted", "thread/goal/cleared",
		"thread/goal/updated", "thread/name/updated", "thread/settings/updated", "thread/started",
		"thread/status/changed", "thread/unarchived", "turn/moderationMetadata", "windowsSandbox/setupCompleted":
		return true
	}
	return false
}

func noticeNotification(method string) bool {
	switch method {
	case "error", "warning", "configWarning", "deprecationNotice", "guardianWarning", "windows/worldWritableWarning", "thread/compacted":
		return true
	}
	return false
}

func notificationScope(scope model.Scope, method string, params map[string]json.RawMessage) model.Scope {
	// Metadata owns only identities in this notification. Caller turn defaults
	// cannot turn an account, hook or thread status change into turn completion.
	scope.Kind, scope.NativeThreadID, scope.NativeTurnID, scope.TurnKey, scope.ItemID = model.ScopeConnection, nil, nil, "", ""
	if id := rawString(params, "threadId"); id != "" {
		scope.Kind, scope.NativeThreadID = model.ScopeThread, &id
	}
	if id := rawString(params, "turnId"); id != "" {
		scope.Kind, scope.NativeTurnID = model.ScopeTurn, &id
	}
	if method == "thread/started" {
		thread := rawObject(params, "thread")
		if id := rawString(thread, "id"); id != "" {
			scope.Kind, scope.NativeThreadID = model.ScopeThread, &id
		}
		if parent := rawString(thread, "parentThreadId"); parent != "" {
			scope.ParentNativeID = &parent
		}
	}
	return scope
}

func (state *State) notification(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage, method string, params map[string]json.RawMessage) model.Disposition {
	obs.Detail = mustJSON(row)
	if !metadataNotification(method) && !noticeNotification(method) {
		return state.unknown(batch, obs, "unknown_method")
	}
	obs = bindObservation(obs, notificationScope(obs.Scope, method, params))
	if metadataNotification(method) {
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	}
	item := model.Item{Type: model.ItemNotice, Category: model.CategorySystem, Role: model.RoleSystem, Text: nativeText(params, "message", "summary"), Detail: obs.Detail, SourceRowType: "wire", SourcePayloadType: method}
	if method == "error" {
		item.Category, item.Text = model.CategoryError, nativeText(rawObject(params, "error"), "message")
	}
	if method == "thread/compacted" {
		item.Type = model.ItemCompaction
	}
	complete := true
	obs.Kind, obs.Text, obs.Complete = model.ObservationSnapshot, item.Text, &complete
	state.emitScopedItem(batch, obs, item)
	return model.RecordSemantic
}

func (state *State) emitScopedItem(batch *model.Batch, obs model.Observation, item model.Item) model.Item {
	if obs.Scope.NativeTurnID != nil {
		turn := state.turn(obs.Scope, obs.Source, false, false)
		return state.emit(batch, turn, obs, item, false)
	}
	state.NextItem++
	item.ID, item.SourceSeq = sourceKey(obs.Source, "item"), state.NextItem
	item.Scope = obs.Scope
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	item.SourceLine, item.SourceLineSHA256 = int(obs.Source.Line), obs.Source.SHA256
	return state.observeRequestItem(batch, item, obs)
}
