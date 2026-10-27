package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

type rpcRequest struct {
	Method           string      `json:"method"`
	Scope            model.Scope `json:"scope"`
	ItemID           string      `json:"itemId,omitempty"`
	ObservationRef   string      `json:"observationRef"`
	Fingerprint      string      `json:"fingerprint"`
	ReplyRef         string      `json:"replyRef,omitempty"`
	ReplyFingerprint string      `json:"replyFingerprint,omitempty"`
	ResultItemID     string      `json:"resultItemId,omitempty"`
	ResolutionRef    string      `json:"resolutionRef,omitempty"`
}

func serverRequest(method string) bool {
	switch method {
	case "account/chatgptAuthTokens/refresh", "attestation/generate", "applyPatchApproval", "execCommandApproval",
		"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval",
		"item/tool/call", "item/tool/requestUserInput", "mcpServer/elicitation/request":
		return true
	}
	return false
}

func rpcID(raw json.RawMessage) (json.RawMessage, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return mustJSON(text), true
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return mustJSON(number), true
	}
	return nil, false
}

func rpcKey(scope model.Scope, requester string, id json.RawMessage) string {
	return string(mustJSON([]any{scope.Source, scope.Incarnation, scope.ConnectionID, requester, id}))
}

func rpcFingerprint(row map[string]json.RawMessage) string {
	var native any
	decoder := json.NewDecoder(bytes.NewReader(mustJSON(row)))
	decoder.UseNumber()
	_ = decoder.Decode(&native)
	digest := sha256.Sum256(mustJSON(native))
	return fmt.Sprintf("%x", digest[:])
}

func (state *State) request(batch *model.Batch, obs model.Observation, row, params map[string]json.RawMessage) model.Disposition {
	method := rawString(row, "method")
	id, valid := rpcID(row["id"])
	if !valid {
		return state.unknown(batch, obs, "invalid_rpc_id")
	}
	if obs.Source.Direction == "send" {
		return state.unknown(batch, obs, "unexpected_rpc_direction")
	}
	if params == nil {
		return state.unknown(batch, obs, "malformed_rpc_request")
	}
	scope := obs.Scope
	if thread := rawString(params, "conversationId"); thread != "" {
		scope.NativeThreadID = &thread
	}
	metadataOnly := method == "account/chatgptAuthTokens/refresh" || method == "attestation/generate"
	safeParams := params
	if metadataOnly {
		scope.NativeThreadID, scope.NativeTurnID = nil, nil
		safeParams = map[string]json.RawMessage{}
		if method == "account/chatgptAuthTokens/refresh" {
			if reason := params["reason"]; reason != nil {
				safeParams["reason"] = reason
			}
		}
	}
	if scope.NativeThreadID == nil {
		scope.Kind = model.ScopeConnection
	}
	obs = bindObservation(obs, scope)
	obs.Detail = mustJSON(map[string]any{"method": method, "id": id, "params": safeParams})
	if !metadataOnly {
		obs.Times = append(obs.Times, nativeItemTimes(params, scope, obs.ID)...)
	}
	key, fingerprint := rpcKey(scope, "server", id), rpcFingerprint(row)
	if earlier, found := state.Requests[key]; found && earlier.ReplyRef == "" && earlier.ResolutionRef == "" {
		if earlier.Fingerprint != fingerprint {
			return state.unknown(batch, obs, "rpc_request_conflict")
		}
		relation := model.Relation{Kind: model.RelationReplayOf, Scope: earlier.Scope, FromObservation: obs.ID, ToObservation: earlier.ObservationRef, ObservationRefs: []string{earlier.ObservationRef, obs.ID}}
		if earlier.ItemID != "" {
			item := state.Items[earlier.ItemID]
			relation.Scope = item.Scope
			item.Relations = append(append([]model.Relation(nil), item.Relations...), relation)
			state.observeRequestItem(batch, item, obs)
		} else {
			batch.Observations = append(batch.Observations, obs)
			batch.Changes = append(batch.Changes, model.Change{Relation: &relation})
		}
		return model.RecordNonContent
	}
	request := rpcRequest{Method: method, Scope: scope, ObservationRef: obs.ID, Fingerprint: fingerprint}
	if metadataOnly {
		batch.Observations = append(batch.Observations, obs)
		state.Requests[key] = request
		return model.RecordNonContent
	}
	if scope.NativeTurnID != nil {
		turn := state.turn(scope, obs.Source, false, false)
		scope = turn.Scope
	}
	state.NextItem++
	item := model.Item{ID: sourceKey(obs.Source, "request"), SourceSeq: state.NextItem, Type: model.ItemInteraction, Role: model.RoleAssistant, Category: model.CategorySystem, Scope: scope, SourceRowType: "wire", SourcePayloadType: method, SourceLine: int(obs.Source.Line), SourceLineSHA256: obs.Source.SHA256, Detail: mustJSON(safeParams)}
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	item.TurnKey = scope.TurnKey
	if item.TurnKey != "" {
		item.TurnOrdinal = state.Turns[item.TurnKey].Ordinal
	}
	item.Aliases = []model.Alias{{Namespace: "codex.serverRequestId", Value: id, Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
	switch method {
	case "execCommandApproval", "item/commandExecution/requestApproval":
		item.Text, _ = codexCommandDisplay(params)
		if item.Text == "" {
			item.Text = "Command approval requested"
		}
	case "applyPatchApproval", "item/fileChange/requestApproval":
		item.Text = "File change approval requested"
	case "item/permissions/requestApproval":
		item.Text = "Permissions requested"
	case "item/tool/requestUserInput":
		item.Text = "User input requested"
		item.Parts = []model.Part{{Type: "questions", Detail: mustJSON(params["questions"]), Source: obs.Source, Availability: model.AvailableInline}}
	case "mcpServer/elicitation/request":
		item.Text = nativeText(params, "message")
	case "item/tool/call":
		item.Type, item.Category = model.ItemToolCall, model.CategoryToolCall
		item.CallID, item.ToolName = rawString(params, "callId"), rawString(params, "tool")
		item.Text = codexToolSummary(item.ToolName, params)
		item.Parts = []model.Part{{Type: "tool_input", Detail: mustJSON(params["arguments"]), Source: obs.Source, Availability: model.AvailableInline}}
	}
	complete := true
	obs.Kind, obs.Text, obs.Complete, obs.Outcome = model.ObservationSnapshot, item.Text, &complete, model.OutcomeRunning
	if item.Type == model.ItemToolCall {
		obs.Outcome = model.OutcomeUnknown
	}
	item = state.observeRequestItem(batch, item, obs)
	request.ItemID, request.Scope = item.ID, item.Scope
	state.Requests[key] = request
	return model.RecordSemantic
}

func (state *State) observeRequestItem(batch *model.Batch, item model.Item, obs model.Observation) model.Item {
	obs = bindObservation(obs, item.Scope)
	item, diagnostics := assemble.Observe(item, obs)
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	state.Items[item.ID] = item
	batch.Observations = append(batch.Observations, obs)
	batch.Changes = append(batch.Changes, model.Change{Item: &item})
	if item.TurnKey != "" {
		turn := state.Turns[item.TurnKey]
		if turn.StartSourceSeq == 0 {
			turn.StartSourceSeq = item.SourceSeq
		}
		if turn.EndSourceSeq < item.SourceSeq {
			turn.EndSourceSeq = item.SourceSeq
		}
		state.putTurn(batch, turn)
	}
	return item
}
