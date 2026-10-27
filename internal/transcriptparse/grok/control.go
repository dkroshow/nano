package grok

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"time"
)

type rpcRequest struct {
	ID             json.RawMessage `json:"id"`
	Method         string          `json:"method"`
	Scope          model.Scope     `json:"scope"`
	ObservationRef string          `json:"observationRef"`
	RequestDigest  string          `json:"requestDigest"`
	ReplyRef       string          `json:"replyRef,omitempty"`
	ReplyDigest    string          `json:"replyDigest,omitempty"`
}
type runningPrompt struct {
	Scope          model.Scope `json:"scope"`
	Text           string      `json:"text"`
	ObservationRef string      `json:"observationRef"`
}

func reverseMethod(method string) bool {
	return method == "session/request_permission" || method == "_x.ai/exit_plan_mode" || method == "_x.ai/ask_user_question"
}
func rpcKey(scope model.Scope, namespace string, id json.RawMessage) string {
	return string(mustJSON([]any{scope.Source, scope.Incarnation, scope.ConnectionID, namespace, id}))
}
func sessionKey(scope model.Scope) string {
	scope.NativeTurnID = nil
	return scopedKey(scope, "session", "")
}
func fieldsOf(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return fields
}
func pickFields(fields map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, key := range keys {
		if value, ok := fields[key]; ok {
			out[key] = value
		}
	}
	return out
}
func (state *State) control(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) model.Disposition {
	method := documentString(row, "method")
	params := fieldsOf(row["params"])
	scope := scopeFrom(input, decodedUpdate{SessionID: documentString(params, "sessionId"), PromptID: documentString(params, "promptId")}, model.ScopeConnection)
	if scope.NativeThreadID != nil {
		scope.Kind = model.ScopeThread
	}
	obs := model.Observation{ID: sourceKey(source, "control"), Scope: scope, Source: source, Kind: model.ObservationMetadata, Capture: model.CaptureComplete}
	obs.Times = callerClocks(input, scope, obs.ID)
	detail := map[string]any{"method": method, "rpcId": row["id"]}
	if len(row["id"]) > 0 && !rpcScalarID(row["id"]) {
		obs.Kind = model.ObservationUnknown
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_rpc_id", source))
		return state.emitControl(batch, obs, detail, model.RecordUnknown)
	}
	if len(row["id"]) > 0 && method != "" {
		namespace := "client"
		if reverseMethod(method) || source.Direction == "recv" {
			namespace = "server"
		}
		if reverseMethod(method) {
			detail["params"] = params
		} else {
			detail["params"] = pickFields(params, "sessionId", "cwd", "protocolVersion")
		}
		if reverseMethod(method) && source.Direction == "send" {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unexpected_rpc_direction", source))
			return state.emitControl(batch, obs, detail, model.RecordUnknown)
		}
		key, digest := rpcKey(scope, namespace, row["id"]), rpcDigest(row)
		if earlier, found := state.Requests[key]; found && earlier.ReplyRef == "" {
			if earlier.RequestDigest != digest {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("rpc_request_conflict", source))
			} else {
				obs.Scope = earlier.Scope
				rpcReplay(batch, obs, earlier.Scope, earlier.ObservationRef)
			}
			return state.emitControl(batch, obs, detail, model.RecordNonContent)
		}
		if reverseMethod(method) {
			state.startInteraction(batch, &obs, method, params, row["id"])
		}
		state.Requests[key] = rpcRequest{ID: mustJSON(row["id"]), Method: method, Scope: obs.Scope, ObservationRef: obs.ID, RequestDigest: digest}
	} else if len(row["id"]) > 0 {
		namespace := "client"
		if source.Direction == "send" {
			namespace = "server"
		}
		requestKey := rpcKey(scope, namespace, row["id"])
		request, matched := state.Requests[requestKey]
		if source.Direction == "" {
			server, serverMatched := state.Requests[rpcKey(scope, "server", row["id"])]
			if matched && serverMatched {
				matched = false
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_rpc_namespace", source))
			} else if serverMatched {
				request, matched = server, true
				requestKey = rpcKey(scope, "server", row["id"])
			}
		}
		result := fieldsOf(row["result"])
		if matched {
			obs.Scope = request.Scope
			detail["requestObservationRef"], detail["method"] = request.ObservationRef, request.Method
			if reverseMethod(request.Method) {
				detail["result"] = pickFields(result, "outcome", "optionId", "feedback", "answers")
			} else {
				detail["result"] = safeResult(result)
			}
		} else {
			detail["result"] = safeResult(result)
		}
		if failure := fieldsOf(row["error"]); failure != nil {
			detail["error"] = pickFields(failure, "code")
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("rpc_error", source))
		}
		if matched {
			digest := rpcDigest(row)
			if request.ReplyRef != "" {
				if request.ReplyDigest != digest {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("rpc_response_conflict", source))
				} else {
					rpcReplay(batch, obs, request.Scope, request.ReplyRef)
				}
				return state.emitControl(batch, obs, detail, model.RecordNonContent)
			}
			request.ReplyRef, request.ReplyDigest = obs.ID, digest
			state.Requests[requestKey] = request
			relation := model.Relation{Kind: model.RelationResultOf, Scope: request.Scope, FromObservation: obs.ID, ToObservation: request.ObservationRef, ObservationRefs: []string{request.ObservationRef, obs.ID}}
			batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
			if reverseMethod(request.Method) {
				state.answerInteraction(batch, &obs, request, result, fieldsOf(row["error"]) != nil)
			}
		}
		if matched && request.Method == "session/load" && row["error"] == nil && source.Direction != "send" {
			state.Requests[requestKey] = state.loadState(batch, &obs, request, result)
		} else if fieldsOf(result["_meta"])["x.ai/runningPromptId"] != nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unproven_load_state", source))
		}
		if result != nil && documentString(result, "stopReason") != "" {
			if !matched || request.Method != "session/prompt" || row["error"] != nil || source.Direction == "send" {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unproven_rpc_terminal", source))
			} else {
				native := fieldsOf(result["_meta"])
				prompt := documentString(native, "promptId")
				target := obs.Scope
				if prompt != "" {
					target.NativeTurnID = &prompt
				}
				state.controlTerminal(batch, input, source, target, documentString(result, "stopReason"), obs.ID, ".result")
			}
		}
	} else {
		switch method {
		case "_x.ai/queue/changed":
			detail["params"] = pickFields(params, "sessionId", "runningPromptId", "runningText", "entries")
			if source.Direction == "send" {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unexpected_control_direction", source))
				break
			}
			if documentString(params, "sessionId") == "" || scope.NativeThreadID == nil {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unscoped_queue", source))
				break
			}
			var prompt *string
			if raw, present := params["runningPromptId"]; present && json.Unmarshal(raw, &prompt) != nil {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_running_prompt", source))
				break
			}
			if prompt != nil && *prompt != "" {
				known := scope
				known.NativeTurnID = prompt
				known.Kind = model.ScopeTurn
				state.RunningPrompts[sessionKey(scope)] = runningPrompt{Scope: known, Text: documentString(params, "runningText"), ObservationRef: obs.ID}
			} else {
				delete(state.RunningPrompts, sessionKey(scope))
			}
		case "_x.ai/session/prompt_complete":
			detail["params"] = pickFields(params, "sessionId", "promptId", "stopReason")
			if source.Direction == "send" {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unexpected_control_direction", source))
			} else {
				state.controlTerminal(batch, input, source, scope, documentString(params, "stopReason"), obs.ID, ".params")
			}
		case "initialized":
		default:
			obs.Kind = model.ObservationUnknown
			obs.Detail = mustJSON(row)
			batch.Observations = append(batch.Observations, obs)
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unsupported_envelope", source))
			return model.RecordUnknown
		}
	}
	return state.emitControl(batch, obs, detail, model.RecordNonContent)
}
func safeResult(result map[string]json.RawMessage) map[string]json.RawMessage {
	safe := pickFields(result, "protocolVersion", "sessionId", "subscription_tier", "subscriptionTier", "stopReason")
	if agent := fieldsOf(result["agentInfo"]); agent != nil {
		safe["agentInfo"] = mustJSON(pickFields(agent, "name", "title", "version"))
	}
	if config := fieldsOf(result["config"]); config != nil {
		safe["config"] = mustJSON(pickFields(config, "currentPeriod", "creditUsagePercent", "used", "monthlyLimit"))
	}
	meta := pickFields(fieldsOf(result["_meta"]), "modelState", "x.ai/sessionConfig", "x.ai/sessionDetail", "x.ai/runningPromptId", "promptId")
	if len(meta) > 0 {
		safe["_meta"] = mustJSON(meta)
	}
	return safe
}
func (state *State) controlTerminal(batch *model.Batch, input model.Input, source model.SourceRef, scope model.Scope, reason, ref, path string) {
	if scope.NativeTurnID == nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unscoped_terminal", source))
		return
	}
	if scope.NativeThreadID == nil {
		var found *model.Scope
		for _, turn := range state.Turns {
			if turn.Scope.NativeThreadID == nil || !sameConnection(scope, turn.Scope) {
				continue
			}
			if turn.Scope.NativeTurnID != nil && *turn.Scope.NativeTurnID == *scope.NativeTurnID {
				copy := turn.Scope
				if found != nil && !sameSession(*found, copy) {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_terminal_scope", source))
					return
				}
				found = &copy
			}
		}
		for _, running := range state.RunningPrompts {
			if running.Scope.NativeThreadID == nil || !sameConnection(scope, running.Scope) {
				continue
			}
			if running.Scope.NativeTurnID != nil && *running.Scope.NativeTurnID == *scope.NativeTurnID {
				copy := running.Scope
				if found != nil && !sameSession(*found, copy) {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_terminal_scope", source))
					return
				}
				found = &copy
			}
		}
		if found == nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unscoped_terminal", source))
			return
		}
		scope.NativeThreadID = found.NativeThreadID
	}
	scopedInput := input
	scopedInput.Scope = scope
	source = nativeChild(source, path)
	state.apply(batch, scopedInput, source, decodedUpdate{Kind: kindTerminal, SessionID: *scope.NativeThreadID, PromptID: *scope.NativeTurnID, Status: reason, Payload: mustJSON(map[string]any{"controlObservationRef": ref, "stopReason": reason})})
}

func nativeChild(source model.SourceRef, suffix string) model.SourceRef {
	if source.BlockPath == "" {
		source.BlockPath = "$"
	}
	source.BlockPath += suffix
	return source
}

func sameConnection(a, b model.Scope) bool {
	return a.Provider == b.Provider && a.Source == b.Source && a.Incarnation == b.Incarnation && a.ConnectionID == b.ConnectionID
}
func captureClock(raw json.RawMessage, scope model.Scope, source model.SourceRef) model.Observation {
	obs := model.Observation{ID: sourceKey(source, "capture"), Kind: model.ObservationMetadata, Scope: scope, Source: source, Detail: mustJSON(raw), Capture: model.CaptureComplete}
	fields := fieldsOf(raw)
	if value, ok := fields["ts"]; ok {
		stamp := model.TimeObservation{Scope: scope, Class: model.TimeCapture, Raw: mustJSON(value), SourceField: "capture.ts", Meaning: model.TimeEvent, Precision: "capture_string", ObservationRef: obs.ID}
		if parsed, err := time.Parse(time.RFC3339Nano, documentString(fields, "ts")); err == nil {
			parsed = parsed.UTC()
			stamp.Value = &parsed
		}
		obs.Times = []model.TimeObservation{stamp}
	}
	return obs
}
