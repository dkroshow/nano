package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coresession "github.com/dkroshow/nano/internal/core/session"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	liveview "github.com/dkroshow/nano/internal/liveview"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func (delivery *Delivery) recordAgentTurnDelivery(transportActionID string, result json.RawMessage, deliveryErr error, definiteFailure bool) {
	if delivery.identity.agentExecutions == nil || delivery.identity.hostedThreadID == "" {
		return
	}
	rec, ok := delivery.identity.agentExecutions.TurnForTransportAction(transportActionID)
	if !ok {
		return
	}
	status := agentTurnRunning
	if deliveryErr != nil {
		status = agentTurnDeliveryUnknown
		var rpcErr *protocol.RPCError
		if definiteFailure || errors.As(deliveryErr, &rpcErr) {
			// A refused/parked result or JSON-RPC error is a provider answer, so
			// failure is known. A transport error may follow acceptance.
			status = agentTurnFailed
		}
	}
	turnID := providerTurnIDFromStart(result)
	if deliveryErr == nil && turnID == "" {
		status = agentTurnDeliveryUnknown
		deliveryErr = errors.New("provider accepted the turn without returning a turn identity")
	}
	if err := delivery.identity.agentExecutions.MarkTurnDelivery(rec.ID, turnID, status, deliveryExecutionError(deliveryErr)); err != nil {
		log.Printf("record agent turn delivery %s: %v", rec.ID, err)
	}
	delivery.reconcileAgentOperations()
	if deliveryErr != nil {
		delivery.state.NotifyRuntime()
	}
}

func (delivery *Delivery) observeAgentExecutionWire(dir wire.Direction, line []byte) {
	if delivery.identity.agentExecutions == nil || delivery.identity.hostedThreadID == "" || dir != protocol.Recv {
		return
	}
	if delivery.identity.provider == "claude" {
		delivery.observeClaudeAgentResult(line)
		return
	}
	if delivery.identity.provider == "grok" {
		delivery.observeGrokAgentWire(line)
		return
	}
	var envelope struct {
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return
	}
	threadID := wire.String(envelope.Params["threadId"])
	turnID := wire.String(envelope.Params["turnId"])
	switch envelope.Method {
	case "item/completed":
		_, batch, err := codex.ParseWire(codex.State{}, model.Input{Bytes: line,
			Source: model.SourceRef{Label: "execution-wire", Incarnation: "frame", Direction: "recv"}})
		if err != nil {
			return
		}
		for _, change := range batch.Changes {
			item := change.Item
			if item == nil || item.Type != model.ItemAgentMessage {
				continue
			}
			if err := delivery.identity.agentExecutions.ObserveCandidate(
				"codex", threadID, turnID, item.NativeItemID, item.Phase, item.Text,
			); err != nil {
				log.Printf("record agent response candidate %s: %v", turnID, err)
			}
		}
	case "turn/completed":
		var turn map[string]json.RawMessage
		if json.Unmarshal(envelope.Params["turn"], &turn) != nil {
			return
		}
		turnID = wire.String(turn["id"])
		status := wire.String(turn["status"])
		var executionErr *threadvalues.ExecutionError
		if status != agentTurnCompleted {
			executionErr = codexExecutionError(turn["error"], status)
		}
		if !delivery.finishesOnSavedProof("codex", turnID) {
			if err := delivery.identity.agentExecutions.MarkTurnTerminal("codex", threadID, turnID, status, executionErr, "", ""); err != nil {
				log.Printf("record agent terminal turn %s: %v", turnID, err)
			}
		}
		if status != agentTurnCompleted {
			delivery.state.NotifyRuntime()
		}
	}
}

func (delivery *Delivery) observeGrokAgentWire(line []byte) {
	batch := grokWireEvidence(line)
	for _, record := range batch.Records {
		if record.Disposition == model.RecordMalformed {
			return
		}
	}
	terminalObserved := false
	for _, change := range batch.Changes {
		parsed := change.Item
		if parsed == nil || parsed.Type != model.ItemAgentMessage {
			continue
		}
		sessionID, promptID := "", ""
		if parsed.Scope.NativeThreadID != nil {
			sessionID = *parsed.Scope.NativeThreadID
		}
		if parsed.Scope.NativeTurnID != nil {
			promptID = *parsed.Scope.NativeTurnID
		}
		if grok.IsSyntheticPromptID(promptID) {
			continue
		}
		// Preserve the existing consumer candidate key. Event/turn identity
		// does not become a native item ID in the module.
		candidate := promptID
		if candidate == "" {
			for _, observation := range parsed.Observations {
				if observation.NativeEventID != "" {
					candidate = observation.NativeEventID
					break
				}
			}
		}
		text, itemID := parsed.Text, threadvalues.FirstNonEmpty(parsed.CallID, candidate+"/agentMessage")
		if err := delivery.identity.agentExecutions.ObserveCandidate("grok", sessionID, promptID, itemID, "final_answer", text); err != nil {
			log.Printf("record Grok response candidate %s: %v", promptID, err)
		}
	}
	if observation, reason, found := grokEvidenceTerminal(batch); found {
		sessionID, promptID := "", ""
		if observation.Scope.NativeThreadID != nil {
			sessionID = *observation.Scope.NativeThreadID
		}
		if observation.Scope.NativeTurnID != nil {
			promptID = *observation.Scope.NativeTurnID
		}
		if !grok.IsSyntheticPromptID(promptID) {
			terminalObserved = true
			delivery.observeGrokAgentTerminal(sessionID, promptID, grokTurnStatus(reason), reason, observation.Detail)
		}
	}
	// Grok 0.2.118 can finish a live prompt with the xAI prompt_complete
	// extension instead of a turn_completed session update. The live projector
	// already treats both as terminal; the hosted operation must do the same.
	if !terminalObserved {
		if promptID, status, _, terminal := grokTerminalFromWire(line); terminal {
			delivery.observeGrokAgentTerminal(delivery.state.ThreadID(), promptID, status, status, nil)
		}
	}
}

func (delivery *Delivery) observeGrokAgentTerminal(sessionID, promptID, status, code string, detail json.RawMessage) {
	var executionErr *threadvalues.ExecutionError
	if status == agentTurnFailed {
		executionErr = &threadvalues.ExecutionError{
			Source: "provider", Code: code, Message: "Grok turn failed", Details: detail,
		}
	}
	itemID := ""
	if status == agentTurnCompleted && delivery.view.liveProjection() != nil {
		itemID = delivery.view.liveProjection().finalAgentItemID(promptID)
	}
	if delivery.identity.agentExecutions != nil {
		if err := delivery.identity.agentExecutions.MarkTurnTerminal(
			"grok", sessionID, promptID, status, executionErr, "", itemID, hostedstore.ResultMeta{},
		); err != nil {
			log.Printf("record Grok terminal turn %s: %v", promptID, err)
		}
	}
	if status != agentTurnCompleted {
		delivery.state.NotifyRuntime()
	}
}

func (delivery *Delivery) observeClaudeAgentResult(line []byte) {
	var message map[string]json.RawMessage
	if json.Unmarshal(line, &message) != nil || wire.String(message["type"]) != "result" {
		return
	}
	// The turn of the prompt the result names (claudeAnsweredTurn), on every
	// transport; a result naming none completes no operation.
	turnID := delivery.view.liveProjection().claudeResultTurn()
	status, detail := ClaudeProjectionTerminal(line)
	var executionErr *threadvalues.ExecutionError
	if status == agentTurnFailed {
		messageText := threadvalues.FirstNonEmpty(wire.String(message["result"]), wire.String(message["subtype"]), "Claude turn failed")
		executionErr = &threadvalues.ExecutionError{Source: "provider", Code: wire.String(message["terminal_reason"]), Message: messageText, Details: detail}
	}
	response := ""
	if status == agentTurnCompleted {
		response = wire.String(message["result"])
	}
	itemID := ""
	if delivery.view.liveProjection() != nil {
		itemID = delivery.view.liveProjection().finalAgentItemID(turnID)
	}
	if delivery.finishesOnSavedProof("claude", turnID) {
		// The result names an execution, not the saved consumption of the
		// prompt: its operation finishes once its own saved row is proven.
		// Keep the answer for that moment.
		if err := delivery.identity.agentExecutions.ObserveCandidate("claude", delivery.state.ThreadID(), turnID, itemID, "final_answer", response); err != nil {
			log.Printf("record Claude agent answer for turn %s: %v", turnID, err)
		}
	} else if err := delivery.identity.agentExecutions.MarkTurnTerminal("claude", delivery.state.ThreadID(), turnID, status, executionErr, response, itemID, hostedstore.ResultMeta{}); err != nil {
		log.Printf("record Claude agent terminal turn %s: %v", turnID, err)
	}
	if status != agentTurnCompleted {
		delivery.state.NotifyRuntime()
	}
}

func codexExecutionError(raw json.RawMessage, status string) *threadvalues.ExecutionError {
	message := "Codex turn " + status
	var detail map[string]json.RawMessage
	if json.Unmarshal(raw, &detail) == nil {
		message = threadvalues.FirstNonEmpty(wire.String(detail["message"]), message)
	}
	return &threadvalues.ExecutionError{
		Source: "provider", Code: status, Message: message,
		Details: append(json.RawMessage(nil), raw...),
	}
}

// ClaudeMemberBatch bounds one owed read of the member step, and the user
// rows and members one saved turn may bring into it.
const ClaudeMemberBatch = 32

// memberStepLocked finishes the Claude prompts of this hosted thread that owe
// their member completion. Claude gives every prompt its own durable turn,
// but a prompt it takes in mid-turn is answered by that execution's one
// result; the saved transcript alone says which prompts that was. The step
// visits each owed prompt at most once, from a cursor so a prompt that cannot
// finish yet never hides later ones. Its reads run under the context's one
// deadline; the operation store's own writes and the settle lock are not
// cancellable and are not counted in it. An owed prompt whose evidence is
// missing, unknown or unreadable stays owed for the next trigger, and the
// thread keeps its owed hint while any is read. The caller holds settleMu.
func (delivery *Delivery) memberStepLocked(ctx context.Context) {
	if delivery.identity.provider != string(model.ProviderClaude) || delivery.turns == nil || delivery.runtime.completedTurnReader() == nil || delivery.identity.agentExecutions == nil {
		return
	}
	store, err := delivery.runtime.completedTurnReader()(ctx)
	if err != nil {
		log.Printf("Claude members of %s: turn store unavailable: %v", delivery.identity.hostedThreadID, err)
		return
	}
	resolve := delivery.claudeResolver(ctx)
	start, from, wrapped, seen := delivery.memberCursor, delivery.memberCursor, false, false
	for {
		batch, err := delivery.turns.Owed(ctx, delivery.identity.hostedThreadID, from, ClaudeMemberBatch)
		if err != nil {
			log.Printf("Claude members of %s: owed prompts unreadable: %v", delivery.identity.hostedThreadID, err)
			return
		}
		full := len(batch) == ClaudeMemberBatch
		if wrapped {
			// The second lap stops where the first began.
			for i, record := range batch {
				if record.Outbox.Sequence > start {
					batch, full = batch[:i], false
					break
				}
			}
		}
		if len(batch) > 0 {
			// A positive owed read is what keeps saved commits reaching this
			// step, after a restart as before it.
			seen = true
			delivery.owedHint.Store(true)
			answers, current := delivery.claudeAnswers(), delivery.state.ThreadID()
			for _, record := range batch {
				answers.FinishOwed(ctx, store, record, ClaudeSettlementEntry(record.Outbox, record.NativeThreadID, current, resolve))
			}
			from = batch[len(batch)-1].Outbox.Sequence
			delivery.memberCursor = from
		}
		if ctx.Err() != nil {
			log.Printf("Claude members of %s: step stopped at its deadline; owed prompts wait for the next trigger", delivery.identity.hostedThreadID)
			return
		}
		if full {
			continue
		}
		if wrapped || start == 0 {
			break
		}
		wrapped, from = true, 0
	}
	if !seen {
		delivery.owedHint.Store(false)
	}
}

// claudeResolver resolves provisional handles for one step, under its
// context, reading each handle's resolution at most once.
func (delivery *Delivery) claudeResolver(ctx context.Context) func(string) string {
	resolved := map[string]string{}
	return func(handle string) string {
		native, ok := resolved[handle]
		if !ok {
			native = delivery.state.resolveClaudeHandle(ctx, handle)
			resolved[handle] = native
		}
		return native
	}
}

// ClaudeAnswers is the one rule for this thread, read with the session's own
// stores and saved readers.
func (delivery *Delivery) claudeAnswers() ClaudeAnswers {
	return NewClaudeAnswers(delivery.identity.hostedThreadID, delivery.turns, delivery.identity.agentExecutions, delivery.claudeSavedRows, delivery.savedTurnFinal)
}

// claudeSavedRows reads saved user rows in one exact native thread.
func (delivery *Delivery) claudeSavedRows(ctx context.Context, native string, keys history.SavedUserKeys) ([]history.SavedUserRow, error) {
	if native == delivery.state.ThreadID() || delivery.savedPromptsIn == nil {
		if native != delivery.state.ThreadID() || delivery.savedPrompts == nil {
			return nil, history.ErrSavedTranscriptUnavailable
		}
		return delivery.savedPrompts(ctx, keys)
	}
	return delivery.savedPromptsIn(ctx, native, keys)
}

// ExactClaudeNative is the native thread a settlement entry names, when it is
// a real one rather than an unresolved provisional handle.
func ExactClaudeNative(entry prompts.OutboxEntry) (string, bool) {
	if entry.Binding == nil || entry.Binding.Scope.NativeThreadID == nil {
		return "", false
	}
	native := *entry.Binding.Scope.NativeThreadID
	return native, native != "" && !ClaudeNativeIDProvisional(native)
}

// SavedRowsGeneration is the one projection generation a read's rows came
// from; none or several is no generation.
func SavedRowsGeneration(rows []history.SavedUserRow) (string, bool) {
	generation := ""
	for _, row := range rows {
		switch {
		case row.ProjectionKey == "":
			return "", false
		case generation == "":
			generation = row.ProjectionKey
		case generation != row.ProjectionKey:
			return "", false
		}
	}
	return generation, generation != ""
}

// SavedRowsOf converts saved-user evidence into transcript rows with durable terminal status.
func SavedRowsOf(rows []history.SavedUserRow) []transcript.SavedRow {
	saved := make([]transcript.SavedRow, len(rows))
	for i, row := range rows {
		saved[i] = transcript.SavedRow{Item: row.Item, Terminal: liveview.DurableTerminalStatus(string(row.TurnStatus))}
	}
	return saved
}

// finishesOnSavedProof reports a provider turn whose open operations finish
// only on their own saved proof, through finishJoinedAgentTurns, never on the
// turn's terminal frame: a Codex prompt bound by its client id may have been
// steered into a turn another prompt started, or rejected by a hook after
// Codex acknowledged it, so the turn ending proves nothing about it. A Claude
// result names the execution that answered, never which prompts that
// execution consumed, so every Claude prompt bound by its own uuid waits for
// its own saved row too. A lookup error keeps the operation open rather than
// finishing it unproven. A resume supplies its indexed open operations so
// the same rule needs no additional execution-store scan.
func (delivery *Delivery) finishesOnSavedProof(provider, turnID string, openOperations ...[]threadvalues.TurnRecord) bool {
	// The bounded result reader checks each prompt's saved proof itself.
	// Start its deadline on provider completion even if no file row arrives.
	if delivery.identity.agentExecutions != nil {
		if delivery.identity.agentExecutions.ReadsSavedResults() {
			return false
		}
	}
	switch {
	case delivery.turns == nil:
		return false
	case provider == string(model.ProviderCodex):
		if !delivery.runtime.acceptsRunningTurnInput() {
			return false
		}
	case provider != string(model.ProviderClaude):
		return false
	}
	var open []threadvalues.TurnRecord
	if len(openOperations) == 0 {
		if delivery.identity.agentExecutions != nil {
			open = delivery.identity.agentExecutions.OpenTurnsForProviderTurn(delivery.identity.hostedThreadID, provider, turnID)
		}
	} else {
		open = openOperations[0]
	}
	for _, op := range open {
		prompt, _, err := delivery.turns.Get(context.Background(), op.TransportActionID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil || prompt.Outbox.Binding != nil && prompt.Outbox.Binding.EventID != "" {
			return true
		}
	}
	return false
}

// finishJoinedAgentTurns finishes the operations that share one provider
// turn, once two facts exist: the turn's recorded outcome, and the saved
// transcript proving the operation's own prompt joined the turn. Whichever
// fact arrives second calls this, so an operation missing either stays open.
func (delivery *Delivery) finishJoinedAgentTurns(ctx context.Context, turnID string) {
	claude := delivery.identity.provider == string(model.ProviderClaude)
	if delivery.identity.agentExecutions == nil || delivery.turns == nil || delivery.runtime.completedTurnReader() == nil || !claude && !delivery.runtime.acceptsRunningTurnInput() {
		return
	}
	store, err := delivery.runtime.completedTurnReader()(ctx)
	if err != nil {
		return
	}
	open := delivery.identity.agentExecutions.OpenTurnsForProviderTurn(delivery.identity.hostedThreadID, delivery.identity.provider, turnID)
	if len(open) == 0 {
		return
	}
	turn, err := store.Get(ctx, delivery.identity.hostedThreadID, turnID)
	if err != nil || turn.Outcome == "" {
		return
	}
	var executionErr *threadvalues.ExecutionError
	if turn.Outcome != agentTurnCompleted {
		executionErr = &threadvalues.ExecutionError{Source: "provider", Code: turn.Outcome, Message: delivery.identity.provider + " turn " + turn.Outcome}
	}
	// Every member gets the turn's one final answer, named by the recorded
	// final message id: a member of this native thread and turn that
	// finished with it or observed it. One that did not complete keeps no
	// answer.
	response, itemID, meta := "", "", hostedstore.ResultMeta{}
	if turn.Outcome == agentTurnCompleted && turn.FinalMessageID != "" {
		response, meta, _ = delivery.identity.agentExecutions.SharedTurnResult(delivery.identity.hostedThreadID, delivery.identity.provider, delivery.state.ThreadID(), turnID, turn.FinalMessageID)
		itemID = turn.FinalMessageID
	}
	// Missing wire text cannot hold the operation open indefinitely: its
	// saved-result reader decides the answer and the bounded failure.
	for _, op := range open {
		prompt, _, err := delivery.turns.Get(ctx, op.TransportActionID)
		if err != nil || prompt.Outbox.State != prompts.Confirmed {
			continue
		}
		if err := delivery.identity.agentExecutions.FinishJoinedTurn(op.ID, turnID, turn.Outcome, executionErr, response, itemID, meta); err != nil {
			log.Printf("finish joined agent turn %s: %v", op.ID, err)
		}
	}
}

// reconcileAgentOperations closes the notification-before-turn/start-response
// race and rebuilds terminal state from a resumed projection after Nano
// restarts. The saved-result reader supplies the answer.
func (delivery *Delivery) reconcileAgentOperations() {
	if delivery.identity.agentExecutions == nil || delivery.identity.hostedThreadID == "" || delivery.view.liveProjection() == nil {
		return
	}
	for _, turn := range delivery.view.liveProjection().turnList() {
		if turn.ID == "" || !CodexResumeTurnTerminal(turn.Status) {
			continue
		}
		var executionErr *threadvalues.ExecutionError
		if turn.Status != agentTurnCompleted {
			executionErr = &threadvalues.ExecutionError{Source: "provider", Code: turn.Status, Message: delivery.identity.provider + " turn " + turn.Status, Details: turn.Error}
		}
		if delivery.finishesOnSavedProof(delivery.identity.provider, turn.ID) {
			continue
		}
		_ = delivery.identity.agentExecutions.MarkTurnTerminal(
			delivery.identity.provider, delivery.state.ThreadID(), turn.ID, turn.Status, executionErr,
			"", turn.FinalItemID, hostedstore.ResultMeta{},
		)
	}
}

// observeCodexResume captures full final-answer text from thread/resume before
// the response's turn list is stripped and the bounded live projection takes
// over. It also lets a turn that completed while Nano was down settle its
// durable operation on re-attach.
func (delivery *Delivery) observeCodexResume(resp *codexResumeResponse) {
	if delivery.identity.agentExecutions == nil || delivery.identity.hostedThreadID == "" {
		return
	}
	turns, valid := resp.projectionTurns()
	if !valid {
		return
	}
	threadID := codexObjectString(resp.thread, "id")
	var terminalTurns []codexResumeProjectionTurn
	var positions []int
	var marks []hostedstore.TerminalTurn
	for position, turn := range turns {
		if turn.Status != agentTurnCompleted && !CodexResumeTurnTerminal(turn.Status) {
			continue
		}
		mark := hostedstore.TerminalTurn{ProviderTurnID: turn.ID, Status: turn.Status}
		if turn.Status != agentTurnCompleted {
			mark.Error = codexExecutionError(turn.Error, turn.Status)
		}
		terminalTurns = append(terminalTurns, turn)
		positions = append(positions, position)
		marks = append(marks, mark)
	}
	matches := delivery.identity.agentExecutions.ResumedTurnMatches(delivery.identity.hostedThreadID, "codex", threadID, marks)
	// Only a turn whose operation is still live uses its final answer, so the
	// reply's items are parsed from the earliest such turn, and not at all
	// without one: parsing every turn of every resumed thread was the largest
	// CPU cost of a boot.
	if first := slices.IndexFunc(matches, func(m hostedstore.ResumedTurnMatch) bool { return m.Live }); first >= 0 {
		parsedItems := resp.items(positions[first])
		for i, turn := range terminalTurns[first:] {
			if parsed, found := parsedItems[[2]string{turn.ID, codexResumeFinalItemID(turn)}]; found && parsed.Type == model.ItemAgentMessage && turn.Status == agentTurnCompleted {
				marks[first+i].Response, marks[first+i].ItemID = parsed.Text, parsed.NativeItemID
			}
		}
	}
	for i := range marks {
		if len(matches[i].OpenOperations) > 0 {
			marks[i].FinishOnSavedProof = delivery.finishesOnSavedProof("codex", marks[i].ProviderTurnID, matches[i].OpenOperations)
		}
	}
	endedWhileDown, _ := delivery.identity.agentExecutions.MarkResumedTurnsTerminal(delivery.identity.hostedThreadID, "codex", threadID, marks, matches)
	for i, turn := range terminalTurns {
		if endedWhileDown[i] {
			delivery.observeCodexResumedTurnFinish(threadID, turn)
		}
	}
}

// codexResumeFinalItemID is the id of a resumed turn's final answer: its
// last final_answer agent message, else its last agent message.
func codexResumeFinalItemID(turn codexResumeProjectionTurn) string {
	final, last := "", ""
	for _, raw := range turn.Items {
		item, _ := raw.(map[string]any)
		if codexObjectString(item, "type") != "agentMessage" {
			continue
		}
		last = codexObjectString(item, "id")
		if codexObjectString(item, "phase") == "final_answer" {
			final = last
		}
	}
	if final == "" {
		return last
	}
	return final
}

// observeCodexResumedTurnFinish records the completed-turn fact a live
// turn/completed would have recorded, so completion subscriptions learn of a
// turn that ended while Nano was down. Each field is derived as that live path
// derives it, with the provider's own completion time; the turn store accepts
// the same fact twice and refuses a conflicting one.
func (delivery *Delivery) observeCodexResumedTurnFinish(nativeThreadID string, turn codexResumeProjectionTurn) {
	core := delivery.runtime.coreSession()
	if core == nil {
		log.Printf("codex turn %s of %s ended while Nano was down; no core session records it", turn.ID, delivery.identity.hostedThreadID)
		return
	}
	at := time.Now()
	if completedAt := codexTimestampMS(turn.CompletedAt); completedAt > 0 {
		at = time.UnixMilli(completedAt)
	}
	finish := coreturns.Finish{HostedThreadID: delivery.identity.hostedThreadID, Provider: delivery.identity.provider, NativeThreadID: nativeThreadID,
		TurnID: turn.ID, Outcome: turn.Status, At: at, Source: "adapter"}
	if delivery.queuedTurns != nil {
		if prompt, found := delivery.queuedTurns.ByProviderTurn(delivery.identity.provider, nativeThreadID, delivery.identity.hostedThreadID, turn.ID); found {
			finish.ActionID = prompt.ClientActionID
		}
	}
	// The final message resume decoded wins; the projection may not hold it.
	if finish.FinalMessageID = codexResumeFinalItemID(turn); finish.FinalMessageID == "" && delivery.view.liveProjection() != nil {
		finish.FinalMessageID = delivery.view.liveProjection().finalAgentItemID(turn.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	event := coresession.AdapterEvent{ProviderTurnID: turn.ID, TurnEvents: []coresession.TurnEvent{{Finished: &finish}}}
	if err := core.Observe(ctx, event); err != nil {
		log.Printf("record codex turn %s of %s that ended while Nano was down: %v", turn.ID, delivery.identity.hostedThreadID, err)
	}
}
