package store

import (
	"context"
	"database/sql"
	"maps"
	"time"

	"github.com/dkroshow/nano/internal/threads"
)

// ResumedTurnMatches scans the fleet once before prompt-store reads, which
// must happen outside the execution lock. Only the reply's turns are indexed.
func (s *ExecutionStore) ResumedTurnMatches(hostedThreadID, provider, nativeThreadID string, turns []TerminalTurn) []ResumedTurnMatch {
	out := make([]ResumedTurnMatch, len(turns))
	if s == nil || len(turns) == 0 {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	matches := make(map[string]*ResumedTurnMatch, len(turns))
	for _, turn := range turns {
		if turn.ProviderTurnID != "" {
			matches[turn.ProviderTurnID] = &ResumedTurnMatch{}
		}
	}
	for id, rec := range s.turns {
		m := matches[rec.ProviderTurnID]
		if m == nil || rec.Provider != provider {
			continue
		}
		open := hostedThreadID != "" && rec.HostedThreadID == hostedThreadID && !threads.TurnTerminal(rec.Status)
		if open {
			m.OpenOperations = append(m.OpenOperations, rec)
		}
		bound := boundToTurn(rec, provider, nativeThreadID, rec.ProviderTurnID)
		if bound {
			m.BoundOperations = append(m.BoundOperations, id)
		}
		if (open || bound) && !threads.TurnTerminal(rec.Status) {
			m.Live = true
		}
	}
	for i, turn := range turns {
		if m := matches[turn.ProviderTurnID]; m != nil {
			out[i] = *m
		}
	}
	return out
}

// MarkResumedTurnsTerminal applies reply order without another fleet scan.
// Records may finish or change binding during the unlocked prompt reads, so
// each indexed operation is checked again before it supplies or takes a mark.
func (s *ExecutionStore) MarkResumedTurnsTerminal(hostedThreadID, provider, nativeThreadID string, turns []TerminalTurn, matches []ResumedTurnMatch) ([]bool, error) {
	endedWhileDown := make([]bool, len(turns))
	if s == nil || len(turns) == 0 {
		return endedWhileDown, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for i, turn := range turns {
		m := matches[i]
		for _, op := range m.OpenOperations {
			if rec, found := s.turns[op.ID]; found && rec.HostedThreadID == hostedThreadID && rec.Provider == provider &&
				rec.ProviderTurnID == turn.ProviderTurnID && !threads.TurnTerminal(rec.Status) {
				endedWhileDown[i] = true
			}
		}
		chosen, count := "", 0
		for _, id := range m.BoundOperations {
			rec, found := s.turns[id]
			if !found || !boundToTurn(rec, provider, nativeThreadID, turn.ProviderTurnID) {
				continue
			}
			chosen, count = id, count+1
			if turn.Status == agentTurnCompleted && s.observeCandidateLocked(id, turn.ItemID, "final_answer", turn.Response) {
				changed = true
			}
		}
		if s.savedResultRead != nil {
			for _, id := range m.BoundOperations {
				if rec, ok := s.turns[id]; ok && boundToTurn(rec, provider, nativeThreadID, turn.ProviderTurnID) && s.markTurnTerminalLocked(id, turn.Status, turn.Error, turn.Response, turn.ItemID) {
					changed = true
				}
			}
			continue
		}
		if !turn.FinishOnSavedProof && count == 1 && s.markTurnTerminalLocked(chosen, turn.Status, turn.Error, turn.Response, turn.ItemID) {
			changed = true
		}
	}
	if !changed {
		return endedWhileDown, nil
	}
	return endedWhileDown, s.persistAndNotifyLocked()
}

// Only a thread's latest submitted operation decides attention; a successful
// follow-up clears an earlier failure without a separate acknowledgement.
func (s *ExecutionStore) refreshAttentionLocked(hostedID string) {
	if !s.attentionDirty[hostedID] {
		return
	}
	latest, latestAt := threads.TurnRecord{}, time.Time{}
	for id := range s.turnsByHosted[hostedID] {
		rec := s.turns[id]
		// Compared as times: RFC3339Nano trims trailing zeros, so as text
		// ".2374Z" sorts after the later ".237401Z".
		at := threads.ParseTimestamp(rec.CreatedAt)
		if latest.ID == "" || at.After(latestAt) || (at.Equal(latestAt) && rec.ID > latest.ID) {
			latest, latestAt = rec, at
		}
	}
	delete(s.attentionDirty, hostedID)
	if latest.ID == "" {
		delete(s.attentionCache, hostedID)
	} else {
		s.attentionCache[hostedID] = latest.Status == agentTurnFailed || latest.Status == agentTurnDeliveryUnknown
	}
}

// SessionsNeedingAttention reports hosted threads with unresolved operation failures.
func (s *ExecutionStore) SessionsNeedingAttention() map[string]bool {
	if s == nil {
		return map[string]bool{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hostedID := range s.attentionDirty {
		s.refreshAttentionLocked(hostedID)
	}
	return maps.Clone(s.attentionCache)
}

// SessionNeedsAttention reports unresolved failures for a hosted thread.
func (s *ExecutionStore) SessionNeedsAttention(hostedID string) bool {
	if s == nil || hostedID == "" {
		return false
	}
	s.mu.RLock()
	if !s.attentionDirty[hostedID] {
		needs := s.attentionCache[hostedID]
		s.mu.RUnlock()
		return needs
	}
	s.mu.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshAttentionLocked(hostedID)
	return s.attentionCache[hostedID]
}

// MarkTurnDelivery records a provider delivery outcome.
func (s *ExecutionStore) MarkTurnDelivery(operationID, providerTurnID, status string, executionErr *threads.ExecutionError) error {
	if s == nil || operationID == "" {
		return nil
	}
	return s.MarkTurnDeliveryWith(operationID, providerTurnID, status, executionErr, nil)
}

// MarkTurnDeliveryWith is markTurnDelivery that also makes with, a write the
// caller owes with it, in the same transaction on the threads database: both
// commit, or neither does. with runs even when the operation keeps its
// outcome.
func (s *ExecutionStore) MarkTurnDeliveryWith(operationID, providerTurnID, status string, executionErr *threads.ExecutionError, with func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.turns[operationID]
	if !ok || threads.TurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
		if with == nil {
			return nil
		}
		return s.persistAndNotifyWithLocked(with)
	}
	if providerTurnID != "" {
		rec.ProviderTurnID = providerTurnID
	}
	oldStatus := rec.Status
	rec.Status = status
	rec.Error = executionErr
	rec.RunID = s.runID
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked(oldStatus, rec)
	return s.persistAndNotifyWithLocked(with)
}

// ObserveCandidate records a possible final answer until terminal evidence arrives.
func (s *ExecutionStore) ObserveCandidate(provider, nativeThreadID, providerTurnID, itemID, phase, response string) error {
	if s == nil || providerTurnID == "" || response == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	// Every operation sharing the turn reads the same reply.
	for id, rec := range s.turns {
		if boundToTurn(rec, provider, nativeThreadID, providerTurnID) && s.observeCandidateLocked(id, itemID, phase, response) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *ExecutionStore) observeCandidateLocked(id, itemID, phase, response string) bool {
	rec, found := s.turns[id]
	if !found || response == "" || threads.TurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved || rec.CandidatePhase == "final_answer" && phase != "final_answer" {
		return false
	}
	rec.CandidateItemID = itemID
	rec.CandidatePhase = phase
	rec.CandidateResponse, rec.CandidateBytes, rec.CandidateTruncated = boundAgentResult(response)
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	return true
}

// MarkTurnTerminal records terminal provider evidence for matching operations.
func (s *ExecutionStore) MarkTurnTerminal(provider, nativeThreadID, providerTurnID, status string, executionErr *threads.ExecutionError, response, itemID string, resultMeta ...ResultMeta) error {
	if s == nil || providerTurnID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.savedResultRead != nil {
		changed := false
		for id, rec := range s.turns {
			if boundToTurn(rec, provider, nativeThreadID, providerTurnID) && s.markTurnTerminalLocked(id, status, executionErr, response, itemID, resultMeta...) {
				changed = true
			}
		}
		if changed {
			return s.persistAndNotifyLocked()
		}
		return nil
	}
	id := s.operationForTurnLocked(provider, nativeThreadID, providerTurnID)
	if !s.markTurnTerminalLocked(id, status, executionErr, response, itemID, resultMeta...) {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *ExecutionStore) markTurnTerminalLocked(id, status string, executionErr *threads.ExecutionError, response, itemID string, resultMeta ...ResultMeta) bool {
	rec, found := s.turns[id]
	if !found || threads.TurnTerminal(rec.Status) {
		return false
	}
	meta := ResultMeta{}
	if len(resultMeta) > 0 {
		meta = resultMeta[0]
	}
	if response == "" {
		// The turn's one operation observed its reply on the wire.
		response = rec.CandidateResponse
		if itemID == "" {
			itemID = rec.CandidateItemID
		}
		meta = ResultMeta{Bytes: rec.CandidateBytes, Truncated: rec.CandidateTruncated}
	}
	return s.applyTurnFinishLocked(id, status, executionErr, response, itemID, meta)
}

// FinishJoinedTurn finishes an operation whose own prompt the saved
// transcript proved part of a provider turn, with the turn's recorded outcome
// and the one final answer resolved for that turn, exactly as given.
func (s *ExecutionStore) FinishJoinedTurn(operationID, providerTurnID, outcome string, executionErr *threads.ExecutionError, response, itemID string, meta ResultMeta) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.turns[operationID]; !ok || rec.ProviderTurnID != providerTurnID {
		return nil
	}
	return s.finishTurnLocked(operationID, outcome, executionErr, response, itemID, meta)
}

// FinishResolvedTurn finishes an operation with an answer already read from
// its own saved turn, so no saved-result wait follows.
func (s *ExecutionStore) FinishResolvedTurn(operationID, providerTurnID, outcome string, executionErr *threads.ExecutionError, response, itemID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.turns[operationID]; !ok || rec.ProviderTurnID != providerTurnID {
		return nil
	}
	if !s.applyResolvedTurnFinishLocked(operationID, outcome, executionErr, response, itemID, ResultMeta{}) {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *ExecutionStore) finishTurnLocked(id, status string, executionErr *threads.ExecutionError, response, itemID string, meta ResultMeta) error {
	if !s.applyTurnFinishLocked(id, status, executionErr, response, itemID, meta) {
		return nil
	}
	return s.persistAndNotifyLocked()
}

func (s *ExecutionStore) applyTurnFinishLocked(id, status string, executionErr *threads.ExecutionError, response, itemID string, meta ResultMeta) bool {
	if s.savedResultRead != nil && threads.NormalizeTerminalStatus(status) == agentTurnCompleted {
		return s.awaitSavedResultLocked(id, itemID)
	}
	return s.applyResolvedTurnFinishLocked(id, status, executionErr, response, itemID, meta)
}

func (s *ExecutionStore) applyResolvedTurnFinishLocked(id, status string, executionErr *threads.ExecutionError, response, itemID string, meta ResultMeta) bool {
	rec, found := s.turns[id]
	if !found || threads.TurnTerminal(rec.Status) && !(threads.AnswerNotSaved(rec) && status == agentTurnCompleted) {
		return false
	}
	oldStatus := rec.Status
	rec.Status = threads.NormalizeTerminalStatus(status)
	rec.Error = executionErr
	if rec.Status == agentTurnCompleted {
		if meta.Truncated {
			rec.FinalResponse, _, _ = boundAgentResult(response)
			rec.FinalResponseBytes = meta.Bytes
			rec.FinalResponseTruncated = true
		} else {
			rec.FinalResponse, rec.FinalResponseBytes, rec.FinalResponseTruncated = boundAgentResult(response)
		}
		rec.FinalItemID = itemID
	}
	rec.CandidateItemID, rec.CandidatePhase, rec.CandidateResponse = "", "", ""
	rec.CandidateBytes, rec.CandidateTruncated = 0, false
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked(oldStatus, rec)
	return true
}

// MarkWaiting records whether an operation awaits input.
func (s *ExecutionStore) MarkWaiting(sessionID, providerTurnID string, waiting bool) error {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var chosen string
	for id, rec := range s.turns {
		if rec.HostedThreadID != sessionID || threads.TurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
			continue
		}
		if providerTurnID != "" && rec.ProviderTurnID != providerTurnID {
			continue
		}
		if chosen == "" || threads.TimestampBefore(s.turns[chosen].CreatedAt, rec.CreatedAt) {
			chosen = id
		}
	}
	if chosen == "" {
		return nil
	}
	rec := s.turns[chosen]
	oldStatus := rec.Status
	if waiting {
		rec.Status = agentTurnWaiting
	} else if rec.Status == agentTurnWaiting {
		rec.Status = agentTurnRunning
	}
	touchAgentTurn(&rec)
	s.setTurnLocked(rec.ID, &rec)
	s.noteTurnTransitionLocked(oldStatus, rec)
	return s.persistAndNotifyLocked()
}

// MarkSessionTurns sets the outcome of the session’s open operations.
func (s *ExecutionStore) MarkSessionTurns(sessionID, status string, executionErr *threads.ExecutionError) error {
	if s == nil || sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, rec := range s.turns {
		if rec.HostedThreadID != sessionID || threads.TurnTerminal(rec.Status) || rec.Status == agentTurnAwaitingSaved {
			continue
		}
		oldStatus := rec.Status
		rec.Status = status
		rec.Error = executionErr
		if threads.TurnTerminal(status) {
			rec.CandidateItemID, rec.CandidatePhase, rec.CandidateResponse = "", "", ""
			rec.CandidateBytes, rec.CandidateTruncated = 0, false
		}
		touchAgentTurn(&rec)
		s.setTurnLocked(rec.ID, &rec)
		s.noteTurnTransitionLocked(oldStatus, rec)
		changed = true
	}
	if !changed {
		return nil
	}
	return s.persistAndNotifyLocked()
}

// SharedTurnResult finds a provider turn's final answer by its exact item id
// among the operations bound to that turn in that native thread: one already
// finished with it, or one that observed it on the wire. Unlike boundToTurn,
// a record without a native thread never supplies another member's answer.
func (s *ExecutionStore) SharedTurnResult(hostedThreadID, provider, nativeThreadID, providerTurnID, finalItemID string) (string, ResultMeta, bool) {
	if s == nil || finalItemID == "" || nativeThreadID == "" {
		return "", ResultMeta{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.turns {
		if rec.HostedThreadID != hostedThreadID || rec.Provider != provider || rec.NativeThreadID != nativeThreadID || rec.ProviderTurnID != providerTurnID {
			continue
		}
		if rec.Status == agentTurnCompleted && rec.FinalItemID == finalItemID {
			return rec.FinalResponse, ResultMeta{Bytes: rec.FinalResponseBytes, Truncated: rec.FinalResponseTruncated}, true
		}
		if rec.CandidateItemID == finalItemID {
			return rec.CandidateResponse, ResultMeta{Bytes: rec.CandidateBytes, Truncated: rec.CandidateTruncated}, true
		}
	}
	return "", ResultMeta{}, false
}

// operationForTurnLocked finds the one operation a provider turn belongs to.
// A turn that several operations share has no single owner: each of them
// finishes only when the saved transcript proves its own prompt joined it.
func (s *ExecutionStore) operationForTurnLocked(provider, nativeThreadID, providerTurnID string) string {
	found := ""
	for id, rec := range s.turns {
		if !boundToTurn(rec, provider, nativeThreadID, providerTurnID) {
			continue
		}
		if found != "" {
			return ""
		}
		found = id
	}
	return found
}

func boundToTurn(rec threads.TurnRecord, provider, nativeThreadID, providerTurnID string) bool {
	return rec.Provider == provider && rec.ProviderTurnID == providerTurnID &&
		(nativeThreadID == "" || rec.NativeThreadID == "" || rec.NativeThreadID == nativeThreadID)
}

// WaitTurn waits for an operation revision or the caller’s deadline.
func (s *ExecutionStore) WaitTurn(ctx context.Context, id string, afterRevision int64, wait time.Duration) (threads.TurnRecord, bool) {
	deadline := time.NewTimer(wait)
	if wait <= 0 {
		deadline.Stop()
	}
	for {
		s.mu.RLock()
		rec, ok := s.turns[id]
		ch := s.changed
		s.mu.RUnlock()
		if !ok || rec.Revision > afterRevision || wait <= 0 {
			return rec, ok
		}
		select {
		case <-ctx.Done():
			return rec, true
		case <-deadline.C:
			return rec, true
		case <-ch:
		}
	}
}
