package codex

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) wire(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) model.Disposition {
	var capture json.RawMessage
	if wrapper := rawObject(row, "msg"); wrapper != nil {
		source.Direction = rawString(row, "dir")
		capture = row["ts"]
		row = wrapper
	}
	params := rawObject(row, "params")
	scope := state.scope(input, params)
	obs := observation(source, scope, nil)
	obs.Times = wireTimes(input, capture, scope, obs.ID)
	method := rawString(row, "method")
	// RPC/control bodies can contain credentials. Preserve only the method and
	// typed request scalar until a method-specific decoder defines safe fields.
	obs.Detail = mustJSON(map[string]any{"method": method, "id": row["id"]})
	if serverRequest(method) {
		return state.request(batch, obs, row, params)
	}
	if method == "serverRequest/resolved" {
		return state.resolveRequest(batch, obs, params)
	}
	if method == "" && (row["result"] != nil || row["error"] != nil) {
		return state.reply(batch, obs, row)
	}
	if source.Direction == "send" {
		if method != "" && row["id"] != nil {
			return state.clientRequest(batch, obs, row)
		}
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	}
	switch method {
	case "process/outputDelta", "process/exited", "command/exec/outputDelta":
		return state.processUpdate(batch, obs, method, params)
	case "thread/realtime/started", "thread/realtime/closed", "thread/realtime/sdp", "thread/realtime/error", "thread/realtime/itemAdded", "thread/realtime/outputAudio/delta", "thread/realtime/transcript/delta", "thread/realtime/transcript/done":
		return state.realtime(batch, obs, method, params)
	case "turn/diff/updated", "turn/plan/updated", "item/autoApprovalReview/started", "item/autoApprovalReview/completed":
		return state.turnFacet(batch, obs, method, params)
	case "item/plan/delta", "item/fileChange/outputDelta", "item/fileChange/patchUpdated", "item/mcpToolCall/progress", "item/commandExecution/terminalInteraction":
		return state.itemUpdate(batch, obs, method, params)
	case "item/started", "item/completed":
		payload := rawObject(params, "item")
		if len(payload) == 0 {
			return state.unknown(batch, obs, "malformed_item")
		}
		turn := state.turn(scope, source, false, false)
		complete := method == "item/completed"
		obs.Kind, obs.Complete, obs.Detail = model.ObservationSnapshot, &complete, mustJSON(payload)
		obs.Outcome = nativeOutcome(payload)
		obs.Times = append(obs.Times, nativeItemTimes(params, turn.Scope, obs.ID)...)
		item := nativeItem(payload, source)
		obs.NativeEventID = promptClientID(item, payload)
		if liveWireSource(source) {
			item = boundLiveCommandItem(item)
			if item.Type == model.ItemCommand {
				obs.Detail = item.Detail
			}
		}
		batch.Diagnostics = append(batch.Diagnostics, itemDiagnostic(item, payload, source)...)
		state.emit(batch, turn, obs, item, false)
		return model.RecordSemantic
	case "turn/started", "turn/completed":
		payload := rawObject(params, "turn")
		if payload == nil {
			return state.unknown(batch, obs, "malformed_turn")
		}
		if id := rawString(payload, "id"); id != "" {
			scope.NativeTurnID = &id
		}
		turn := state.turn(scope, source, method == "turn/started", false)
		obs.Detail = mustJSON(payload)
		obs.Times = append(obs.Times, nativeTurnTimes(payload, turn.Scope, obs.ID)...)
		outcome := nativeOutcome(payload)
		if method == "turn/started" {
			turn.Outcome, turn.Status = model.OutcomeRunning, model.TurnInProgress
			state.putTurn(batch, turn)
			batch.Observations = append(batch.Observations, bindObservation(obs, turn.Scope))
		} else {
			state.terminal(batch, turn, obs, outcome, method)
		}
		return model.RecordNonContent
	case "thread/tokenUsage/updated":
		obs.Detail = mustJSON(params)
		state.tokenCount(batch, obs, map[string]json.RawMessage{"info": params["tokenUsage"]})
		return model.RecordNonContent
	case "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta", "item/reasoning/summaryPartAdded", "item/commandExecution/outputDelta":
		id := rawString(params, "itemId")
		if id == "" {
			return state.unknown(batch, obs, "missing_item_id")
		}
		index, partType := 0, "summary"
		if strings.Contains(method, "reasoning/") {
			indexField := "summaryIndex"
			if method == "item/reasoning/textDelta" {
				partType, indexField = "reasoning", "contentIndex"
			}
			if raw := params[indexField]; json.Unmarshal(raw, &index) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || index < 0 {
				return state.unknown(batch, obs, "invalid_part_index")
			}
		}
		turn := state.turn(scope, source, false, false)
		key := scopedKey(turn.Scope, "item", id)
		prior := state.Items[state.NativeItems[key]]
		item := model.Item{NativeItemID: id, Type: model.ItemAgentMessage, Role: model.RoleAssistant, Category: model.CategoryMessages, SourcePayloadType: method}
		if prior.ID != "" {
			item = prior
		}
		obs.Kind, obs.Detail = model.ObservationDelta, mustJSON(params)
		item.Text = nativeText(params, "delta", "chunk")
		if strings.Contains(method, "reasoning/") {
			item.Type, item.Category = model.ItemReasoning, model.CategoryReasoning
			item.Parts = append([]model.Part(nil), prior.Parts...)
			found := -1
			for i, part := range item.Parts {
				if part.Type == partType && part.Index != nil && *part.Index == index {
					found = i
				}
			}
			if found < 0 {
				item.Parts = append(item.Parts, model.Part{Type: partType, Index: &index, Availability: model.AvailableInline, Source: source})
				found = len(item.Parts) - 1
			}
			item.Parts[found].Text += item.Text
			sort.SliceStable(item.Parts, func(i, j int) bool {
				a, b := item.Parts[i], item.Parts[j]
				if a.Type != b.Type {
					return a.Type == "summary"
				}
				return a.Index != nil && b.Index != nil && *a.Index < *b.Index
			})
			item.Text = reasoningText(item.Parts)
			obs.Kind = model.ObservationSnapshot
		}
		if method == "item/commandExecution/outputDelta" {
			item.Type, item.Category = model.ItemCommand, model.CategoryToolCall
			item.Parts = append([]model.Part(nil), prior.Parts...)
			output := model.Part{Type: "tool_output", Availability: model.AvailableInline, Source: source}
			found := -1
			for i, part := range item.Parts {
				if part.Type == "tool_output" {
					output = part
					found = i
				}
			}
			output = appendLiveCommandOutput(output, item.Text)
			if found < 0 {
				item.Parts = append(item.Parts, output)
			} else {
				item.Parts[found] = output
			}
			item.Text = prior.Text
			obs.Kind = model.ObservationSnapshot
			obs.Detail = mustJSON(map[string]any{"method": method, "deltaBytes": len(nativeText(params, "delta", "chunk"))})
		}
		state.emit(batch, turn, obs, item, false)
		return model.RecordSemantic
	default:
		return state.notification(batch, obs, row, method, params)
	}
}
