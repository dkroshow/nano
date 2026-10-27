package codex

import (
	"encoding/json"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) clientRequest(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) model.Disposition {
	id, valid := rpcID(row["id"])
	if !valid {
		return state.unknown(batch, obs, "invalid_rpc_id")
	}
	if obs.Scope.NativeThreadID == nil {
		obs.Scope.Kind = model.ScopeConnection
		obs = bindObservation(obs, obs.Scope)
	}
	key, fingerprint := rpcKey(obs.Scope, "client", id), rpcFingerprint(row)
	if earlier, found := state.Requests[key]; found && earlier.ReplyRef == "" {
		if earlier.Fingerprint != fingerprint {
			return state.unknown(batch, obs, "rpc_request_conflict")
		}
		state.replyReceipt(batch, obs, earlier.Scope, "", earlier.ObservationRef)
		return model.RecordNonContent
	}
	state.Requests[key] = rpcRequest{Method: rawString(row, "method"), Scope: obs.Scope, ObservationRef: obs.ID, Fingerprint: fingerprint}
	batch.Observations = append(batch.Observations, obs)
	return model.RecordNonContent
}

func (state *State) reply(batch *model.Batch, obs model.Observation, row map[string]json.RawMessage) model.Disposition {
	id, valid := rpcID(row["id"])
	if !valid {
		return state.unknown(batch, obs, "invalid_rpc_id")
	}
	var key string
	var request rpcRequest
	matches := 0
	for _, namespace := range []string{"client", "server"} {
		if obs.Source.Direction == "send" && namespace != "server" || obs.Source.Direction == "recv" && namespace != "client" {
			continue
		}
		candidate := rpcKey(obs.Scope, namespace, id)
		if pending, found := state.Requests[candidate]; found {
			key, request, matches = candidate, pending, matches+1
		}
	}
	if matches == 0 {
		if obs.Source.Direction == "recv" && row["error"] == nil && snapshotThread(row) != nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unresolved_snapshot_response", obs.Source))
			return state.threadSnapshot(batch, obs, row, "")
		}
		return state.unknown(batch, obs, "unresolved_rpc_response")
	}
	if matches != 1 {
		return state.unknown(batch, obs, "ambiguous_rpc_namespace")
	}
	if !rpcScopeCompatible(obs.Scope, request.Scope) {
		return state.unknown(batch, obs, "rpc_scope_conflict")
	}
	result := replyFields(request.Method, rawObject(row, "result"))
	detail := map[string]any{"method": request.Method, "id": id, "result": result, "requestObservationRef": request.ObservationRef}
	if failure := rawObject(row, "error"); failure != nil {
		detail["error"] = selectFields(failure, "code")
	}
	obs.Detail = mustJSON(detail)
	fingerprint := rpcFingerprint(row)
	if request.ReplyRef != "" {
		if request.ReplyFingerprint != fingerprint {
			return state.unknown(batch, obs, "rpc_response_conflict")
		}
		itemID := request.ItemID
		if request.ResultItemID != "" {
			itemID = request.ResultItemID
		}
		state.replyReceipt(batch, obs, request.Scope, itemID, request.ReplyRef)
		return model.RecordNonContent
	}
	obs = bindObservation(obs, request.Scope)
	if row["error"] != nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("rpc_error", obs.Source))
	}
	relation := model.Relation{Kind: model.RelationResultOf, Scope: request.Scope, FromObservation: obs.ID, ToObservation: request.ObservationRef, ObservationRefs: []string{request.ObservationRef, obs.ID}}
	if request.ItemID == "" && threadSnapshotMethod(request.Method) && row["error"] == nil {
		disposition := state.threadSnapshot(batch, obs, row, request.Method)
		batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
		request.ReplyRef, request.ReplyFingerprint = obs.ID, fingerprint
		state.Requests[key] = request
		return disposition
	}
	if request.ItemID == "" {
		batch.Observations = append(batch.Observations, obs)
		batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
	} else {
		item := state.Items[request.ItemID]
		outcome := replyOutcome(request.Method, result)
		if row["error"] != nil {
			outcome = model.OutcomeFailed
		} else if outcome == model.OutcomeUnknown {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_rpc_result", obs.Source))
		}
		obs.Outcome = outcome
		if request.Method == "item/tool/call" {
			state.NextItem++
			call := item
			item = model.Item{ID: sourceKey(obs.Source, "rpc-result"), SourceSeq: state.NextItem, Type: model.ItemToolResult, Category: model.CategoryToolResult, Role: model.RoleTool, Scope: call.Scope, TurnKey: call.TurnKey, TurnOrdinal: call.TurnOrdinal, CallID: call.CallID, ToolName: call.ToolName, SourceRowType: "wire", SourcePayloadType: request.Method + "/result", SourceLine: int(obs.Source.Line), SourceLineSHA256: obs.Source.SHA256, Detail: mustJSON(result)}
			item.Scope.ItemID = item.ID
			item.Parts = contentParts(result["contentItems"], obs.Source, "tool_output")
			var texts []string
			for _, part := range item.Parts {
				if part.Text != "" {
					texts = append(texts, part.Text)
				}
			}
			item.Text = strings.Join(texts, "\n")
			complete := true
			obs.Kind, obs.Text, obs.Complete = model.ObservationResult, item.Text, &complete
			relation.Scope, relation.FromItem, relation.ToItem = item.Scope, item.ID, call.ID
			request.ResultItemID = item.ID
		}
		item.Relations = append(append([]model.Relation(nil), item.Relations...), relation)
		state.observeRequestItem(batch, item, obs)
	}
	request.ReplyRef, request.ReplyFingerprint = obs.ID, fingerprint
	state.Requests[key] = request
	if request.ItemID == "" {
		return model.RecordNonContent
	}
	return model.RecordSemantic
}

func (state *State) replyReceipt(batch *model.Batch, obs model.Observation, scope model.Scope, itemID, original string) {
	obs = bindObservation(obs, scope)
	relation := model.Relation{Kind: model.RelationReplayOf, Scope: scope, FromObservation: obs.ID, ToObservation: original, ObservationRefs: []string{original, obs.ID}}
	if itemID == "" {
		batch.Observations = append(batch.Observations, obs)
		batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
		return
	}
	item := state.Items[itemID]
	relation.Scope = item.Scope
	item.Relations = append(append([]model.Relation(nil), item.Relations...), relation)
	state.observeRequestItem(batch, item, obs)
}

func selectFields(fields map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, key := range keys {
		if value, present := fields[key]; present {
			out[key] = value
		}
	}
	return out
}

func replyFields(method string, fields map[string]json.RawMessage) map[string]json.RawMessage {
	switch method {
	case "applyPatchApproval", "execCommandApproval", "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return selectFields(fields, "decision")
	case "item/permissions/requestApproval":
		return selectFields(fields, "permissions", "scope")
	case "item/tool/requestUserInput":
		return selectFields(fields, "answers")
	case "mcpServer/elicitation/request":
		return selectFields(fields, "action", "content")
	case "item/tool/call":
		return selectFields(fields, "success", "contentItems")
	case "account/chatgptAuthTokens/refresh", "attestation/generate":
		return nil
	default:
		// Client RPC results need their own decoders before nested payloads
		// become evidence. Unknown replies can carry authentication tokens.
		return selectFields(fields, "threadId", "turnId", "protocolVersion")
	}
}

func replyOutcome(method string, result map[string]json.RawMessage) model.Outcome {
	switch method {
	case "item/permissions/requestApproval":
		if rawObject(result, "permissions") != nil {
			return model.OutcomeCompleted
		}
	case "item/tool/requestUserInput":
		if rawObject(result, "answers") != nil {
			return model.OutcomeCompleted
		}
	case "item/tool/call":
		if success, valid := nativeBool(result["success"]); valid {
			if success {
				return model.OutcomeCompleted
			}
			return model.OutcomeFailed
		}
	default:
		switch rawString(result, "decision", "action") {
		case "approved", "approved_for_session", "accept", "acceptForSession":
			return model.OutcomeCompleted
		case "denied", "decline":
			return model.OutcomeDeclined
		case "abort", "cancel", "cancelled":
			return model.OutcomeCancelled
		}
	}
	return model.OutcomeUnknown
}
