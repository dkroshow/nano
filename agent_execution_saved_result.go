package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/transcript"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

const (
	// savedOperationResultWait covers catalog catch-up after a Nano restart,
	// when indexing a saved answer has taken longer than twenty seconds.
	savedOperationResultWait = 2 * time.Minute
	// savedResultLastRead bounds the one read after the wait has ended, as
	// when Nano restarts past an operation's deadline.
	savedResultLastRead = 2 * time.Second
)

// savedResultJob is the one reader of an operation's saved answer: the
// polling wait of an awaiting_saved operation, or the read a catalog apply of
// its native thread starts for a result_not_saved one. An apply during a read
// asks for one more read after it.
type savedResultJob struct {
	provider, nativeThreadID string
	reading, again           bool
}

// useSavedResults connects operation completion to the saved transcript.
func (s *agentExecutionStore) useSavedResults(read func(context.Context, agentTurnRecord) (string, string, error), finished func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedResultRead = read
	s.savedResultFinished = finished
	s.savedResultJobs = make(map[string]*savedResultJob)
	// Restart resumes unfinished waits with their original deadline, and reads
	// each not-saved answer once: the catalog may have indexed it meanwhile.
	for id, rec := range s.turns {
		switch {
		case rec.Status == agentTurnAwaitingSaved:
			s.startSavedResultLocked(id)
		case answerNotSaved(rec):
			s.savedResultJobs[id] = &savedResultJob{provider: rec.Provider, nativeThreadID: rec.NativeThreadID}
			s.readSavedResultLocked(id)
		}
	}
}

func (s *agentExecutionStore) awaitSavedResultLocked(id, itemID string) bool {
	rec, ok := s.turns[id]
	if !ok || agentTurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
		return false
	}
	prior := rec.Status
	// Grok's wire key names a turn/role, not a native message. The catalog's
	// exact final_item_id supplies the result pointer for that provider.
	if rec.Provider == "grok" {
		itemID = ""
	}
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Status, rec.FinalItemID, rec.Error = agentTurnAwaitingSaved, itemID, nil
	rec.FinalResponse, rec.FinalResponseBytes, rec.FinalResponseTruncated = "", 0, false
	rec.CandidateItemID, rec.CandidatePhase, rec.CandidateResponse = "", "", ""
	rec.CandidateBytes, rec.CandidateTruncated = 0, false
	touchAgentTurn(&rec)
	s.setTurnLocked(id, &rec)
	s.noteTurnTransitionLocked(prior, rec)
	return true
}

func (s *agentExecutionStore) startSavedResultLocked(id string) {
	if s.savedResultRead == nil || s.savedResultJobs[id] != nil {
		return
	}
	rec, ok := s.turns[id]
	if !ok || rec.Status != agentTurnAwaitingSaved {
		return
	}
	s.savedResultJobs[id] = &savedResultJob{provider: rec.Provider, nativeThreadID: rec.NativeThreadID, reading: true}
	go s.runSavedResult(id, rec, s.savedResultRead)
}

// savedTranscriptApplied tells the operations of one native thread that the
// catalog committed more of its file. Whatever the thread's attachment, each
// result_not_saved operation reads its exact answer again; a later saved
// answer still completes it.
func (s *agentExecutionStore) savedTranscriptApplied(provider, nativeThreadID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, job := range s.savedResultJobs {
		if rec, ok := s.turns[id]; !job.reading && (!ok || !answerNotSaved(rec)) {
			delete(s.savedResultJobs, id) // pruned, or finished another way
			continue
		}
		if job.provider != provider || job.nativeThreadID != nativeThreadID {
			continue
		}
		if job.reading {
			job.again = true
			continue
		}
		s.readSavedResultLocked(id)
	}
}

// readSavedResultLocked starts one bounded read of a result_not_saved
// operation's answer, unless the operation no longer has that outcome.
func (s *agentExecutionStore) readSavedResultLocked(id string) {
	job := s.savedResultJobs[id]
	rec, ok := s.turns[id]
	if !ok || !answerNotSaved(rec) {
		delete(s.savedResultJobs, id)
		return
	}
	job.reading, job.again = true, false
	go s.readLateSavedResult(id, rec, job, s.savedResultRead)
}

// readLateSavedResult reads a not-saved operation's answer once, and again a
// second later while a busy or changing catalog cuts the read short: only a
// read that completed without the answer waits for the next commit.
func (s *agentExecutionStore) readLateSavedResult(id string, rec agentTurnRecord, job *savedResultJob, read func(context.Context, agentTurnRecord) (string, string, error)) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), savedResultLastRead)
		text, itemID, err := read(ctx, rec)
		cancel()
		if err == nil && savedAnswerFinishes(rec, itemID) {
			s.commitSavedResult(id, rec, true, text, itemID)
			return
		}
		cut := err != nil && !savedAnswerAbsent(err)
		if cut {
			time.Sleep(time.Second)
		}
		s.mu.Lock()
		current, ok := s.turns[id]
		if s.savedResultJobs[id] != job || !ok || !answerNotSaved(current) {
			if s.savedResultJobs[id] == job {
				delete(s.savedResultJobs, id)
			}
			s.mu.Unlock()
			return
		}
		if !cut && !job.again {
			job.reading = false
			s.mu.Unlock()
			return
		}
		job.again, rec = false, current
		s.mu.Unlock()
	}
}

// savedAnswerFinishes reports a saved answer that may finish rec. A Claude
// answer is its own saved turn's (claudeAnswers), whatever final the live
// result named: results were once bound to the wrong prompt. Another
// provider's must match the live final when there is one.
func savedAnswerFinishes(rec agentTurnRecord, itemID string) bool {
	return itemID != "" && (rec.Provider == string(model.ProviderClaude) || rec.FinalItemID == "" || itemID == rec.FinalItemID)
}

// savedAnswerAbsent reports a saved-answer read that completed without the
// exact answer, as opposed to one a busy or changing catalog cut short.
func savedAnswerAbsent(err error) bool {
	return errors.Is(err, history.ErrSavedTranscriptUnavailable) || errors.Is(err, history.ErrTranscriptBodyUnavailable)
}

// answerNotSaved reports an operation whose saved answer was not found within
// its wait: the one terminal outcome a later saved answer may still replace.
func answerNotSaved(rec agentTurnRecord) bool {
	return rec.Status == agentTurnFailed && rec.Error != nil && rec.Error.Source == "saved" && rec.Error.Code == "result_not_saved"
}

// runSavedResult reads one pending operation's saved answer until it is found
// or its original deadline passes, then makes one last, briefly bounded read.
// Only pending results poll, once a second for at most their two minutes.
func (s *agentExecutionStore) runSavedResult(id string, rec agentTurnRecord, read func(context.Context, agentTurnRecord) (string, string, error)) {
	started, _ := time.Parse(time.RFC3339Nano, rec.UpdatedAt)
	deadline := started.Add(savedOperationResultWait)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		last := !time.Now().Before(deadline)
		until := deadline
		if last {
			until = time.Now().Add(savedResultLastRead)
		}
		ctx, cancel := context.WithDeadline(context.Background(), until)
		text, itemID, err := read(ctx, rec)
		cancel()
		if found := err == nil && savedAnswerFinishes(rec, itemID); found || last {
			if !found && err != nil && !savedAnswerAbsent(err) {
				// The last read was cut short: read again once the failure commits.
				s.mu.Lock()
				if job := s.savedResultJobs[id]; job != nil {
					job.again = true
				}
				s.mu.Unlock()
			}
			s.commitSavedResult(id, rec, found, text, itemID)
			return
		}
		<-tick.C
	}
}

// commitSavedResult records a decided outcome, retrying only the write while
// the store refuses it: it never reads again, so a found answer is kept.
func (s *agentExecutionStore) commitSavedResult(id string, rec agentTurnRecord, found bool, text, itemID string) {
	for failures := 0; ; failures++ {
		err := s.finishSavedResult(id, rec, found, text, itemID)
		if err == nil {
			return
		}
		if failures == 0 {
			log.Printf("saved operation result %s commit unavailable; retrying: %v", id, err)
		}
		time.Sleep(time.Second)
	}
}

// finishSavedResult records a pending operation's saved answer, or its
// result_not_saved failure. It fails only when the store refuses the commit;
// an operation that no longer waits for this result needs nothing. A
// committed failure keeps its job, so later applies read the answer again.
func (s *agentExecutionStore) finishSavedResult(id string, rec agentTurnRecord, found bool, text, itemID string) error {
	s.mu.Lock()
	current, ok := s.turns[id]
	if !ok || current.Status != agentTurnAwaitingSaved && !(found && answerNotSaved(current)) {
		delete(s.savedResultJobs, id)
		s.mu.Unlock()
		return nil
	}
	status, errResult := agentTurnCompleted, (*agentExecutionError)(nil)
	switch {
	// A changed attachment cannot supply a result for the previous native thread.
	case current.NativeThreadID != rec.NativeThreadID || current.ProviderTurnID != rec.ProviderTurnID || current.FinalItemID != rec.FinalItemID:
		text, itemID, status = "", rec.FinalItemID, agentTurnFailed
		errResult = &agentExecutionError{Source: "saved", Code: "result_not_saved", Message: "The result identity changed before its saved answer was found."}
	case !found:
		text, itemID, status = "", rec.FinalItemID, agentTurnFailed
		errResult = &agentExecutionError{Source: "saved", Code: "result_not_saved", Message: "The final answer was not saved or indexed within 2 minutes."}
	}
	if !s.applyResolvedTurnFinishLocked(id, status, errResult, text, itemID, agentResultMeta{}) {
		delete(s.savedResultJobs, id)
		s.mu.Unlock()
		return nil
	}
	if err := s.persistAndNotifyLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	if job := s.savedResultJobs[id]; job != nil && status == agentTurnFailed {
		job.provider, job.nativeThreadID, job.reading = current.Provider, current.NativeThreadID, false
		if job.again {
			s.readSavedResultLocked(id)
		}
	} else {
		delete(s.savedResultJobs, id)
	}
	finished := s.savedResultFinished
	s.mu.Unlock()
	if finished != nil {
		finished(rec.HostedThreadID)
	}
	return nil
}

func (s *server) savedOperationResultFinished(hostedID string) {
	if sess := s.sessionForHostedThread(hostedID); sess != nil {
		if sess.provider == "claude" {
			sess.requestMemberStep()
		}
		return
	}
	s.runDetachedClaudeOwed(hostedID)
}

// savedOperationResult is an operation's answer and final item id from the
// saved transcript: the final row of the completed saved turn that holds the
// operation's own prompt, all at one catalog generation.
func (s *server) savedOperationResult(ctx context.Context, rec agentTurnRecord) (string, string, error) {
	// The prompt's binding, not its delivery state, proves membership below:
	// settlement can lag the file, or expire a prompt whose answer it holds.
	prompt, _, err := s.turns.Get(ctx, rec.TransportActionID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	if err != nil || prompt.Outbox.HostedThreadID != rec.HostedThreadID || string(prompt.Provider) != rec.Provider {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	entry := prompt.Outbox
	claude := rec.Provider == string(model.ProviderClaude)
	if claude {
		entry = claudeSettlementEntry(entry, prompt.NativeThreadID, rec.NativeThreadID, nil)
		if native, ok := exactClaudeNative(entry); !ok || native != rec.NativeThreadID {
			return "", "", history.ErrSavedTranscriptUnavailable
		}
		answer, err := s.claudeAnswers(rec.HostedThreadID).answer(ctx, entry)
		if err != nil {
			return "", "", err
		}
		if answer.outcome != agentTurnCompleted {
			return "", "", history.ErrSavedTranscriptUnavailable // an interrupted turn has no answer
		}
		return answer.text, answer.finalID, nil
	}
	// The prompt's binding must name the operation's thread and, except for
	// Claude, whose execution turn is never saved, its provider turn.
	binding := entry.Binding
	if rec.NativeThreadID == "" || binding == nil || binding.Scope.NativeThreadID == nil || *binding.Scope.NativeThreadID != rec.NativeThreadID ||
		!claude && (binding.Scope.NativeTurnID == nil || *binding.Scope.NativeTurnID != rec.ProviderTurnID) {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	// A binding without its own event matches any user row of its turn, so the
	// rows other prompts in that turn claim by their events are not its own.
	// The store keeps one such binding per turn; settlement's rule weighs them.
	stored := []prompts.OutboxEntry{entry}
	if binding.EventID == "" && binding.Scope.NativeTurnID != nil {
		events, err := s.turns.TurnEvents(ctx, rec.HostedThreadID, prompt.Provider, rec.NativeThreadID, *binding.Scope.NativeTurnID)
		if err != nil {
			return "", "", err
		}
		for _, event := range events {
			stored = append(stored, prompts.OutboxEntry{ActionID: "event:" + event, Binding: &prompts.Binding{Scope: binding.Scope, EventID: event}})
		}
	}
	var keys history.SavedUserKeys
	addSavedUserKey(&keys, binding, rec.Provider)
	rows, err := s.savedPromptRows(ctx, rec.NativeThreadID, keys)
	if err != nil {
		return "", "", err
	}
	generation, ok := savedRowsGeneration(rows)
	if !ok && len(rows) > 0 {
		return "", "", history.ErrSavedGenerationChanged // rows from two generations: a rebuild is in flight
	}
	settlement, turn := transcript.SavedSettlementTurn(entry, stored, savedRowsOf(rows))
	if !ok || settlement != transcript.SettlementSettled {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	for _, row := range rows {
		if row.Item.TurnKey == turn && row.TurnStatus != history.TurnCompleted {
			return "", "", history.ErrSavedTranscriptUnavailable // an interrupted turn has no answer
		}
	}
	text, itemID, err := s.savedTurnFinal(ctx, rec.NativeThreadID, generation, turn)
	if err != nil {
		return "", "", err
	}
	if itemID == "" || rec.FinalItemID != "" && itemID != rec.FinalItemID {
		return "", "", history.ErrSavedTranscriptUnavailable
	}
	return text, itemID, nil
}
