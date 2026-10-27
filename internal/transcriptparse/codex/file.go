package codex

import (
	"encoding/json"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"strings"
)

// File dispatch follows history/transcript_codex.go. Evidence assembly owns
// identity, timing and completion instead of the old current-turn accumulator.
func (state *State) file(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) model.Disposition {
	if state.auxiliary(batch, input, source, row) {
		return model.RecordNonContent
	}
	kind := rawString(row, "type")
	payload := rawObject(row, "payload")
	nativeSession := ""
	if kind == "session_meta" {
		nativeSession = rawString(payload, "id", "thread_id", "threadId", "session_id")
		if nativeSession != "" {
			state.NativeThreads[input.Scope.ConnectionID] = nativeSession
		}
	}
	scope := state.scope(input, payload)
	if nativeSession != "" {
		scope.NativeThreadID = &nativeSession
	}
	if scope.NativeTurnID == nil {
		if id := rawString(rawObject(payload, "internal_chat_message_metadata_passthrough"), "turn_id"); id != "" {
			scope.NativeTurnID = &id
		}
	}
	obs := observation(source, scope, payload)
	obs.NativeEventID = rawString(payload, "event_id")
	obs.Times = fileTimes(input, row, payload, scope, obs.ID)
	state.catalog(batch, obs, row, payload)
	switch kind {
	case "world_state", "inter_agent_communication_metadata":
		obs.Detail = mustJSON(row)
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	case "session_meta":
		batch.Observations = append(batch.Observations, obs)
		return model.RecordNonContent
	case "turn_context":
		turn := state.turn(scope, source, true, false)
		turn.Config = codexTurnConfig(payload)
		settings := codexCollaborationSettings(payload)
		turn.Model = firstRawString([]map[string]json.RawMessage{payload, settings}, "model")
		turn.Effort = firstRawString([]map[string]json.RawMessage{payload, settings}, "effort", "reasoning_effort", "reasoningEffort")
		if turn.ServiceTier == "" {
			turn.ServiceTier = codexTurnServiceTier(payload)
		}
		state.putTurn(batch, turn)
		batch.Observations = append(batch.Observations, bindObservation(obs, turn.Scope))
		return model.RecordNonContent
	case "token_usage_record":
		turn := state.turn(scope, source, false, false)
		state.usageRecord(batch, turn, obs, payload)
		return model.RecordNonContent
	case "event_msg":
		event := rawString(payload, "type")
		if fileActionKind(event) {
			return state.fileAction(batch, obs, payload)
		}
		switch event {
		case "thread_settings_applied":
			obs.Detail = mustJSON(row)
			tier := codexExplicitSettingsServiceTier(payload)
			if tier != "" {
				group := threadKey(scope)
				active := state.Turns[state.ActiveTurns[group]]
				if active.Key != "" && !settledOutcome(active.Outcome) {
					active.ServiceTier = tier
					active.Observations = append(append([]model.Observation(nil), active.Observations...), obs)
					state.putTurn(batch, active)
				} else {
					state.PendingSettings[group] = pendingSettings{Tier: tier, Observation: obs}
				}
			}
			batch.Observations = append(batch.Observations, obs)
			return model.RecordNonContent
		case "thread_goal_updated", "thread_name_updated":
			obs.Detail = mustJSON(row)
			batch.Observations = append(batch.Observations, obs)
			return model.RecordNonContent
		case "task_started":
			turn := state.turn(scope, source, true, false)
			turn.Outcome, turn.Status = model.OutcomeRunning, model.TurnInProgress
			state.putTurn(batch, turn)
			batch.Observations = append(batch.Observations, bindObservation(obs, turn.Scope))
			return model.RecordNonContent
		case "task_complete", "turn_aborted":
			turn := state.turn(scope, source, false, false)
			for _, field := range []string{"duration_ms", "durationMs"} {
				if raw, present := payload[field]; present {
					var value *int64
					if json.Unmarshal(raw, &value) != nil || value == nil || *value < 0 {
						batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_duration", source))
					} else {
						turn.DurationMS = *value
						turn.Durations = append(append([]model.Duration(nil), turn.Durations...), model.Duration{Scope: turn.Scope, Value: *value, Unit: "millisecond", Basis: "native", SourceField: "payload." + field, ObservationRef: obs.ID})
					}
					break
				}
			}
			outcome := model.OutcomeCompleted
			if event == "turn_aborted" {
				outcome = model.OutcomeInterrupted
			}
			state.terminal(batch, turn, obs, outcome, event)
			return model.RecordNonContent
		case "token_count":
			state.tokenCount(batch, obs, payload)
			return model.RecordNonContent
		case "item_completed":
			nested := rawObject(payload, "item")
			if nested == nil {
				return state.unknown(batch, obs, "malformed_item")
			}
			turn := state.turn(scope, source, false, false)
			obs.Kind = model.ObservationSnapshot
			complete := true
			obs.Complete = &complete
			obs.Outcome = nativeOutcome(nested)
			obs.Times = append(obs.Times, nativeItemTimes(payload, turn.Scope, obs.ID)...)
			item := nativeItem(nested, source)
			if obs.NativeEventID == "" {
				obs.NativeEventID = promptClientID(item, nested)
			}
			batch.Diagnostics = append(batch.Diagnostics, itemDiagnostic(item, nested, source)...)
			item.SourceRowType, item.SourcePayloadType = kind, event
			// A completed UserMessage, AgentMessage or Reasoning is the event
			// representation of a saved response_item. Use the existing
			// turn-scoped twin matcher; same-representation repeats and
			// ambiguous candidates stay separate.
			state.emit(batch, turn, obs, item, completedTwinType(item.Type) && turn.NativeTurnID != "")
			return model.RecordSemantic
		}
	case "response_item":
	case "compacted":
		item := model.Item{Type: model.ItemCompaction, Role: model.RoleSystem, Category: model.CategorySystem, Text: nativeText(payload, "message", "text"), Detail: mustJSON(row), SourceRowType: kind}
		turn := state.turn(scope, source, false, false)
		obs.Kind = model.ObservationSnapshot
		complete := true
		obs.Complete = &complete
		state.emit(batch, turn, obs, item, false)
		return model.RecordSemantic
	default:
		obs.Detail = mustJSON(row)
		return state.unknown(batch, obs, "unknown_row")
	}
	item, known := fileItem(payload, kind, source)
	if !known {
		obs.Detail = mustJSON(row)
		return state.unknown(batch, obs, "unknown_item")
	}
	for _, part := range item.Parts {
		if part.Type == "text" || part.Type == "input_text" || part.Type == "output_text" || part.Type == "input_image" || part.Type == "image" || part.Type == "input_audio" || part.Type == "audio" || part.Type == "input_file" || part.Type == "file" {
			continue
		}
		if item.Type == model.ItemAgentMessage || item.Type == model.ItemUserMessage {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("unknown_content_part", part.Source))
		}
	}
	// Only an unbound genuine user message starts a derived turn. Native task
	// boundaries remain authoritative, and serialization twins share that turn.
	newPrompt := false
	if item.Type == model.ItemUserMessage && scope.NativeTurnID == nil {
		active := state.Turns[state.ActiveTurns[threadKey(scope)]]
		if active.Scope.NativeTurnID == nil {
			for _, previous := range state.Items {
				if previous.TurnKey == active.Key && previous.Type == model.ItemUserMessage {
					newPrompt = true
					break
				}
			}
			candidates := state.Twins[twinBucket(active, item.Role, item.Type, item.Text)]
			if len(candidates) == 1 && state.Items[candidates[0]].SourceRowType != item.SourceRowType {
				newPrompt = false
			}
		}
	}
	turn := state.turn(scope, source, false, newPrompt)
	obs.Kind = model.ObservationSnapshot
	complete := true
	obs.Complete = &complete
	obs.Outcome = nativeOutcome(payload)
	if kind == "response_item" {
		switch rawString(payload, "type") {
		case "function_call", "custom_tool_call", "local_shell_call":
			// These response records contain generated arguments. Their status
			// cannot supply the separately executed tool's outcome.
			obs.Outcome = model.OutcomeUnknown
		case "function_call_output", "local_shell_call_output":
			call := state.Items[state.Calls[scopedKey(turn.Scope, "call", turn.Key+"/"+item.CallID)]]
			if call.Type == model.ItemCommand && obs.Outcome == model.OutcomeUnknown {
				obs.Outcome = nativeOutcome(rawObject(payload, "output"))
			}
		}
	}
	if obs.NativeEventID == "" {
		// The legacy event_msg user_message copy of a prompt also carries it.
		obs.NativeEventID = promptClientID(item, payload)
	}
	// Legacy agent_reasoning events and response reasoning are serialization
	// twins too, but only inside a native turn.
	state.emit(batch, turn, obs, item, item.Category == model.CategoryMessages || item.Type == model.ItemReasoning && turn.NativeTurnID != "")
	return model.RecordSemantic
}
func (state *State) unknown(batch *model.Batch, obs model.Observation, code string) model.Disposition {
	obs.Kind = model.ObservationUnknown
	batch.Observations = append(batch.Observations, obs)
	batch.Diagnostics = append(batch.Diagnostics, diagnostic(code, obs.Source))
	return model.RecordUnknown
}

// promptClientID is the clientUserMessageId a caller gave turn/start or
// turn/steer. Codex keeps it on the UserMessage item: client_id when saved,
// clientId on the wire. It is the prompt's own identity when several prompts
// share one turn, so it becomes the observation's native event id, and only
// for a genuine user prompt.
func promptClientID(item model.Item, raw map[string]json.RawMessage) string {
	if item.Type != model.ItemUserMessage || item.Role != model.RoleUser || item.Synthetic {
		return ""
	}
	return rawString(raw, "client_id", "clientId")
}

func fileItem(payload map[string]json.RawMessage, rowType string, source model.SourceRef) (model.Item, bool) {
	kind := rawString(payload, "type")
	item := model.Item{Type: model.ItemProviderEvent, Role: model.RoleAssistant, Category: model.CategoryUnclassified, NativeItemID: rawString(payload, "id", "item_id", "itemId"), Phase: rawString(payload, "phase"), Detail: mustJSON(payload), SourceRowType: rowType, SourcePayloadType: kind}
	switch kind {
	case "message", "agent_message", "user_message":
		item.Type, item.Category = model.ItemAgentMessage, model.CategoryMessages
		if rowType == "response_item" && kind == "agent_message" {
			item.Sidechain = true
			item.FilterTags = []string{"collaboration"}
		}
		item.Text = contentText(payload["content"])
		if item.Text == "" {
			item.Text = nativeText(payload, "message", "text")
		}
		role := rawString(payload, "role")
		if role == "user" || kind == "user_message" {
			item.Type, item.Role = model.ItemUserMessage, model.RoleUser
			text, human := codexResponseUserText(payload)
			if !human && item.Text == "" {
				for _, part := range contentParts(payload["content"], source, "text") {
					if part.Type == "input_image" || part.Type == "image" || part.Type == "input_audio" || part.Type == "input_file" {
						human = true
					}
				}
			}
			if text != "" {
				item.Text = text
			}
			if !human {
				item.Type, item.Role, item.Category, item.Synthetic = model.ItemNotice, model.RoleSystem, model.CategorySystem, true
			}
		}
		if role == "developer" || role == "system" {
			item.Type, item.Role, item.Category, item.Synthetic = model.ItemNotice, model.RoleSystem, model.CategorySystem, true
		}
		item.Parts = contentParts(payload["content"], source, "text")
		if len(item.Parts) == 0 {
			item.Parts = []model.Part{{Type: "text", Text: item.Text, Availability: model.AvailableInline, Source: source}}
		}
	case "reasoning", "agent_reasoning":
		item.Type, item.Category = model.ItemReasoning, model.CategoryReasoning
		item.Parts = contentParts(payload["summary"], source, "summary")
		item.Parts = append(item.Parts, contentParts(payload["content"], source, "reasoning")...)
		item.Text = reasoningText(item.Parts)
		if item.Text == "" {
			item.Text = nativeText(payload, "text", "message")
		}
		if len(payload["encrypted_content"]) > 0 {
			item.Parts = append(item.Parts, model.Part{Type: "encrypted", Availability: model.AvailableRedacted, Source: source})
		}
	case "function_call", "custom_tool_call", "local_shell_call", "web_search_call", "tool_search_call":
		item.Type, item.Category = model.ItemToolCall, model.CategoryToolCall
		item.CallID = rawString(payload, "call_id", "callId")
		item.ToolName = rawString(payload, "name")
		if item.ToolName == "" {
			item.ToolName = kind
		}
		item.Text = codexToolSummary(item.ToolName, payload)
		if isCommandTool(item.ToolName, kind) {
			item.Type = model.ItemCommand
			if command, _ := codexCommandDisplay(payload); command != "" {
				item.Text = command
			}
			if kind == "local_shell_call" {
				if command, _ := codexCommandDisplay(rawObject(payload, "action")); command != "" {
					item.Text = command
				}
			}
		} else if strings.Contains(strings.ToLower(item.ToolName), "patch") {
			item.Type = model.ItemFileChange
		} else if item.ToolName == "request_user_input" {
			item.Type = model.ItemInteraction
		}
		item.Parts = []model.Part{{Type: "tool_input", Text: codexTextOrJSON(payload, "arguments", "input", "action"), Availability: model.AvailableInline, Source: source}}
	case "image_generation_call":
		item.Type, item.Category, item.ToolName = model.ItemToolCall, model.CategoryToolCall, kind
		item.Parts = []model.Part{{Type: "image_result", Text: nativeText(payload, "result"), Availability: model.AvailableInline, Source: source}}
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "tool_search_output", "web_search_end", "mcp_tool_call_end":
		item.Type, item.Role, item.Category = model.ItemToolResult, model.RoleTool, model.CategoryToolResult
		item.CallID = rawString(payload, "call_id", "callId")
		item.Text = codexTextOrJSON(payload, "output", "text", "result", "message", "tools", "results")
		item.Parts = []model.Part{{Type: "tool_output", Text: item.Text, Availability: model.AvailableInline, Source: source}}
	case "plan":
		item.Type, item.Category = model.ItemPlan, model.CategorySystem
		item.Text = nativeText(payload, "text", "plan")
	case "context_compacted":
		item.Type, item.Role, item.Category = model.ItemCompaction, model.RoleSystem, model.CategorySystem
		item.Text = "Conversation context compacted"
	case "compaction", "context_compaction":
		item.Type, item.Role, item.Category = model.ItemCompaction, model.RoleSystem, model.CategorySystem
		item.Text = nativeText(payload, "message", "text")
		if len(payload["encrypted_content"]) > 0 {
			item.Parts = []model.Part{{Type: "encrypted", Availability: model.AvailableRedacted, Source: source}}
		}
	case "guardian_assessment":
		item.Type, item.Role, item.Category = model.ItemInteraction, model.RoleSystem, model.CategorySystem
		item.NativeItemID = ""
		item.Text = nativeText(rawObject(payload, "action"), "command", "program", "toolName", "target")
		item.Parts = []model.Part{{Type: "review_action", Detail: payload["action"], Source: source, Availability: model.AvailableInline}}
		if id := rawString(payload, "id"); id != "" {
			item.Aliases = []model.Alias{{Namespace: "codex.reviewId", Value: mustJSON(id), Authority: model.BasisNative, ObservationRefs: []string{sourceKey(source, "row")}}}
		}
	case "sub_agent_activity", "collab_waiting_end", "collab_agent_spawn_end", "collab_close_end", "collab_agent_interaction_end", "error":
		item.Type, item.Role, item.Category = model.ItemNotice, model.RoleSystem, model.CategorySystem
		item.Text = nativeText(payload, "text", "message")
		if item.Text == "" {
			item.Text = strings.ReplaceAll(kind, "_", " ")
		}
		if kind == "error" {
			item.Text = rawString(payload, "message", "error")
		}
	default:
		return item, false
	}
	return item, true
}
func contentParts(raw json.RawMessage, source model.SourceRef, kind string) []model.Part {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			entries = []json.RawMessage{raw}
		}
	}
	var parts []model.Part
	for i, entry := range entries {
		index := i
		part := model.Part{Type: kind, Index: &index, Availability: model.AvailableInline, Source: source, Detail: mustJSON(entry)}
		var block map[string]json.RawMessage
		if json.Unmarshal(entry, &block) == nil && block != nil {
			part.Text = nativeText(block, "text", "input_text")
			if native := rawString(block, "type"); native != "" && kind == "text" {
				part.Type = native
			}
			if (part.Type == "input_image" || part.Type == "image") && rawString(block, "image_url", "url", "path") != "" {
				part.Availability = model.AvailableReference
			}
		} else {
			var text string
			if json.Unmarshal(entry, &text) == nil {
				part.Text = text
			} else {
				part.Type = "unknown"
			}
		}
		parts = append(parts, part)
	}
	return parts
}
