package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/history"
	threadvalues "github.com/dkroshow/nano/internal/threads"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// ClaudeAnswers decides Claude prompts' answers for one hosted thread by the
// one rule: a prompt's answer is the final message, and the outcome, of the
// Claude turn that consumed it, as Claude's saved transcript records it. A
// saved turn is Claude's own (one promptId), and a prompt is consumed where a
// saved user row carries its submission id: as the row's uuid, in
// mergedSendUuids, or as an absorbed queued_command's source_uuid. Prompts
// consumed by one turn share its answer, and no live result decides one.
// A session builds it from its stores and saved readers; the server builds
// it for a thread with no session.
type ClaudeAnswers struct {
	hosted string
	turns  *prompts.Store
	ops    Executions
	// rows reads saved user rows in one exact native thread; final reads one
	// saved turn's final row at one generation.
	rows  func(context.Context, string, history.SavedUserKeys) ([]history.SavedUserRow, error)
	final func(context.Context, string, string, string) (string, string, error)
}

// ClaudeAnswer is one prompt's decided answer: the saved turn that consumed it
// and that turn's outcome; a completed turn also gives its final row.
type ClaudeAnswer struct {
	// NativeID names the native thread whose saved turn consumed the prompt.
	NativeID   string
	generation string
	// TurnID names that saved consuming turn.
	TurnID string
	// Outcome is the consuming turn's terminal outcome.
	Outcome string
	// Text is the completed turn's final saved answer.
	Text string
	// FinalID is the native identity of that final answer.
	FinalID string
}

// errClaudeAnswerUndecided is a prompt whose answer the saved transcript does
// not decide yet. It is the saved-transcript-unavailable outcome, so every
// caller waits for the next catalog commit as it already does.
var errClaudeAnswerUndecided = fmt.Errorf("%w: claude answer undecided", history.ErrSavedTranscriptUnavailable)

// NewClaudeAnswers supplies one hosted thread's prompt owner and saved readers.
func NewClaudeAnswers(hosted string, turns *prompts.Store, ops Executions, rows savedPromptNativeReader, final savedTurnFinalReader) ClaudeAnswers {
	return ClaudeAnswers{hosted: hosted, turns: turns, ops: ops, rows: rows, final: final}
}

// Answer decides entry's answer. entry is the prompt as settlement sees it
// (claudeSettlementEntry): it must name an exact native thread and its own
// submission id, which no other stored prompt may carry. The rows
// come from one generation the current reader built (history refuses an older
// reader's), its one consuming saved turn must have ended, and only a
// completed turn gives a final row: an interrupted turn's last message is not
// an answer, and a truncated one decides nothing.
func (a ClaudeAnswers) Answer(ctx context.Context, entry prompts.OutboxEntry) (ClaudeAnswer, error) {
	native, ok := ExactClaudeNative(entry)
	if !ok || entry.Binding.EventID == "" || a.turns == nil || a.rows == nil || a.final == nil {
		return ClaudeAnswer{}, errClaudeAnswerUndecided
	}
	// The submission id is the prompt's own: stored with its binding, or, for
	// a prompt confirmed after an uncertain delivery, derived from its action
	// (claudeSubmissionBinding), which no other prompt can share. A stored id
	// another prompt also claims decides nothing.
	event := entry.Binding.EventID
	claims, err := a.turns.ByEvent(ctx, a.hosted, event)
	if err != nil {
		return ClaudeAnswer{}, err
	}
	for _, claim := range claims {
		if claim.Outbox.ActionID != entry.ActionID {
			return ClaudeAnswer{}, errClaudeAnswerUndecided
		}
	}
	rows, err := a.rows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{event}})
	if err != nil {
		return ClaudeAnswer{}, err
	}
	generation, ok := SavedRowsGeneration(rows)
	if !ok {
		return ClaudeAnswer{}, errClaudeAnswerUndecided
	}
	settlement, turn := transcript.SavedSettlementTurn(entry, []prompts.OutboxEntry{entry}, SavedRowsOf(rows))
	if settlement != transcript.SettlementSettled {
		return ClaudeAnswer{}, errClaudeAnswerUndecided
	}
	status := history.TurnUnknown
	for _, row := range rows {
		if row.Item.TurnKey == turn {
			status = row.TurnStatus
		}
	}
	decided := ClaudeAnswer{NativeID: native, generation: generation, TurnID: turn}
	switch status {
	case history.TurnCompleted:
		decided.Outcome = agentTurnCompleted
		decided.Text, decided.FinalID, err = a.final(ctx, native, generation, turn)
		if err != nil {
			return ClaudeAnswer{}, err
		}
		if decided.FinalID == "" {
			return ClaudeAnswer{}, errClaudeAnswerUndecided
		}
	case history.TurnInterrupted:
		decided.Outcome = agentTurnInterrupted
	default:
		return ClaudeAnswer{}, errClaudeAnswerUndecided
	}
	return decided, nil
}

// FinishOwed finishes one owed Claude prompt by its decided answer: its own
// durable turn is finished if it is not yet, its open operation completes
// with the answer, and the obligation clears once that operation durably
// holds the answer or holds a decision the rule does not reverse: a provider
// failure or the person's stop or close. An operation that only waits for
// its saved answer (result_not_saved) keeps the obligation until its late
// read commits; an agent prompt whose operation is no longer retained keeps
// it too. A prompt the saved transcript does not decide yet stays owed.
func (a ClaudeAnswers) FinishOwed(ctx context.Context, store *coreturns.Store, record prompts.Record, entry prompts.OutboxEntry) {
	answer, err := a.Answer(ctx, entry)
	if err != nil {
		if !errors.Is(err, history.ErrSavedTranscriptUnavailable) {
			log.Printf("Claude answer of %s on %s: %v", record.Outbox.ActionID, a.hosted, err)
		}
		return
	}
	var executionErr *threadvalues.ExecutionError
	if answer.Outcome != agentTurnCompleted {
		executionErr = &threadvalues.ExecutionError{Source: "provider", Code: answer.Outcome, Message: "claude turn " + answer.Outcome}
	}
	owns := false
	if record.ProviderTurnID != "" {
		// Saved membership is not execution identity: the durable turn is this
		// prompt's to finish only when it alone claims it.
		claims, err := a.turns.ProviderTurn(ctx, a.hosted, model.ProviderClaude, "", record.ProviderTurnID)
		if err != nil {
			log.Printf("Claude answer of %s on %s: turn claims unreadable: %v", record.Outbox.ActionID, a.hosted, err)
			return
		}
		owns = len(claims) == 1 && claims[0].Outbox.ActionID == record.Outbox.ActionID
	}
	if owns {
		// A turn that holds a result keeps it: its stored facts are the wire's
		// record, and public readers take the answer through this rule.
		execution, err := store.Get(ctx, a.hosted, record.ProviderTurnID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("Claude answer of %s on %s: turn unreadable: %v", record.Outbox.ActionID, a.hosted, err)
			return
		}
		if errors.Is(err, sql.ErrNoRows) || execution.Outcome == "" {
			// It had no live result of its own, so Claude reported the turn's
			// tokens on another execution's: a counter it has not recorded is
			// zero, and one it has is kept.
			zero := int64(0)
			counted := func(value *int64) *int64 {
				if value != nil {
					return value
				}
				return &zero
			}
			usage := coreturns.Usage{InputTokens: counted(execution.UsageInputTokens), CachedInputTokens: counted(execution.UsageCachedInputTokens), OutputTokens: counted(execution.UsageOutputTokens)}
			if _, _, err := store.Finished(ctx, coreturns.Finish{HostedThreadID: a.hosted, Provider: string(model.ProviderClaude), NativeThreadID: answer.NativeID,
				TurnID: record.ProviderTurnID, ActionID: record.Outbox.ActionID, FinalMessageID: answer.FinalID, Outcome: answer.Outcome, At: time.Now(), Source: "saved",
				Usage: usage}); err != nil {
				log.Printf("Claude answer of %s on %s: finish its turn: %v", record.Outbox.ActionID, a.hosted, err)
				return
			}
		}
	}
	op, ok, err := a.ops.PersistedTurnForTransportAction(ctx, a.hosted, record.Outbox.ActionID)
	if err != nil {
		log.Printf("Claude answer of %s on %s: operation unreadable: %v", record.Outbox.ActionID, a.hosted, err)
		return
	}
	if !ok {
		// Operations exist only for agent and system prompts, and retention
		// removes old terminal ones: an agent prompt without one has lost its
		// evidence. A person's prompt has none to finish.
		if record.Origin == prompts.HumanOrigin {
			a.clearOwed(ctx, record)
		}
		return
	}
	if (!threadvalues.TurnTerminal(op.Status) || threadvalues.AnswerNotSaved(op)) && record.Outbox.State == prompts.Confirmed && owns {
		// The answer is already read: it finishes the operation now, with no
		// saved-result wait whose later commit no pass would revisit.
		if err := a.ops.FinishResolvedTurn(op.ID, record.ProviderTurnID, answer.Outcome, executionErr, answer.Text, answer.FinalID); err != nil {
			log.Printf("Claude answer of %s on %s: finish operation %s: %v", record.Outbox.ActionID, a.hosted, op.ID, err)
			return
		}
		// The committed row, not the cache, says what the operation holds.
		if op, ok, err = a.ops.PersistedTurnForTransportAction(ctx, a.hosted, record.Outbox.ActionID); err != nil || !ok {
			return
		}
	}
	holdsAnswer := op.Status == threadvalues.NormalizeTerminalStatus(answer.Outcome) && (answer.Outcome != agentTurnCompleted || op.FinalItemID == answer.FinalID)
	decidedElsewhere := threadvalues.TurnTerminal(op.Status) && !threadvalues.AnswerNotSaved(op)
	if holdsAnswer || decidedElsewhere {
		a.clearOwed(ctx, record)
	}
}

func (a ClaudeAnswers) clearOwed(ctx context.Context, record prompts.Record) {
	if _, err := a.turns.ClearOwed(ctx, record.Outbox.ActionID, record.Revision); err != nil {
		log.Printf("clear owed Claude prompt %s: %v", record.Outbox.ActionID, err)
	}
}
