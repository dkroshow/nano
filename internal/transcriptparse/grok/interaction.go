package grok

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) startInteraction(batch *model.Batch, obs *model.Observation, method string, params map[string]json.RawMessage, id json.RawMessage) {
	state.NextItem++
	item := model.Item{ID: sourceKey(obs.Source, "control-item"), SourceSeq: state.NextItem, Scope: obs.Scope, Type: model.ItemInteraction, Category: model.CategorySystem, Role: model.RoleSystem, ToolName: method, SourceRowType: obs.Source.Kind, SourcePayloadType: method, SourceLine: int(obs.Source.Line), SourceLineSHA256: obs.Source.SHA256}
	item.Aliases = []model.Alias{{Namespace: "grok.rpcRequestId", Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{obs.ID}}}
	if item.Scope.NativeTurnID != nil {
		group := scopedKey(item.Scope, "turn", "")
		turn, exists := state.Turns[group]
		if !exists {
			state.NextTurn++
			turn = model.Turn{Key: sourceKey(obs.Source, "turn"), Ordinal: state.NextTurn, NativeTurnID: *item.Scope.NativeTurnID, Scope: item.Scope, Capture: model.CaptureUnknown, Outcome: model.OutcomeUnknown, Status: model.TurnUnknown, CompletionEvidence: model.CompletionNone}
			turn.Scope.Kind, turn.Scope.TurnKey = model.ScopeTurn, turn.Key
			state.Turns[group] = turn
		}
		item.TurnKey, item.TurnOrdinal, item.Scope.TurnKey = turn.Key, turn.Ordinal, turn.Key
	}
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	switch method {
	case "session/request_permission":
		call := fieldsOf(params["toolCall"])
		item.Text, item.CallID = documentString(call, "title"), documentString(call, "toolCallId")
		item.Parts = []model.Part{{Type: "approval_request", Detail: mustJSON(pickFields(params, "toolCall", "options")), Availability: model.AvailableInline, Source: nativeChild(obs.Source, ".params")}}
	case "_x.ai/exit_plan_mode":
		item.Text, item.CallID = documentString(params, "planContent"), documentString(params, "toolCallId")
		item.Parts = []model.Part{{Type: "plan", Text: item.Text, Availability: model.AvailableInline, Source: nativeChild(obs.Source, ".params.planContent")}}
	case "_x.ai/ask_user_question":
		item.CallID = documentString(params, "toolCallId")
		var questions []map[string]json.RawMessage
		if json.Unmarshal(params["questions"], &questions) != nil || len(questions) == 0 {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("malformed_control_questions", obs.Source))
		}
		var texts []string
		for index, question := range questions {
			index := index
			text := documentString(question, "question")
			texts = append(texts, text)
			item.Parts = append(item.Parts, model.Part{Type: "question", Index: &index, Text: text, Detail: mustJSON(question), Availability: model.AvailableInline, Source: nativeChild(obs.Source, fmt.Sprintf(".params.questions[%d]", index))})
		}
		item.Text = strings.Join(texts, "\n")
	}
	state.Items[item.ID] = item
	complete := true
	obs.Scope, obs.Kind, obs.Text, obs.Complete, obs.Outcome = item.Scope, model.ObservationSnapshot, item.Text, &complete, model.OutcomeRunning
}

func (state *State) answerInteraction(batch *model.Batch, obs *model.Observation, request rpcRequest, result map[string]json.RawMessage, failed bool) {
	item, found := state.Items[request.Scope.ItemID]
	if !found {
		return
	}
	params := fieldsOf(fieldsOf(item.Detail)["params"])
	part := model.Part{Type: "control_response", Availability: model.AvailableInline, Source: nativeChild(obs.Source, ".result"), Detail: mustJSON(pickFields(result, "outcome", "optionId", "feedback", "answers"))}
	obs.Outcome = model.OutcomeUnknown
	if failed {
		part.Source = nativeChild(obs.Source, ".error")
		obs.Outcome = model.OutcomeFailed
	} else {
		switch request.Method {
		case "session/request_permission":
			outcome := fieldsOf(result["outcome"])
			if documentString(outcome, "outcome") == "cancelled" {
				obs.Outcome = model.OutcomeCancelled
				break
			}
			var options []map[string]json.RawMessage
			_ = json.Unmarshal(params["options"], &options)
			selectedID := documentString(outcome, "optionId")
			var selected map[string]json.RawMessage
			matches := 0
			if documentString(outcome, "outcome") == "selected" && selectedID != "" {
				for _, option := range options {
					if documentString(option, "optionId") == selectedID {
						selected, matches = option, matches+1
					}
				}
			}
			if matches == 1 {
				part.Text = documentString(selected, "name")
				switch documentString(selected, "kind") {
				case "allow_once", "allow_always":
					obs.Outcome = model.OutcomeCompleted
				case "reject_once", "reject_always":
					obs.Outcome = model.OutcomeDeclined
				}
			}
			if obs.Outcome == model.OutcomeUnknown {
				batch.Diagnostics = append(batch.Diagnostics, diagnostic("unverified_permission_option", obs.Source))
			}
		case "_x.ai/exit_plan_mode":
			part.Text = documentString(result, "feedback")
			switch documentString(result, "outcome") {
			case "approved":
				obs.Outcome = model.OutcomeCompleted
			case "abandoned":
				obs.Outcome = model.OutcomeCancelled
			}
		case "_x.ai/ask_user_question":
			if documentString(result, "outcome") == "accepted" {
				var answers map[string][]string
				if json.Unmarshal(result["answers"], &answers) != nil {
					batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_control_answers", obs.Source))
					break
				}
				keys := make([]string, 0, len(answers))
				for key := range answers {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				var texts []string
				for _, key := range keys {
					texts = append(texts, key+": "+strings.Join(answers[key], ", "))
				}
				part.Text = strings.Join(texts, "\n")
				seen, complete := map[string]bool{}, true
				for _, question := range item.Parts {
					if question.Type != "question" {
						continue
					}
					if seen[question.Text] {
						batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_question_key", obs.Source))
						complete = false
					}
					seen[question.Text] = true
					if len(answers[question.Text]) == 0 {
						batch.Diagnostics = append(batch.Diagnostics, diagnostic("unanswered_control_question", obs.Source))
						complete = false
					}
				}
				if complete && len(seen) > 0 {
					obs.Outcome = model.OutcomeCompleted
				}
			} else if documentString(result, "outcome") == "cancelled" {
				obs.Outcome = model.OutcomeCancelled
			}
		}
	}
	item.Parts = append(append([]model.Part(nil), item.Parts...), part)
	state.Items[item.ID] = item
}
