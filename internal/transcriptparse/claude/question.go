package claude

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// questionResult joins a single native answer block to an unambiguous question
// request. The request and result remain distinct facets under the accepted
// model, even though the research draft represented both as one interaction.
func (state *State) questionResult(batch *model.Batch, base model.Observation, row map[string]json.RawMessage) bool {
	blocks := contentBlocks(rawObject(row, "message")["content"])
	if len(blocks) > 1 {
		result := rawObject(row, "toolUseResult")
		if result["answers"] != nil || result["questions"] != nil {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_question_answer_overlay", base.Source))
		}
	}
	if len(blocks) != 1 || rawString(blocks[0], "type") != "tool_result" {
		return false
	}
	block := blocks[0]
	callID := rawString(block, "tool_use_id", "toolUseId")
	var invocation model.Item
	count := 0
	if callID != "" {
		for _, item := range state.Items {
			if item.Type != model.ItemInteraction || !strings.EqualFold(item.ToolName, "AskUserQuestion") || item.CallID != callID || !sameQuestionScope(item.Scope, base.Scope) {
				continue
			}
			invocation = item
			count++
		}
	}
	result := rawObject(row, "toolUseResult")
	if count == 0 && result["answers"] == nil && result["questions"] == nil {
		return false
	}
	turn := model.Turn{}
	if count == 1 {
		turn = state.Turns[invocation.TurnKey]
	}
	envelope := state.messageEnvelope(batch, base, turn, row, rawObject(row, "message"))
	base.ID = sourceKey(base.Source, "block:0")
	base.Source.BlockPath = nativePath(base.Source.BlockPath, "$.message.content[0]")
	base.Kind, base.Outcome = model.ObservationResult, model.OutcomeCompleted
	if toolResultStatus(block) == model.ItemStatusFailed {
		base.Outcome = model.OutcomeFailed
	}
	complete := true
	base.Complete = &complete
	base.Text = answerText(row)
	if base.Text == "" {
		base.Text = toolResultText(block)
	}
	base.Detail = mustJSON(map[string]any{"block": block, "toolUseResult": result})
	scope := base.Scope
	if count == 1 {
		scope = invocation.Scope
	}
	state.NextItem++
	item := model.Item{ID: sourceKey(base.Source, "question-result"), SourceSeq: state.NextItem, Scope: scope, TurnKey: scope.TurnKey, Type: model.ItemToolResult, Category: model.CategoryToolResult, Role: model.RoleTool,
		CallID: callID, ToolName: "AskUserQuestion", Detail: mustJSON(map[string]any{"toolUseResult": result}), SourceLine: int(base.Source.Line), SourceLineSHA256: base.Source.SHA256, SourceRowType: "user", SourcePayloadType: "tool_result"}
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	item.TurnOrdinal = state.Turns[item.TurnKey].Ordinal
	index := 0
	item.Parts = []model.Part{{Type: "tool_result", Index: &index, Text: base.Text, Availability: model.AvailableInline, Source: base.Source, Detail: base.Detail}}
	if callID != "" {
		item.Aliases = []model.Alias{{Namespace: "claude.toolUseId", Value: mustJSON(callID), Authority: model.BasisNative, ObservationRefs: []string{base.ID}}}
	}
	item = envelopeAliases(item, row, envelope.ID)
	state.observeItem(batch, &item, &base)
	if count == 1 {
		refs := []string{base.ID}
		if len(invocation.Observations) > 0 {
			refs = append([]string{invocation.Observations[0].ID}, refs...)
		}
		relation := model.Relation{Kind: model.RelationResultOf, Scope: item.Scope, FromItem: item.ID, ToItem: invocation.ID, ObservationRefs: refs}
		state.controlRelation(batch, item.ID, relation)
	} else {
		code := "unresolved_question_call"
		if count > 1 {
			code = "ambiguous_question_call"
		}
		batch.Diagnostics = append(batch.Diagnostics, diagnostic(code, base.Source))
	}
	if uuid := rawString(row, "uuid"); uuid != "" {
		state.Rows[scopedKey(base.Scope, "row", uuid)] = []string{item.ID}
	}
	if item.TurnKey != "" {
		turn := state.Turns[item.TurnKey]
		turn.EndSourceSeq = item.SourceSeq
		state.putTurn(batch, turn)
	}
	return true
}

func sameQuestionScope(a, b model.Scope) bool {
	return sameConnection(a, b) && reflect.DeepEqual(a.NativeThreadID, b.NativeThreadID) && reflect.DeepEqual(a.NativeTurnID, b.NativeTurnID) && reflect.DeepEqual(a.NativeParentCallID, b.NativeParentCallID)
}

// A uniquely addressed earlier control can precede its generated call capture.
// Keep the first item position and the earlier observations' original scope.
func (state *State) questionControlInvocation(scope model.Scope, tool, callID string) model.Item {
	if !strings.EqualFold(tool, "AskUserQuestion") || callID == "" {
		return model.Item{}
	}
	var found model.Item
	for _, item := range state.Items {
		if item.Type != model.ItemInteraction || !strings.EqualFold(item.ToolName, tool) || item.CallID != callID || !sameQuestionScope(item.Scope, scope) {
			continue
		}
		if found.ID != "" || item.SourceRowType != "control_request" {
			return model.Item{}
		}
		for _, alias := range item.Aliases {
			if alias.Namespace == "claude.apiMessageId" {
				return model.Item{}
			}
		}
		found = item
	}
	return found
}

// A control may augment an already generated question call. Another control
// request with the same tool ID is not enough to prove the same invocation.
func (state *State) questionInvocation(scope model.Scope, tool, callID string) (model.Item, bool) {
	var found model.Item
	if !strings.EqualFold(tool, "AskUserQuestion") || callID == "" {
		return found, false
	}
	for _, item := range state.Items {
		if item.Type != model.ItemInteraction || !strings.EqualFold(item.ToolName, "AskUserQuestion") || item.CallID != callID || !sameQuestionScope(item.Scope, scope) {
			continue
		}
		if found.ID != "" {
			return model.Item{}, false
		}
		for _, alias := range item.Aliases {
			if alias.Namespace == "claude.controlRequestId" {
				return model.Item{}, false
			}
		}
		found = item
	}
	return found, found.ID != ""
}
