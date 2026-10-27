package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/assemble"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func (state *State) apply(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage, captureTimes []model.TimeObservation) model.Disposition {
	if raw, present := row["parent_tool_use_id"]; present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var parent string
		if json.Unmarshal(raw, &parent) != nil || parent == "" {
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_parent_call_scope", source))
			batch.Observations = append(batch.Observations, model.Observation{
				ID: sourceKey(source, "row"), Kind: model.ObservationUnknown, Source: source,
				Scope: diagnostic("invalid_parent_call_scope", source).Scope, Capture: model.CaptureComplete,
				Detail: mustJSON(map[string]json.RawMessage{"type": row["type"], "parent_tool_use_id": raw}),
			})
			return model.RecordUnknown
		}
	}
	scope := state.rowScope(batch, input, source, row)
	observation := model.Observation{ID: sourceKey(source, "row"), Kind: model.ObservationMetadata, Scope: scope, Source: source, Capture: model.CaptureComplete}
	observation.Times = recordTimes(input, row, scope, observation.ID)
	for _, stamp := range captureTimes {
		stamp.Scope, stamp.ObservationRef = scope, observation.ID
		observation.Times = append(observation.Times, stamp)
	}
	state.catalog(batch, observation, row)
	observation.NativeEventID = rawString(row, "uuid")
	queued, isQueued := queuedPrompt(row)
	if queued != "" {
		observation.NativeEventID = queued
	}
	// A sidechain flag excludes the parent but does not identify a child.
	// Retain the source evidence until native child or parent-call scope is
	// available; an anonymous row cannot revise the parent's active assembly.
	if rawBool(row, "isSidechain") && rawString(row, "agentId", "agent_id") == "" && scope.NativeParentCallID == nil && input.Scope.ParentNativeID == nil {
		observation.Scope.ParentNativeID = scope.NativeThreadID
		observation.Scope.NativeThreadID, observation.Scope.NativeTurnID = nil, nil
		observation.Kind, observation.Detail = model.ObservationUnknown, mustJSON(row)
		for i := range observation.Times {
			observation.Times[i].Scope = observation.Scope
		}
		d := diagnostic("unbound_sidechain_scope", source)
		d.Scope = observation.Scope
		batch.Diagnostics = append(batch.Diagnostics, d)
		batch.Observations = append(batch.Observations, observation)
		return model.RecordUnknown
	}
	rowType := rawString(row, "type")
	if replayableRow(observation.NativeEventID, rowType) {
		observation.NativeEventDigest = rowDigest(row)
	}
	start := len(batch.Observations)
	defer func() { state.rememberRowTurn(observation, batch.Observations[start:]) }()
	if replay, disposition := state.rowReplay(batch, observation, row); replay {
		return disposition
	}
	switch rowType {
	case "control_request", "control_response", "control_cancel_request":
		return state.control(batch, observation, row)
	case "user", "assistant":
		if rowType == "user" && state.questionResult(batch, observation, row) {
			return model.RecordSemantic
		}
		state.snapshot(batch, observation, row, rowType)
	case "stream_event":
		return state.stream(batch, observation, row)
	case "result":
		if journalRow(row) {
			return state.metadata(batch, observation, row)
		}
		observation.Detail = mustJSON(row)
		turn := state.ensureTurn(scope, source, false)
		state.complete(batch, turn, observation, resultOutcome(row), model.BasisNative, "result")
		state.resultUsage(batch, turn.Key, observation, row)
	case "system":
		subtype := rawString(row, "subtype")
		if subtype == "compact_boundary" || subtype == "compaction" {
			state.compaction(batch, observation, row)
			return model.RecordNonContent
		}
		observation.Detail = mustJSON(row)
		if subtype == "model_refusal_fallback" {
			state.retractNamed(batch, observation, row)
		}
		if subtype == "turn_duration" || subtype == "turn-duration" {
			if key := state.ActiveTurns[threadKey(scope)]; key != "" {
				turn := state.Turns[key]
				turn.Durations = append(append([]model.Duration(nil), turn.Durations...), model.Duration{Scope: turn.Scope, Value: rawInt64(row, "durationMs", "duration_ms"), Unit: "millisecond", Basis: "native", SourceField: "durationMs", ObservationRef: observation.ID})
				turn.DurationMS = rawInt64(row, "durationMs", "duration_ms")
				state.putTurn(batch, turn)
			}
		}
		batch.Observations = append(batch.Observations, observation)
		return model.RecordNonContent
	case "summary":
		state.compaction(batch, observation, row)
		return model.RecordNonContent
	case "attachment":
		if !isQueued {
			return state.metadata(batch, observation, row)
		}
		state.snapshot(batch, observation, row, "user")
	default:
		return state.metadata(batch, observation, row)
	}
	return model.RecordSemantic
}

func (state *State) rowScope(batch *model.Batch, input model.Input, source model.SourceRef, row map[string]json.RawMessage) model.Scope {
	scope := input.Scope
	scope.Kind, scope.Provider, scope.Source, scope.Incarnation = model.ScopeThread, model.ProviderClaude, input.Source.Label, input.Source.Incarnation
	scope.TurnKey, scope.ItemID = "", ""
	native := rawString(row, "sessionId", "session_id")
	if native != "" {
		root := scope.NativeParentCallID == nil && scope.ParentNativeID == nil && rawString(row, "parent_tool_use_id", "agentId", "agent_id") == "" && !rawBool(row, "isSidechain")
		if root {
			if state.NativeThreads[scope.ConnectionID] == "" && scope.NativeThreadID == nil {
				state.bindFirstNativeThread(batch, scope, native, source)
			}
			state.NativeThreads[scope.ConnectionID] = native
		}
	} else if scope.NativeThreadID == nil {
		native = state.NativeThreads[scope.ConnectionID]
	}
	if native != "" {
		scope.NativeThreadID = &native
	}
	if agent := rawString(row, "agentId", "agent_id"); agent != "" && rawBool(row, "isSidechain") {
		scope.ParentNativeID = scope.NativeThreadID
		scope.NativeThreadID = &agent
	}
	if raw, present := row["parent_tool_use_id"]; present {
		scope.NativeParentCallID = nil
		var parent string
		if json.Unmarshal(raw, &parent) == nil && parent != "" {
			scope.NativeParentCallID = &parent
		}
	}
	return scope
}

// queuedPrompt reports a prompt Claude saved as a queued_command attachment
// because it arrived mid-turn, and the submission id it carries: the
// attachment's own uuid is random, and source_uuid is the id the sender gave
// the prompt. Claude's own task notifications are queued the same way and
// stay metadata.
func queuedPrompt(row map[string]json.RawMessage) (string, bool) {
	if rawString(row, "type") != "attachment" {
		return "", false
	}
	attachment := rawObject(row, "attachment")
	if rawString(attachment, "type") != "queued_command" || rawString(attachment, "commandMode") != "prompt" {
		return "", false
	}
	return rawString(attachment, "source_uuid"), true
}

// mergedSends reports the submission ids of the prompts Claude merged into a
// user row. Prompts that queue together reach the model as one turn, saved as
// one row with a content block per prompt: the row's uuid is one prompt's
// id, and mergedSendUuids names the others'.
func mergedSends(row map[string]json.RawMessage) []string {
	var ids []string
	if json.Unmarshal(row["mergedSendUuids"], &ids) != nil {
		return nil
	}
	return slices.DeleteFunc(ids, func(id string) bool { return id == "" })
}

// observeMergedSends records each merged prompt's id on one block of its row,
// as that block's observation records the row's own uuid: each is an exact
// identity of the whole row.
func (state *State) observeMergedSends(batch *model.Batch, item *model.Item, block model.Observation, sends []string) {
	if len(sends) == 0 {
		return
	}
	for i, send := range sends {
		observation := model.Observation{ID: fmt.Sprintf("%s/merged:%d", block.ID, i), Kind: model.ObservationMetadata, Scope: item.Scope, Source: block.Source, NativeEventID: send, Capture: block.Capture}
		var diagnostics []model.Diagnostic
		*item, diagnostics = assemble.Observe(*item, observation)
		batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
		batch.Observations = append(batch.Observations, observation)
	}
	state.Items[item.ID] = *item
	copy := *item
	batch.Changes = append(batch.Changes, model.Change{Item: &copy})
}

// messageBody is a row's message and the native path of its content; a queued
// prompt's content is its attachment's prompt, and a slash command a sender
// sent is the command it sent (sentCommand).
func messageBody(row map[string]json.RawMessage) (map[string]json.RawMessage, string) {
	if _, queued := queuedPrompt(row); queued {
		return map[string]json.RawMessage{"role": json.RawMessage(`"user"`), "content": rawObject(row, "attachment")["prompt"]}, "$.attachment.prompt"
	}
	message := rawObject(row, "message")
	if command, sent := sentCommand(row, message); sent {
		return map[string]json.RawMessage{"role": json.RawMessage(`"user"`), "content": mustJSON(command)}, "$.message.content"
	}
	return message, "$.message.content"
}

// sentCommand reads a slash command an SDK sender sent from the row Claude
// saved it as: a row Claude marks as the sender's prompt (turnOrigin "sdk")
// whose content is Claude's expansion of the command, <command-message>,
// <command-name>/X</command-name> and <command-args>A</command-args>. It is
// the prompt "/X A", as sent, not injected context. A command typed in
// Claude's own terminal carries no such mark and stays a notice. Only content
// that opens with these tags is read, which the reader upgrade's marker
// relies on.
func sentCommand(row, message map[string]json.RawMessage) (string, bool) {
	if rawString(row, "type") != "user" || rawString(row, "turnOrigin") != "sdk" || rawBool(row, "isMeta") {
		return "", false
	}
	text := textBlocks(contentBlocks(message["content"]))
	if !strings.HasPrefix(text, "<command-") {
		return "", false
	}
	_, rest, found := strings.Cut(text, "<command-name>")
	name, _, closed := strings.Cut(rest, "</command-name>")
	if !found || !closed || !strings.HasPrefix(name, "/") {
		return "", false
	}
	_, rest, found = strings.Cut(text, "<command-args>")
	end := strings.LastIndex(rest, "</command-args>")
	if !found || end < 0 || rest[:end] == "" {
		return name, true
	}
	return name + " " + rest[:end], true
}

func threadKey(scope model.Scope) string {
	return scopedKey(scope, "thread", "")
}

func (state *State) ensureTurn(scope model.Scope, source model.SourceRef, newPrompt bool) model.Turn {
	key := threadKey(scope)
	if current := state.ActiveTurns[key]; current != "" && !newPrompt {
		return state.Turns[current]
	}
	if newPrompt {
		delete(state.ActiveMessages, key)
	}
	state.NextTurn++
	turn := model.Turn{Key: sourceKey(source, "turn"), Ordinal: state.NextTurn, Scope: scope, Status: model.TurnUnknown, Outcome: model.OutcomeUnknown, Capture: model.CaptureUnknown, CompletionEvidence: model.CompletionNone}
	turn.Scope.Kind, turn.Scope.TurnKey = model.ScopeTurn, turn.Key
	state.ActiveTurns[key] = turn.Key
	state.Turns[turn.Key] = turn
	return turn
}

func (state *State) putTurn(batch *model.Batch, turn model.Turn) {
	state.Turns[turn.Key] = turn
	batch.Changes = append(batch.Changes, model.Change{Turn: &turn})
}

func (state *State) snapshot(batch *model.Batch, base model.Observation, row map[string]json.RawMessage, role string) {
	body, contentPath := messageBody(row)
	blocks := contentBlocks(body["content"])
	rowKey := scopedKey(base.Scope, "row", rawString(row, "uuid"))
	previous := state.Rows[rowKey]
	if rawString(row, "uuid") == "" {
		previous = nil
	}
	interrupted := role == "user" && interruptionBlocks(blocks)
	// Claude's live stream can put isApiErrorMessage on a user envelope whose
	// content is still a genuine tool_result. The native file retains only the
	// block's is_error bit. Keep that logical item a tool result in both readers;
	// its failed outcome already carries the error semantics.
	errorStub := rawBool(row, "isApiErrorMessage") && !hasToolResultBlock(blocks)
	notice := nativeNotice(row, body, blocks, role) || interrupted || errorStub
	// A queued prompt joins the turn it arrived in.
	_, queued := queuedPrompt(row)
	realPrompt := role == "user" && len(previous) == 0 && !notice && !queued
	if realPrompt {
		realPrompt = false
		for _, block := range blocks {
			kind := rawString(block, "type")
			if kind == "text" && rawText(block, "text") != "" || kind == "image" || kind == "document" {
				realPrompt = true
			}
		}
	}
	var merged []string
	if role == "user" && !notice {
		merged = mergedSends(row)
	}
	// A saved turn is Claude's own: the rows that carry one promptId, however
	// Claude began it (a person's prompt, its own task notification, a slash
	// command) and whatever prompt it took in mid-turn. An interruption marker
	// or API error stub settles the turn it ends, though Claude writes the
	// next turn's promptId on it. A row without a promptId (live wire traffic,
	// local-command artifacts) keeps the prompt rule, and a replayed row never
	// moves a turn.
	newTurn := realPrompt
	promptID := rawString(row, "promptId")
	active := state.ActiveTurns[threadKey(base.Scope)]
	if role == "user" && promptID != "" && len(previous) == 0 && !interrupted && !errorStub {
		newTurn = active == "" || state.TurnPrompts[active] != "" && state.TurnPrompts[active] != promptID
	}
	turn := model.Turn{Scope: base.Scope}
	if !notice || interrupted || errorStub || active != "" || newTurn {
		turn = state.ensureTurn(base.Scope, base.Source, newTurn)
		if promptID != "" && state.TurnPrompts[turn.Key] == "" {
			if state.TurnPrompts == nil {
				state.TurnPrompts = map[string]string{}
			}
			state.TurnPrompts[turn.Key] = promptID
		}
	}
	messageID := rawString(body, "id")
	messageKey := scopedKey(base.Scope, "message", messageID)
	if messageID == "" {
		messageKey = sourceKey(base.Source, "message")
	}
	message := state.Messages[messageKey]
	unretainedMessage := message.Blocks == nil
	if message.Blocks == nil {
		message = messageState{ID: messageID, TurnKey: turn.Key, Blocks: map[int]string{}}
	}
	if message.TurnKey != "" {
		turn = state.Turns[message.TurnKey]
	}
	envelope := state.messageEnvelope(batch, base, turn, row, body)
	start := message.NextBlock
	if message.Stream {
		start = message.SnapshotNext
	}
	nativeIndex := false
	if index, ok := blockIndex(row, "apiBlockIndex"); ok {
		start = index
		nativeIndex = true
	}
	var itemIDs []string
	snapshotEnd := start + len(blocks)
	for i, block := range blocks {
		index := start + i
		// A tool-only assistant snapshot can follow a stream that also emitted
		// text. Its call ID identifies the streamed block within this message;
		// counting received snapshots would overwrite the earlier text block.
		kind, callID := rawString(block, "type"), rawString(block, "id")
		ambiguous := false
		if message.Stream && !nativeIndex && callID != "" && (kind == "tool_use" || kind == "server_tool_use") {
			matches := 0
			for candidateIndex, candidateID := range message.Blocks {
				candidate := state.Items[candidateID]
				if candidate.CallID == callID && candidate.SourcePayloadType == kind {
					index = candidateIndex
					matches++
				}
			}
			ambiguous = matches > 1
		}
		id := message.Blocks[index]
		if i < len(previous) {
			id = previous[i]
			if parts := state.Items[id].Parts; len(parts) > 0 && parts[0].Index != nil {
				index = *parts[0].Index
			}
		}
		observation := base
		observation.ID = sourceKey(base.Source, fmt.Sprintf("block:%d", i))
		path := fmt.Sprintf("%s[%d]", contentPath, i)
		if raw := bytes.TrimSpace(body["content"]); len(raw) > 0 && raw[0] != '[' {
			path = contentPath
		}
		observation.Source.BlockPath = nativePath(base.Source.BlockPath, path)
		prior := state.Items[id]
		if ambiguous || (message.Stream && id != "" && (prior.SourcePayloadType != kind || callID != "" && prior.CallID != callID)) {
			observation.Kind, observation.Detail = model.ObservationUnknown, mustJSON(block)
			batch.Observations = append(batch.Observations, observation)
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("snapshot_block_identity_conflict", observation.Source))
			continue
		}
		observation.Kind = model.ObservationSnapshot
		complete := true
		observation.Complete = &complete
		if messageID != "" && !nativeIndex && unretainedMessage && state.NextTurn > 1 {
			d := diagnostic("provisional_block_index", observation.Source)
			d.Scope = turn.Scope
			d.Message = "Block index counted without retained message; earlier blocks may have been evicted"
			batch.Diagnostics = append(batch.Diagnostics, d)
		}
		item := state.blockItem(turn, observation, block, message.ID, index, nativeIndex, id)
		item = envelopeAliases(item, row, envelope.ID)
		if len(item.Observations) == 0 {
			item.SourceRowType = role
		}
		item.Sidechain = rawBool(row, "isSidechain")
		item.Detail = envelopeDetail(row)
		if notice {
			item.Type, item.Role, item.Category, item.Synthetic = model.ItemNotice, model.RoleSystem, model.CategorySystem, true
			if rawBool(row, "isCompactSummary") {
				item.Type = model.ItemCompaction
			}
		}
		if interrupted {
			observation.Outcome = model.OutcomeInterrupted
		} else if errorStub {
			observation.Outcome = model.OutcomeFailed
			item.Category = model.CategoryError
		}
		if role == "user" && item.Type == model.ItemAgentMessage {
			item.Type, item.Role = model.ItemUserMessage, model.RoleUser
		}
		if role == "user" && item.Type == model.ItemProviderEvent {
			item.Role = model.RoleUser
		}
		state.observeBlock(batch, turn, &item, observation, block)
		state.observeMergedSends(batch, &item, observation, merged)
		message.Blocks[index] = item.ID
		itemIDs = append(itemIDs, item.ID)
		if index+1 > snapshotEnd {
			snapshotEnd = index + 1
		}
	}
	if len(previous) == 0 {
		if message.Stream {
			message.SnapshotNext = snapshotEnd
		} else if start+len(blocks) > message.NextBlock {
			message.NextBlock = start + len(blocks)
		}
	}
	if rawString(row, "uuid") != "" {
		state.Rows[rowKey] = itemIDs
	}
	state.Messages[messageKey] = message
	if interrupted || errorStub {
		outcome, reason := model.OutcomeInterrupted, "interruption_marker"
		if errorStub {
			outcome, reason = model.OutcomeFailed, "api_error_stub"
		}
		state.complete(batch, state.Turns[turn.Key], base, outcome, model.BasisNative, reason)
		return
	}
	if notice {
		return
	}
	if role == "assistant" {
		state.ActiveMessages[threadKey(base.Scope)] = messageKey
		state.recordUsage(batch, messageKey, rawObject(body, "usage"), base)
		if modelName := rawString(body, "model"); modelName != "" && modelName != "<synthetic>" {
			turn = state.Turns[turn.Key]
			turn.Model = modelName
			state.putTurn(batch, turn)
		}
		state.stopReason(batch, messageKey, rawString(body, "stop_reason", "stopReason"), base)
	}
}

func hasToolResultBlock(blocks []map[string]json.RawMessage) bool {
	for _, block := range blocks {
		if rawString(block, "type") == "tool_result" {
			return true
		}
	}
	return false
}

func (state *State) blockItem(turn model.Turn, observation model.Observation, block map[string]json.RawMessage, messageID string, index int, nativeIndex bool, id string) model.Item {
	if item, ok := state.Items[id]; ok {
		return item
	}
	kind := rawString(block, "type")
	item := model.Item{}
	if kind == "tool_use" {
		item = state.questionControlInvocation(turn.Scope, rawString(block, "name"), rawString(block, "id"))
	}
	if item.ID == "" {
		state.NextItem++
		item = model.Item{ID: sourceKey(observation.Source, fmt.Sprintf("item:%d", index)), SourceSeq: state.NextItem, Type: model.ItemAgentMessage, Role: model.RoleAssistant, Category: model.CategoryMessages}
		item.SourceLine, item.SourceLineSHA256 = int(observation.Source.Line), observation.Source.SHA256
	} else {
		item.Aliases = append([]model.Alias(nil), item.Aliases...)
	}
	item.Scope, item.TurnKey, item.TurnOrdinal = turn.Scope, turn.Key, turn.Ordinal
	item.Scope.Kind, item.Scope.ItemID = model.ScopeItem, item.ID
	item.SourcePayloadType = kind
	part := model.Part{Type: kind, Index: &index, Availability: model.AvailableInline, Source: observation.Source, Detail: mustJSON(block)}
	switch kind {
	case "text":
	case "image", "document":
		if rawString(rawObject(block, "source"), "type") == "url" {
			part.Availability = model.AvailableReference
		}
	case "thinking":
		item.Type, item.Category = model.ItemReasoning, model.CategoryReasoning
	case "redacted_thinking":
		item.Type, item.Category = model.ItemReasoning, model.CategoryReasoning
		part.Availability = model.AvailableRedacted
	case "tool_use", "server_tool_use":
		item.Type, item.Category = model.ItemToolCall, model.CategoryToolCall
		item.ToolName = rawString(block, "name")
		item.CallID = rawString(block, "id")
		if strings.EqualFold(item.ToolName, "AskUserQuestion") {
			item.Type, item.Category = model.ItemInteraction, model.CategorySystem
		}
	case "tool_result":
		item.Type, item.Category, item.Role = model.ItemToolResult, model.CategoryToolResult, model.RoleTool
		item.CallID = rawString(block, "tool_use_id", "toolUseId")
	default:
		item.Type, item.Category = model.ItemProviderEvent, model.CategoryUnclassified
	}
	item.Parts = []model.Part{part}
	basis := model.BasisDerived
	if nativeIndex {
		basis = model.BasisNative
	}
	if messageID != "" {
		item.NativeItemID = fmt.Sprintf("%s:%d", messageID, index)
		item.Aliases = append(item.Aliases, model.Alias{Namespace: "claude.messageBlock", Value: mustJSON(item.NativeItemID), Authority: basis, ObservationRefs: []string{observation.ID}})
		item.Aliases = append(item.Aliases, model.Alias{Namespace: "claude.apiMessageId", Value: mustJSON(messageID), Authority: model.BasisNative, ObservationRefs: []string{observation.ID}})
	}
	item.Aliases = append(item.Aliases, model.Alias{Namespace: "claude.blockIndex", Value: mustJSON(index), Authority: basis, ObservationRefs: []string{observation.ID}})
	if item.CallID != "" {
		value := mustJSON(item.CallID)
		found := false
		for _, alias := range item.Aliases {
			found = found || alias.Namespace == "claude.toolUseId" && bytes.Equal(alias.Value, value)
		}
		if !found {
			item.Aliases = append(item.Aliases, model.Alias{Namespace: "claude.toolUseId", Value: value, Authority: model.BasisNative, ObservationRefs: []string{observation.ID}})
		}
	}
	return item
}

func (state *State) observeBlock(batch *model.Batch, turn model.Turn, item *model.Item, observation model.Observation, block map[string]json.RawMessage) {
	observation.Scope = item.Scope
	observation.Detail = mustJSON(block)
	observation.Times = append([]model.TimeObservation(nil), observation.Times...)
	for i := range observation.Times {
		observation.Times[i].Scope = item.Scope
		observation.Times[i].ObservationRef = observation.ID
	}
	if observation.Kind == model.ObservationSnapshot {
		switch rawString(block, "type") {
		case "text":
			observation.Text = rawText(block, "text")
		case "thinking":
			observation.Text = rawText(block, "thinking")
			if _, present := block["thinking"]; !present {
				observation.Text = rawText(block, "text")
			}
		case "tool_use", "server_tool_use":
			if raw := block["input"]; len(raw) > 0 {
				// Claude starts incremental tool arguments with an empty object.
				// Keep that native evidence, but do not prepend it to JSON deltas.
				input := rawObject(block, "input")
				if observation.Complete == nil || *observation.Complete || input == nil || len(input) != 0 {
					observation.Text = string(raw)
				}
			}
		case "tool_result":
			observation.Text = toolResultText(block)
			observation.Outcome = model.OutcomeCompleted
			if toolResultStatus(block) == model.ItemStatusFailed {
				observation.Outcome = model.OutcomeFailed
			}
		}
		if kind := rawString(block, "type"); kind == "invalid" || kind == "" || item.Type == model.ItemProviderEvent {
			code := "unknown_content_block"
			if kind == "invalid" || kind == "" {
				code = "invalid_content_block"
			}
			d := diagnostic(code, observation.Source)
			d.Scope = item.Scope
			batch.Diagnostics = append(batch.Diagnostics, d)
		}
	}
	var diagnostics []model.Diagnostic
	priorRelations := len(item.Relations)
	if toolInvocation(*item) && len(item.Parts) > 0 {
		item.Text = item.Parts[0].Text
	}
	*item, diagnostics = assemble.Observe(*item, observation)
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	if len(diagnostics) == 0 && len(item.Relations) == priorRelations && toolInvocation(*item) {
		state.reopenProvisional(batch, turn, observation)
	}
	item.Parts = append([]model.Part(nil), item.Parts...)
	if len(item.Parts) > 0 {
		item.Parts[0].Text = item.Text
		if observation.Kind == model.ObservationSnapshot && len(diagnostics) == 0 && len(item.Relations) == priorRelations {
			item.Parts[0].Detail = mustJSON(block)
		}
	}
	specializeTool(item)
	callKey := scopedKey(turn.Scope, "call", turn.Key+"/"+item.CallID)
	if toolInvocation(*item) && item.CallID != "" {
		if prior, exists := state.Calls[callKey]; exists && prior != item.ID {
			// An empty entry records ambiguity, including after checkpoint restore.
			state.Calls[callKey] = ""
			batch.Diagnostics = append(batch.Diagnostics, diagnostic("ambiguous_call_id", observation.Source))
		} else {
			state.Calls[callKey] = item.ID
		}
	}
	if item.Type == model.ItemToolResult && len(item.Relations) == 0 {
		call := state.Items[state.Calls[callKey]]
		*item, diagnostics = assemble.LinkResult(call, *item)
		batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
		// A result row does not name its tool; Enhanced hides a shell
		// command's result with the command, failed or not.
		if len(diagnostics) == 0 && call.Type == model.ItemCommand && !slices.Contains(item.FilterTags, "command") {
			item.FilterTags = append(item.FilterTags, "command")
		}
	}
	state.Items[item.ID] = *item
	batch.Observations = append(batch.Observations, observation)
	copy := *item
	batch.Changes = append(batch.Changes, model.Change{Item: &copy})
	if turn.Key == "" {
		return
	}
	turn = state.Turns[turn.Key]
	if turn.StartSourceSeq == 0 {
		turn.StartSourceSeq = item.SourceSeq
	}
	if item.SourceSeq > turn.EndSourceSeq {
		turn.EndSourceSeq = item.SourceSeq
	}
	state.putTurn(batch, turn)
}

func (state *State) recordUsage(batch *model.Batch, messageKey string, fields map[string]json.RawMessage, observation model.Observation) {
	if len(fields) == 0 {
		return
	}
	message := state.Messages[messageKey]
	if message.Usage == nil {
		message.Usage = map[string]json.RawMessage{}
	}
	for key, value := range fields {
		message.Usage[key] = value
	}
	state.Messages[messageKey] = message
	values, invalid := decodeUsage(message.Usage, false)
	if invalid {
		batch.Diagnostics = append(batch.Diagnostics, diagnostic("invalid_usage_counter", observation.Source))
	}
	turn := state.Turns[message.TurnKey]
	observation.ID += "/usage-evidence"
	observation.Scope, observation.Kind = turn.Scope, model.ObservationMetadata
	observation.Detail = mustJSON(map[string]any{"apiMessageId": message.ID, "usage": message.Usage})
	observation.Times = append([]model.TimeObservation(nil), observation.Times...)
	for i := range observation.Times {
		observation.Times[i].Scope = turn.Scope
		observation.Times[i].ObservationRef = observation.ID
	}
	turn.Observations = append(append([]model.Observation(nil), turn.Observations...), observation)
	batch.Observations = append(batch.Observations, observation)
	usage := model.UsageObservation{ID: observation.ID + "/usage", Scope: turn.Scope, Grain: model.UsageResponse, SampleID: message.ID, Revision: observation.ID, Semantics: "snapshot", Values: values, ObservationRefs: []string{observation.ID}}
	turn.UsageObservations = append(append([]model.UsageObservation(nil), turn.UsageObservations...), usage)
	var diagnostics []model.Diagnostic
	turn, diagnostics = assemble.SummarizeUsage(turn)
	batch.Diagnostics = append(batch.Diagnostics, diagnostics...)
	state.putTurn(batch, turn)
}

func (state *State) stopReason(batch *model.Batch, messageKey, reason string, observation model.Observation) bool {
	message := state.Messages[messageKey]
	turn := state.Turns[message.TurnKey]
	observation.Detail = mustJSON(map[string]string{"stop_reason": reason})
	switch reason {
	case "end_turn", "stop_sequence":
		state.complete(batch, turn, observation, model.OutcomeCompleted, model.BasisDerived, "stop_reason")
	case "max_tokens":
		state.complete(batch, turn, observation, model.OutcomeTruncated, model.BasisDerived, "max_tokens")
	case "api_error":
		// A failed API message may be retried inside an unfinished conversation.
		observation.Scope, observation.Kind, observation.Outcome = turn.Scope, model.ObservationStop, model.OutcomeFailed
		observation.Times = append([]model.TimeObservation(nil), observation.Times...)
		for i := range observation.Times {
			observation.Times[i].Scope = turn.Scope
		}
		observation.Detail = mustJSON(map[string]string{"stop_reason": reason})
		turn.Observations = append(append([]model.Observation(nil), turn.Observations...), observation)
		batch.Observations = append(batch.Observations, observation)
		state.putTurn(batch, turn)
	default:
		return false
	}
	return true
}

func (state *State) complete(batch *model.Batch, turn model.Turn, observation model.Observation, outcome model.Outcome, basis model.Basis, reason string) {
	provisionalAfterNative := false
	if basis == model.BasisDerived {
		for _, fact := range turn.Completion {
			if fact.Basis == model.BasisNative {
				provisionalAfterNative = true
				break
			}
		}
	}
	observation.Scope, observation.Kind, observation.Outcome = turn.Scope, model.ObservationStop, outcome
	observation.Times = append([]model.TimeObservation(nil), observation.Times...)
	for i := range observation.Times {
		observation.Times[i].Scope = turn.Scope
		observation.Times[i].ObservationRef = observation.ID
	}
	turn.Observations = append(append([]model.Observation(nil), turn.Observations...), observation)
	turn.Completion = append(append([]model.CompletionFact(nil), turn.Completion...), model.CompletionFact{Scope: turn.Scope, Outcome: outcome, Basis: basis, ObservationRefs: []string{observation.ID}, Reason: reason})
	if provisionalAfterNative {
		batch.Observations = append(batch.Observations, observation)
		state.putTurn(batch, turn)
		return
	}
	turn.Outcome = outcome
	turn.Status = model.TurnUnknown
	switch outcome {
	case model.OutcomeCompleted:
		turn.Status = model.TurnCompleted
	case model.OutcomeInterrupted:
		turn.Status = model.TurnInterrupted
	case model.OutcomeTruncated:
		turn.Status = model.TurnTruncated
	}
	turn.CompletionEvidence = model.CompletionStopReason
	if reason == "result" {
		turn.CompletionEvidence = model.CompletionClaudeResult
	}
	if outcome != model.OutcomeCompleted {
		for _, id := range turn.FinalItemRefs {
			item := state.Items[id]
			item.Finality = model.Finality{State: model.FinalityRetracted, Basis: basis, ObservationRefs: []string{observation.ID}}
			item.IsFinalAnswer = false
			state.Items[id] = item
			batch.Changes = append(batch.Changes, model.Change{Item: &item})
		}
		turn.FinalItemRefs, turn.FinalItemID = nil, ""
	}
	if outcome == model.OutcomeCompleted {
		message := state.Messages[state.ActiveMessages[threadKey(turn.Scope)]]
		indices := make([]int, 0, len(message.Blocks))
		for index := range message.Blocks {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		turn.FinalItemRefs = nil
		turn.FinalItemID = ""
		for _, index := range indices {
			item := state.Items[message.Blocks[index]]
			if item.TurnKey != turn.Key || item.Type != model.ItemAgentMessage || item.Text == "" || item.Finality.State == model.FinalityRetracted {
				continue
			}
			finality := model.FinalityCandidate
			if basis == model.BasisNative {
				finality = model.FinalityFinal
			}
			item.Finality = model.Finality{State: finality, Basis: basis, ObservationRefs: []string{observation.ID}}
			item.IsFinalAnswer = finality == model.FinalityFinal
			state.Items[item.ID] = item
			batch.Changes = append(batch.Changes, model.Change{Item: &item})
			turn.FinalItemRefs = append(turn.FinalItemRefs, item.ID)
			turn.FinalItemID = item.ID
		}
	}
	batch.Observations = append(batch.Observations, observation)
	state.putTurn(batch, turn)
}

func resultOutcome(row map[string]json.RawMessage) model.Outcome {
	reason := strings.ToLower(rawString(row, "terminal_reason", "stop_reason", "subtype"))
	if rawBool(row, "interrupted") || strings.Contains(reason, "interrupt") || strings.Contains(reason, "aborted") || strings.Contains(reason, "cancel") {
		return model.OutcomeInterrupted
	}
	if rawBool(row, "is_error") || strings.HasPrefix(reason, "error") {
		return model.OutcomeFailed
	}
	if reason == "success" || reason == "completed" {
		return model.OutcomeCompleted
	}
	return model.OutcomeUnknown
}

func recordTimes(input model.Input, row map[string]json.RawMessage, scope model.Scope, ref string) []model.TimeObservation {
	native := model.TimeObservation{Scope: scope, Class: model.TimeNative, SourceField: "timestamp", Precision: "native_string", Meaning: model.TimeWrite, ObservationRef: ref}
	if raw, ok := row["timestamp"]; ok {
		native.Raw = raw
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
				stamp = stamp.UTC()
				native.Value = &stamp
			}
		}
	}
	times := []model.TimeObservation{native}
	for _, clock := range []struct {
		value *time.Time
		class model.TimeClass
		field string
	}{{input.CaptureAt, model.TimeCapture, "caller.captureAt"}, {input.ObservedAt, model.TimeObserved, "caller.observedAt"}} {
		if clock.value != nil {
			stamp := clock.value.UTC()
			times = append(times, model.TimeObservation{Scope: scope, Value: &stamp, Class: clock.class, SourceField: clock.field, Precision: "caller_supplied", Meaning: model.TimeEvent, ObservationRef: ref})
		}
	}
	return times
}

func blockIndex(row map[string]json.RawMessage, key string) (int, bool) {
	var index int
	raw, ok := row[key]
	if !ok || json.Unmarshal(raw, &index) != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func knownMetadataRow(kind string) bool {
	switch kind {
	case "custom-title", "ai-title", "last-prompt", "mode", "agent-name", "file-history-snapshot", "file-history-delta", "worktree-state", "attachment", "queue-operation", "progress", "permission-mode", "rate_limit_event":
		return true
	default:
		return false
	}
}
