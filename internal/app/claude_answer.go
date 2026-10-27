package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// claudeAnswers decides Claude prompts' answers for one hosted thread by the
// one rule: a prompt's answer is the final message, and the outcome, of the
// Claude turn that consumed it, as Claude's saved transcript records it. A
// saved turn is Claude's own (one promptId), and a prompt is consumed where a
// saved user row carries its submission id: as the row's uuid, in
// mergedSendUuids, or as an absorbed queued_command's source_uuid. Prompts
// consumed by one turn share its answer, and no live result decides one.
// A session builds it from its stores and saved readers; the server builds
// it for a thread with no session.
type claudeAnswers struct {
	hosted string
	turns  *prompts.Store
	ops    *agentExecutionStore
	// rows reads saved user rows in one exact native thread; final reads one
	// saved turn's final row at one generation.
	rows  func(context.Context, string, history.SavedUserKeys) ([]history.SavedUserRow, error)
	final func(context.Context, string, string, string) (string, string, error)
}

// claudeAnswer is one prompt's decided answer: the saved turn that consumed it
// and that turn's outcome; a completed turn also gives its final row.
type claudeAnswer struct {
	native, generation, turn string
	outcome                  string
	text, finalID            string
}

// errClaudeAnswerUndecided is a prompt whose answer the saved transcript does
// not decide yet. It is the saved-transcript-unavailable outcome, so every
// caller waits for the next catalog commit as it already does.
var errClaudeAnswerUndecided = fmt.Errorf("%w: claude answer undecided", history.ErrSavedTranscriptUnavailable)

// answer decides entry's answer. entry is the prompt as settlement sees it
// (claudeSettlementEntry): it must name an exact native thread and its own
// submission id, which no other stored prompt may carry. The rows
// come from one generation the current reader built (history refuses an older
// reader's), its one consuming saved turn must have ended, and only a
// completed turn gives a final row: an interrupted turn's last message is not
// an answer, and a truncated one decides nothing.
func (a claudeAnswers) answer(ctx context.Context, entry prompts.OutboxEntry) (claudeAnswer, error) {
	native, ok := exactClaudeNative(entry)
	if !ok || entry.Binding.EventID == "" || a.turns == nil || a.rows == nil || a.final == nil {
		return claudeAnswer{}, errClaudeAnswerUndecided
	}
	// The submission id is the prompt's own: stored with its binding, or, for
	// a prompt confirmed after an uncertain delivery, derived from its action
	// (claudeSubmissionBinding), which no other prompt can share. A stored id
	// another prompt also claims decides nothing.
	event := entry.Binding.EventID
	claims, err := a.turns.ByEvent(ctx, a.hosted, event)
	if err != nil {
		return claudeAnswer{}, err
	}
	for _, claim := range claims {
		if claim.Outbox.ActionID != entry.ActionID {
			return claudeAnswer{}, errClaudeAnswerUndecided
		}
	}
	rows, err := a.rows(ctx, native, history.SavedUserKeys{NativeEventIDs: []string{event}})
	if err != nil {
		return claudeAnswer{}, err
	}
	generation, ok := savedRowsGeneration(rows)
	if !ok {
		return claudeAnswer{}, errClaudeAnswerUndecided
	}
	settlement, turn := transcript.SavedSettlementTurn(entry, []prompts.OutboxEntry{entry}, savedRowsOf(rows))
	if settlement != transcript.SettlementSettled {
		return claudeAnswer{}, errClaudeAnswerUndecided
	}
	status := history.TurnUnknown
	for _, row := range rows {
		if row.Item.TurnKey == turn {
			status = row.TurnStatus
		}
	}
	decided := claudeAnswer{native: native, generation: generation, turn: turn}
	switch status {
	case history.TurnCompleted:
		decided.outcome = agentTurnCompleted
		decided.text, decided.finalID, err = a.final(ctx, native, generation, turn)
		if err != nil {
			return claudeAnswer{}, err
		}
		if decided.finalID == "" {
			return claudeAnswer{}, errClaudeAnswerUndecided
		}
	case history.TurnInterrupted:
		decided.outcome = agentTurnInterrupted
	default:
		return claudeAnswer{}, errClaudeAnswerUndecided
	}
	return decided, nil
}

// finishOwed finishes one owed Claude prompt by its decided answer: its own
// durable turn is finished if it is not yet, its open operation completes
// with the answer, and the obligation clears once that operation durably
// holds the answer or holds a decision the rule does not reverse: a provider
// failure or the person's stop or close. An operation that only waits for
// its saved answer (result_not_saved) keeps the obligation until its late
// read commits; an agent prompt whose operation is no longer retained keeps
// it too. A prompt the saved transcript does not decide yet stays owed.
func (a claudeAnswers) finishOwed(ctx context.Context, store *coreturns.Store, record prompts.Record, entry prompts.OutboxEntry) {
	answer, err := a.answer(ctx, entry)
	if err != nil {
		if !errors.Is(err, history.ErrSavedTranscriptUnavailable) {
			log.Printf("Claude answer of %s on %s: %v", record.Outbox.ActionID, a.hosted, err)
		}
		return
	}
	var executionErr *agentExecutionError
	if answer.outcome != agentTurnCompleted {
		executionErr = &agentExecutionError{Source: "provider", Code: answer.outcome, Message: "claude turn " + answer.outcome}
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
			if _, _, err := store.Finished(ctx, coreturns.Finish{HostedThreadID: a.hosted, Provider: string(model.ProviderClaude), NativeThreadID: answer.native,
				TurnID: record.ProviderTurnID, ActionID: record.Outbox.ActionID, FinalMessageID: answer.finalID, Outcome: answer.outcome, At: time.Now(), Source: "saved",
				Usage: usage}); err != nil {
				log.Printf("Claude answer of %s on %s: finish its turn: %v", record.Outbox.ActionID, a.hosted, err)
				return
			}
		}
	}
	op, ok, err := a.ops.persistedTurnForTransportAction(ctx, a.hosted, record.Outbox.ActionID)
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
	if (!agentTurnTerminal(op.Status) || answerNotSaved(op)) && record.Outbox.State == prompts.Confirmed && owns {
		// The answer is already read: it finishes the operation now, with no
		// saved-result wait whose later commit no pass would revisit.
		if err := a.ops.finishResolvedTurn(op.ID, record.ProviderTurnID, answer.outcome, executionErr, answer.text, answer.finalID); err != nil {
			log.Printf("Claude answer of %s on %s: finish operation %s: %v", record.Outbox.ActionID, a.hosted, op.ID, err)
			return
		}
		// The committed row, not the cache, says what the operation holds.
		if op, ok, err = a.ops.persistedTurnForTransportAction(ctx, a.hosted, record.Outbox.ActionID); err != nil || !ok {
			return
		}
	}
	holdsAnswer := op.Status == normalizeAgentTerminalStatus(answer.outcome) && (answer.outcome != agentTurnCompleted || op.FinalItemID == answer.finalID)
	decidedElsewhere := agentTurnTerminal(op.Status) && !answerNotSaved(op)
	if holdsAnswer || decidedElsewhere {
		a.clearOwed(ctx, record)
	}
}

// claudeAnswers is the rule for a hosted thread, read with the server's own
// stores and saved readers: an operation's answer, attached or not, and a
// thread with no session.
func (s *server) claudeAnswers(hosted string) claudeAnswers {
	return claudeAnswers{hosted: hosted, turns: s.turns, ops: s.agentExecutions, rows: s.savedPromptRows, final: s.savedTurnFinal}
}

func (a claudeAnswers) clearOwed(ctx context.Context, record prompts.Record) {
	if _, err := a.turns.ClearOwed(ctx, record.Outbox.ActionID, record.Revision); err != nil {
		log.Printf("clear owed Claude prompt %s: %v", record.Outbox.ActionID, err)
	}
}

// claudeOwedRun keeps one detached owed pass per hosted thread: a request
// during a pass is kept and runs one more pass, never dropped.
type claudeOwedRun struct {
	mu    sync.Mutex
	dirty atomic.Bool
}

// finishDetachedClaudeOwed finishes, after a catalog commit of one native
// thread, the owed prompts of each hosted thread no session holds: an exited
// thread's prompts are finished by the same rule, with no provider. A thread
// with a session runs its own member step.
func (s *server) finishDetachedClaudeOwed(native string) {
	if s.turns == nil || s.agentExecutions == nil || s.completedTurns == nil || native == "" {
		return
	}
	hosts, err := s.turns.OwedHosts(context.Background(), model.ProviderClaude, native)
	if err != nil {
		log.Printf("Claude owed prompts in %s unreadable: %v", native, err)
		return
	}
	for _, hosted := range hosts {
		if s.sessionForHostedThread(hosted) == nil {
			s.runDetachedClaudeOwed(hosted)
		}
	}
}

// runDetachedClaudeOwed runs one hosted thread's owed pass, one at a time: a
// request during a pass runs one more pass after it.
func (s *server) runDetachedClaudeOwed(hosted string) {
	if s.turns == nil || s.agentExecutions == nil || s.completedTurns == nil {
		return
	}
	value, _ := s.claudeOwedRuns.LoadOrStore(hosted, &claudeOwedRun{})
	run := value.(*claudeOwedRun)
	run.dirty.Store(true)
	for run.mu.TryLock() {
		for run.dirty.Swap(false) {
			s.claudeOwedPass(hosted)
		}
		run.mu.Unlock()
		if !run.dirty.Load() {
			break
		}
	}
}

// claudeOwedPass finishes every owed Claude prompt of one hosted thread the
// saved transcript decides, reading provisional handles' recorded
// resolutions. Undecided prompts stay owed for the next commit.
func (s *server) claudeOwedPass(hosted string) {
	ctx, cancel := context.WithTimeout(context.Background(), savedPromptSettleTimeout)
	defer cancel()
	store, err := s.completedTurns.store.coreStore(ctx)
	if err != nil {
		log.Printf("Claude owed prompts of %s: turn store unavailable: %v", hosted, err)
		return
	}
	resolve := func(handle string) string {
		native, err := s.controlMetadata.resolvedNative(ctx, hosted, string(model.ProviderClaude), handle)
		if err != nil {
			return ""
		}
		return native
	}
	answers := s.claudeAnswers(hosted)
	for from := uint64(0); ctx.Err() == nil; {
		batch, err := s.turns.Owed(ctx, hosted, from, claudeMemberBatch)
		if err != nil {
			log.Printf("Claude owed prompts of %s unreadable: %v", hosted, err)
			return
		}
		for _, record := range batch {
			answers.finishOwed(ctx, store, record, claudeSettlementEntry(record.Outbox, record.NativeThreadID, "", resolve))
		}
		if len(batch) < claudeMemberBatch {
			return
		}
		from = batch[len(batch)-1].Outbox.Sequence
	}
}
