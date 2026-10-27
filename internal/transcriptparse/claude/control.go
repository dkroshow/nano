package claude

import (
	"encoding/json"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Control IDs identify one request within a requester/session namespace. They
// are neither tool call IDs nor native event IDs. Responses settle controls,
// never conversation turns or the execution outcome of the permitted tool.
type controlRequest struct {
	Subtype        string             `json:"subtype"`
	ItemID         string             `json:"itemId,omitempty"`
	Request        model.Observation  `json:"request"`
	RequestDigest  string             `json:"requestDigest"`
	Response       *model.Observation `json:"response,omitempty"`
	ResponseDigest string             `json:"responseDigest,omitempty"`
}

func controlNamespace(direction, subtype string) string {
	if direction == "send" || direction == "recv" {
		return direction
	}
	switch subtype {
	case "initialize", "interrupt", "set_model", "set_permission_mode":
		return "send"
	case "can_use_tool", "elicitation", "request_user_dialog", "hook_callback", "mcp_message":
		return "recv"
	}
	return "unknown"
}

func controlKey(scope model.Scope, namespace, id string) string {
	scope.NativeTurnID = nil
	return scopedKey(scope, "control/"+namespace, id)
}

func selectFields(fields map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, key := range keys {
		if raw, ok := fields[key]; ok {
			out[key] = raw
		}
	}
	return out
}

func (state *State) control(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) model.Disposition {
	obs.NativeEventID = ""
	obs.Scope.NativeTurnID = nil
	if obs.Scope.NativeThreadID == nil {
		obs.Scope.Kind = model.ScopeConnection
	}
	obs.Times = append([]model.TimeObservation(nil), obs.Times...)
	for i := range obs.Times {
		obs.Times[i].Scope = obs.Scope
	}
	switch rawString(row, "type") {
	case "control_request":
		return state.controlRequest(batch, obs, row)
	case "control_response":
		return state.controlResponse(batch, obs, row)
	default:
		obs.Kind = model.ObservationUnknown
		obs.Detail = mustJSON(selectFields(row, "type", "request_id"))
		batch.Observations = append(batch.Observations, obs)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_control_schema", obs.Source))
		return model.RecordUnknown
	}
}

func (state *State) controlRequest(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) model.Disposition {
	request := rawObject(row, "request")
	id, subtype := rawString(row, "request_id"), rawString(request, "subtype")
	obs.Source.BlockPath = nativePath(obs.Source.BlockPath, "$.request")
	fields := selectFields(request, "subtype")
	interaction, verified := false, true
	switch subtype {
	case "initialize":
		fields = selectFields(request, "subtype", "protocolVersion")
	case "interrupt":
	case "set_model":
		fields = selectFields(request, "subtype", "model")
	case "can_use_tool":
		interaction = true
		fields = selectFields(request, "subtype", "tool_name", "tool_use_id", "input", "permission_suggestions", "decision_reason", "blocked_path")
	case "elicitation":
		interaction = true
		fields = selectFields(request, "subtype", "server_name", "message", "requested_schema", "mode", "url", "elicitation_id")
	case "request_user_dialog":
		interaction, verified = true, false
		fields = selectFields(request, "subtype", "dialog_kind", "payload")
	case "set_permission_mode":
		verified = false
		fields = selectFields(request, "subtype", "mode")
	case "hook_callback":
		verified = false
		fields = selectFields(request, "subtype", "callback_id", "input")
	case "mcp_message":
		verified = false
		fields = selectFields(request, "subtype", "server_name", "message")
	default:
		verified = false
	}
	namespace := controlNamespace(obs.Source.Direction, subtype)
	obs.Detail = mustJSON(map[string]any{"type": "control_request", "request_id": id, "requester": namespace, "request": fields})
	if id == "" || subtype == "" || request == nil {
		obs.Kind = model.ObservationUnknown
		batch.Observations = append(batch.Observations, obs)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_control_request", obs.Source))
		return model.RecordMalformed
	}
	key, digest := controlKey(obs.Scope, namespace, id), rowDigest(row)
	if prior, ok := state.Controls[key]; ok {
		if prior.RequestDigest != digest {
			batch.Observations = append(batch.Observations, obs)
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("control_request_conflict", obs.Source))
			return model.RecordUnknown
		}
		state.controlReplay(batch, prior, obs, prior.Request)
		if !verified {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_control_schema", obs.Source))
			return model.RecordUnknown
		}
		return model.RecordNonContent
	}
	entry := controlRequest{Subtype: subtype, Request: obs, RequestDigest: digest}
	if interaction {
		item, matched := state.questionInvocation(obs.Scope, rawString(request, "tool_name"), rawString(request, "tool_use_id"))
		if !matched {
			state.NextItem++
			item = model.Item{ID: sourceKey(obs.Source, "control-item"), SourceSeq: state.NextItem, Scope: obs.Scope, Type: model.ItemInteraction, Category: model.CategorySystem, Role: model.RoleSystem,
				ToolName: rawString(request, "tool_name"), CallID: rawString(request, "tool_use_id"), SourceLine: int(obs.Source.Line), SourceLineSHA256: obs.Source.SHA256, SourceRowType: "control_request", SourcePayloadType: subtype}
			item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
		}
		item.Aliases = append(append([]model.Alias(nil), item.Aliases...), model.Alias{Namespace: "claude.controlRequestId", Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}})
		if !matched && item.CallID != "" {
			item.Aliases = append(item.Aliases, model.Alias{Namespace: "claude.toolUseId", Value: mustJSON(item.CallID), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}})
		}
		obs.Kind, obs.Outcome = model.ObservationSnapshot, model.OutcomeRunning
		if matched {
			obs.Kind = model.ObservationMetadata
		}
		complete := true // the request payload is complete; its decision is pending.
		obs.Complete = &complete
		item.Detail = mustJSON(map[string]any{"state": "pending", "request": fields})
		state.observeItem(batch, &item, &obs)
		entry.ItemID = item.ID
	} else {
		batch.Observations = append(batch.Observations, obs)
	}
	entry.Request = obs
	state.Controls[key] = entry
	if !verified {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_control_schema", obs.Source))
		return model.RecordUnknown
	}
	return model.RecordNonContent
}

func (state *State) controlResponse(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) model.Disposition {
	response := rawObject(row, "response")
	id, subtype := rawString(response, "request_id"), rawString(response, "subtype")
	body := rawObject(response, "response")
	obs.Source.BlockPath = nativePath(obs.Source.BlockPath, "$.response")
	// Unknown control replies can contain credentials. Retain only protocol and
	// known decision fields until a matched request establishes the response schema.
	detail := selectFields(response, "request_id", "subtype", "error")
	detail["response"] = mustJSON(selectFields(body, "behavior", "updatedInput", "message", "action", "content", "status"))
	obs.Detail = mustJSON(detail)
	if id == "" || (subtype != "success" && subtype != "error") {
		batch.Observations = append(batch.Observations, obs)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_control_response", obs.Source))
		return model.RecordMalformed
	}
	var key string
	var entry controlRequest
	count := 0
	for _, namespace := range []string{"send", "recv", "unknown"} {
		if obs.Source.Direction == namespace && namespace != "unknown" {
			continue
		}
		candidateKey := controlKey(obs.Scope, namespace, id)
		if candidate, ok := state.Controls[candidateKey]; ok {
			key, entry = candidateKey, candidate
			count++
		}
	}
	if count != 1 {
		code := "unresolved_control_response"
		if count > 1 {
			code = "ambiguous_control_namespace"
		}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic(code, obs.Source))
		batch.Observations = append(batch.Observations, obs)
		return model.RecordUnknown
	}
	digest := rowDigest(row)
	if entry.Response != nil {
		if entry.ResponseDigest != digest {
			batch.Observations = append(batch.Observations, obs)
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("control_response_conflict", obs.Source))
			return model.RecordUnknown
		}
		state.controlReplay(batch, entry, obs, *entry.Response)
		return model.RecordNonContent
	}
	if entry.Subtype == "initialize" {
		detail["response"] = mustJSON(selectFields(body, "models", "commands", "pending_permission_requests", "pending_user_dialog_requests"))
		for _, field := range []string{"pending_permission_requests", "pending_user_dialog_requests"} {
			if raw := body[field]; raw != nil {
				var pending []json.RawMessage
				if json.Unmarshal(raw, &pending) != nil || len(pending) != 0 {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_pending_controls", obs.Source))
					break
				}
			}
		}
	}
	if entry.Subtype == "interrupt" {
		detail["response"] = mustJSON(selectFields(body, "still_queued"))
	}
	detail["requestObservationRef"] = mustJSON(entry.Request.ID)
	detail["requestSubtype"] = mustJSON(entry.Subtype)
	obs.Detail = mustJSON(detail)
	obs.Kind, obs.Outcome = model.ObservationResult, model.OutcomeCompleted
	if subtype == "error" {
		obs.Outcome = model.OutcomeFailed
	} else if entry.ItemID != "" {
		obs.Outcome = controlDecision(entry.Subtype, body)
		if obs.Outcome == model.OutcomeUnknown {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_control_decision", obs.Source))
		}
	}
	if entry.ItemID != "" {
		item := state.Items[entry.ItemID]
		// The decision is metadata on the invocation; it does not replace its
		// generated question or tool-argument content with an empty string.
		obs.Kind = model.ObservationMetadata
		var itemDetail map[string]json.RawMessage
		_ = json.Unmarshal(item.Detail, &itemDetail)
		itemDetail["state"], itemDetail["response"] = mustJSON("answered"), mustJSON(detail)
		item.Detail = mustJSON(itemDetail)
		state.observeItem(batch, &item, &obs)
	} else {
		batch.Observations = append(batch.Observations, obs)
	}
	relation := model.Relation{Kind: model.RelationResultOf, Scope: obs.Scope, FromObservation: obs.ID, ToObservation: entry.Request.ID, ObservationRefs: []string{entry.Request.ID, obs.ID}}
	state.controlRelation(batch, entry.ItemID, relation)
	entry.Response, entry.ResponseDigest = &obs, digest
	state.Controls[key] = entry
	return model.RecordNonContent
}

func controlDecision(subtype string, body map[string]json.RawMessage) model.Outcome {
	switch subtype {
	case "can_use_tool":
		switch rawString(body, "behavior") {
		case "allow":
			return model.OutcomeCompleted
		case "deny":
			return model.OutcomeDeclined
		}
	case "elicitation":
		switch rawString(body, "action") {
		case "accept":
			return model.OutcomeCompleted
		case "decline":
			return model.OutcomeDeclined
		case "cancel":
			return model.OutcomeCancelled
		}
	case "request_user_dialog":
		if rawString(body, "status") == "cancelled" {
			return model.OutcomeCancelled
		}
	}
	return model.OutcomeUnknown
}

func (state *State) observeItem(batch *model.Batch, item *model.Item, obs *model.Observation) {
	obs.Scope = item.Scope
	obs.Times = append([]model.TimeObservation(nil), obs.Times...)
	for i := range obs.Times {
		obs.Times[i].Scope, obs.Times[i].ObservationRef = item.Scope, obs.ID
	}
	var diagnostics []model.Diagnostic
	*item, diagnostics = assemble.Observe(*item, *obs)
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	state.Items[item.ID] = *item
	copy := *item
	batch.Changes = append(batch.Changes, model.Change{Item: &copy})
	batch.Observations = append(batch.Observations, *obs)
}

func (state *State) controlRelation(batch *model.Batch, itemID string, relation model.Relation) {
	if itemID != "" {
		item := state.Items[itemID]
		item.Relations = append(append([]model.Relation(nil), item.Relations...), relation)
		state.Items[itemID] = item
		batch.Changes = append(batch.Changes, model.Change{Item: &item})
	}
	batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
}

func (state *State) controlReplay(batch *model.Batch, entry controlRequest, obs, earlier model.Observation) {
	obs.Kind, obs.Outcome, obs.Complete = model.ObservationMetadata, "", nil
	if entry.ItemID != "" {
		item := state.Items[entry.ItemID]
		state.observeItem(batch, &item, &obs)
	} else {
		batch.Observations = append(batch.Observations, obs)
	}
	relation := model.Relation{Kind: model.RelationReplayOf, Scope: obs.Scope, FromObservation: obs.ID, ToObservation: earlier.ID, ObservationRefs: []string{earlier.ID, obs.ID}}
	state.controlRelation(batch, entry.ItemID, relation)
}
