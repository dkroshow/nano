package grok

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Model history and execution telemetry are separate native sources. Their
// row order and prompt indices do not establish ACP turn or replay identity.
func (state *State) auxiliary(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) model.Disposition {
	scope := scopeFrom(input, decodedUpdate{SessionID: documentString(row, "session_id")}, model.ScopeThread)
	scope.NativeTurnID = nil
	obs := model.Observation{ID: sourceKey(source, "auxiliary"), Scope: scope, Source: source, Kind: model.ObservationMetadata, Capture: model.CaptureComplete, Detail: mustJSON(row)}
	obs.Times = auxiliaryTimes(batch, input, row, obs)
	typ := documentString(row, "type")
	switch typ {
	case "turn_started", "loop_started", "phase_changed", "first_token", "tool_started", "permission_requested", "permission_resolved", "tool_completed":
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	case "system", "user", "assistant", "reasoning", "tool_result", "backend_tool_call":
	default:
		obs.Kind = model.ObservationUnknown
		batch.Observations = append(batch.Observations, obs)
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_auxiliary_record", source))
		return model.RecordUnknown
	}
	item := model.Item{Type: model.ItemNotice, Category: model.CategorySystem, Role: model.RoleSystem, Detail: obs.Detail, SourceRowType: "model_history", SourcePayloadType: typ, Synthetic: documentString(row, "synthetic_reason") != ""}
	item.Parts = auxiliaryParts(batch, row["content"], nativeChild(source, ".content"))
	switch typ {
	case "user":
		item.Type, item.Category, item.Role = model.ItemUserMessage, model.CategoryMessages, model.RoleUser
	case "assistant":
		item.Type, item.Category, item.Role = model.ItemAgentMessage, model.CategoryMessages, model.RoleAssistant
	case "reasoning":
		item.Type, item.Category, item.Role = model.ItemReasoning, model.CategoryReasoning, model.RoleAssistant
		item.Parts = auxiliaryParts(batch, row["summary"], nativeChild(source, ".summary"))
		if encrypted, present := row["encrypted_content"]; present && string(encrypted) != "null" {
			item.Parts = append(item.Parts, model.Part{Type: "encrypted_content", Detail: mustJSON(encrypted), Availability: model.AvailableInline, Source: nativeChild(source, ".encrypted_content")})
		}
		if id := documentString(row, "id"); id != "" {
			item.Aliases = []model.Alias{{Namespace: "grok.reasoningId", Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
		}
	case "tool_result":
		item.Type, item.Category, item.Role, item.CallID = model.ItemToolResult, model.CategoryToolResult, model.RoleTool, documentString(row, "tool_call_id")
		if item.CallID != "" {
			item.Aliases = []model.Alias{{Namespace: "grok.modelToolCallId", Value: mustJSON(item.CallID), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
		}
	case "backend_tool_call":
		call := fieldsOf(row["kind"])
		item.Type, item.Category, item.Role, item.CallID = model.ItemToolCall, model.CategoryToolCall, model.RoleAssistant, documentString(call, "id")
		item.ToolName = documentString(call, "tool_type")
		item.Parts = []model.Part{{Type: "tool_action", Detail: mustJSON(call["action"]), Availability: model.AvailableInline, Source: nativeChild(source, ".kind.action")}}
		obs.Outcome = toolOutcome(documentString(call, "status"))
		if item.CallID != "" {
			item.Aliases = []model.Alias{{Namespace: "grok.backendToolCallId", Value: mustJSON(item.CallID), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
		}
	}
	item = state.emitAuxiliaryItem(batch, obs, item)
	if typ == "tool_result" {
		key := scopedKey(obs.Scope, "model-call", item.CallID)
		call := state.Items[state.Calls[key]]
		if item.CallID == "" || call.ID == "" {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unresolved_model_tool_result", source))
		} else {
			relation := model.Relation{Kind: model.RelationResultOf, Scope: item.Scope, FromItem: item.ID, ToItem: call.ID, ObservationRefs: []string{call.Observations[0].ID, item.Observations[0].ID}}
			item.Relations = []model.Relation{relation}
			state.Items[item.ID] = item
			batch.Changes = append(batch.Changes, model.Change{Item: &item}, model.Change{Relation: &relation})
		}
	}
	if typ == "assistant" {
		var calls []map[string]json.RawMessage
		if raw := row["tool_calls"]; len(raw) != 0 && json.Unmarshal(raw, &calls) != nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_model_tool_calls", source))
		}
		for index, call := range calls {
			callSource := nativeChild(source, fmt.Sprintf(".tool_calls[%d]", index))
			callObs := obs
			callObs.ID, callObs.Source, callObs.Detail = sourceKey(callSource, "auxiliary"), callSource, mustJSON(call)
			callObs.Times = append([]model.TimeObservation(nil), obs.Times...)
			tool := model.Item{Type: model.ItemToolCall, Category: model.CategoryToolCall, Role: model.RoleAssistant, ToolName: documentString(call, "name"), CallID: documentString(call, "id"), Detail: callObs.Detail, SourceRowType: "model_history", SourcePayloadType: "tool_call"}
			tool.Parts = []model.Part{{Type: "tool_arguments", Text: documentString(call, "arguments"), Detail: mustJSON(call["arguments"]), Availability: model.AvailableInline, Source: nativeChild(callSource, ".arguments")}}
			if tool.CallID != "" {
				tool.Aliases = []model.Alias{{Namespace: "grok.modelToolCallId", Value: mustJSON(tool.CallID), Authority: model.BasisNative, ObservationRefs: []string{callObs.ID}}}
			}
			tool = state.emitAuxiliaryItem(batch, callObs, tool)
			if tool.CallID != "" {
				key := scopedKey(obs.Scope, "model-call", tool.CallID)
				if _, exists := state.Calls[key]; exists {
					state.Calls[key] = ""
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_model_call_id", callSource))
				} else {
					state.Calls[key] = tool.ID
				}
			}
		}
	}
	return model.RecordSemantic
}

func (state *State) emitAuxiliaryItem(batch *model.Batch, obs model.Observation, item model.Item) model.Item {
	state.NextItem++
	item.ID, item.SourceSeq, item.Scope = sourceKey(obs.Source, "auxiliary-item"), state.NextItem, obs.Scope
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	item.SourceLine, item.SourceLineSHA256 = int(obs.Source.Line), obs.Source.SHA256
	complete := true
	obs.Kind, obs.Complete, obs.Text, obs.Scope = model.ObservationSnapshot, &complete, partsText(item.Parts), item.Scope
	for i := range obs.Times {
		obs.Times[i].Scope, obs.Times[i].ObservationRef = item.Scope, obs.ID
	}
	var diagnostics []model.Diagnostic
	item, diagnostics = assemble.Observe(item, obs)
	state.Items[item.ID] = item
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	batch.Observations = append(batch.Observations, obs)
	batch.Changes = append(batch.Changes, model.Change{Item: &item})
	return item
}

func auxiliaryParts(batch *model.Batch, raw json.RawMessage, source model.SourceRef) []model.Part {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []model.Part{{Type: "text", Text: text, Availability: model.AvailableInline, Source: source}}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_model_content", source))
		return []model.Part{{Type: "unknown", Detail: mustJSON(raw), Availability: model.AvailableInline, Source: source}}
	}
	var parts []model.Part
	for index, raw := range blocks {
		index := index
		block := fieldsOf(raw)
		typ := documentString(block, "type")
		if typ == "" {
			typ = "unknown"
		}
		parts = append(parts, model.Part{Type: typ, Text: documentString(block, "text"), Index: &index, Detail: mustJSON(raw), Availability: model.AvailableInline, Source: nativeChild(source, fmt.Sprintf("[%d]", index))})
	}
	return parts
}

func auxiliaryTimes(batch *model.Batch, input model.Input, row map[string]json.RawMessage, obs model.Observation) []model.TimeObservation {
	var times []model.TimeObservation
	if raw, present := row["ts"]; present {
		stamp := model.TimeObservation{Scope: obs.Scope, Class: model.TimeNative, Raw: mustJSON(raw), SourceField: "ts", Meaning: model.TimeEvent, Precision: "native_string", ObservationRef: obs.ID}
		if parsed, err := time.Parse(time.RFC3339Nano, documentString(row, "ts")); err == nil {
			parsed = parsed.UTC()
			stamp.Value = &parsed
		} else if string(raw) != "null" {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_timestamp", obs.Source))
		}
		times = append(times, stamp)
	}
	return append(times, callerClocks(input, obs.Scope, obs.ID)...)
}

func callerClocks(input model.Input, scope model.Scope, ref string) []model.TimeObservation {
	var times []model.TimeObservation
	for _, clock := range []struct {
		value *time.Time
		class model.TimeClass
		field string
	}{{input.CaptureAt, model.TimeCapture, "caller.captureAt"}, {input.ObservedAt, model.TimeObserved, "caller.observedAt"}} {
		if clock.value != nil {
			value := clock.value.UTC()
			times = append(times, model.TimeObservation{Scope: scope, Class: clock.class, Value: &value, SourceField: clock.field, Meaning: model.TimeEvent, Precision: "caller_supplied", ObservationRef: ref})
		}
	}
	return times
}
