package grok

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) loadState(batch *model.Batch, obs *model.Observation, request rpcRequest, result map[string]json.RawMessage) rpcRequest {
	if result == nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_load_result", obs.Source))
		return request
	}
	scope := request.Scope
	scope.Kind, scope.NativeTurnID, scope.TurnKey, scope.ItemID = model.ScopeThread, nil, "", ""
	if session := documentString(result, "sessionId"); session != "" {
		if scope.NativeThreadID != nil && *scope.NativeThreadID != session {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("rpc_scope_conflict", obs.Source))
			return request
		}
		scope.NativeThreadID = &session
	}
	if scope.NativeThreadID == nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unscoped_load_state", obs.Source))
		return request
	}
	obs.Scope, request.Scope = scope, scope
	meta := fieldsOf(result["_meta"])
	var prompt *string
	if raw, present := meta["x.ai/runningPromptId"]; present && json.Unmarshal(raw, &prompt) != nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_running_prompt", obs.Source))
		return request
	}
	key := sessionKey(scope)
	if prompt == nil || *prompt == "" {
		// Absence changes reconnect binding only. Native conversation outcome
		// and native end time require their own terminal evidence.
		delete(state.RunningPrompts, key)
		return request
	}
	scope.Kind, scope.NativeTurnID = model.ScopeTurn, prompt
	state.RunningPrompts[key] = runningPrompt{Scope: scope, ObservationRef: obs.ID}
	return request
}
