package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/lifecycle"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const codexNativeMissing = "codex_native_thread_missing"

func (sess *session) codexMayBeZeroTurn() bool {
	return sess.codexZeroTurnEvidence("")
}

func (sess *session) codexZeroTurnEvidence(refusedAction string) bool {
	sess.stateMu.Lock()
	eligible := sess.codexZeroTurnEligible && !sess.closed
	sess.stateMu.Unlock()
	if !eligible {
		return false
	}
	var attempts []sendAttemptRecord
	if sess.sendAttempts != nil {
		attempts = sess.sendAttempts.listThread("codex", sess.currentThreadID())
	}
	for _, attempt := range attempts {
		if attempt.Reason == codexNativeMissing {
			return false
		}
		if refusedAction != "" && attempt.ClientActionID == refusedAction && attempt.Status == sendAttemptSending {
			continue
		}
		if attempt.Status != sendAttemptQueued && attempt.Status != sendAttemptRefused {
			return false
		}
	}
	if sess.agentExecutions != nil {
		for _, operation := range sess.agentExecutions.turnsForHosted(sess.hostedThreadID) {
			if operation.ProviderTurnID != "" {
				return false
			}
		}
	}
	return sess.projection == nil || len(sess.projection.turnList()) == 0
}

func (sess *session) codexZeroTurnNativeMissing(actionID string, err error) bool {
	var rpcErr *protocol.RPCError
	return providerLifecycleCapabilities(sess.provider).ResumeWithoutTranscript && errors.As(err, &rpcErr) &&
		rpcErr.Message == "thread not found: "+sess.currentThreadID() &&
		sess.codexZeroTurnRecoveryNeeded() && sess.codexZeroTurnEvidence(actionID)
}

func (sess *session) containCodexNativeMissing(message string) error {
	sess.stateMu.Lock()
	sess.codexZeroTurnEligible = false
	sess.codexZeroTurnResume = false
	sess.stateMu.Unlock()
	return sess.failCodexReconnect(message)
}

func (sess *session) failCodexReconnect(message string) error {
	sess.sendMu.Lock()
	changed := false
	defer func() {
		sess.sendMu.Unlock()
		if !changed {
			return
		}
		sess.publishProjectionRuntime()
		sess.notifyRuntime()
		sess.updateHostedThread("reconnect_failed", message)
	}()
	sess.stateMu.Lock()
	closed := sess.closed || sess.connectionStatusLocked() == sessionStatusExited
	if !closed {
		changed = true
		sess.transitionLifecycleLocked(lifecycle.EventNativeMissing)
		sess.spawnError = message
	}
	sess.stateMu.Unlock()
	if closed {
		return nil
	}
	for _, queued := range sess.queuedPrompts(sess.currentThreadID()) {
		if queued.State != submissionQueued && queued.State != submissionParked {
			continue
		}
		attempt, ok := sess.sendAttempt(queued.ClientActionID)
		if queued.State == submissionParked {
			if !ok || (attempt.Reason != "reconnect_failed" && attempt.Reason != codexNativeMissing) {
				continue
			}
		} else if ok && attempt.Status != sendAttemptQueued && !(attempt.Status == sendAttemptRefused && attempt.Reason == "reconnect_failed") {
			continue
		}
		if sess.turns == nil {
			return fmt.Errorf("transactional turns store is unavailable")
		}
		var record prompts.Record
		var err error
		if queued.State == submissionParked {
			record, _, err = sess.turns.Get(context.Background(), queued.ClientActionID)
		} else {
			record, err = sess.turns.Cancel(context.Background(), queued.ClientActionID)
		}
		if err != nil {
			return fmt.Errorf("park terminal submission: %w", err)
		}
		sess.publishTransactionalOutbox(record.Outbox)
		if sess.agentExecutions != nil {
			if operation, found := sess.agentExecutions.turnForTransportAction(queued.ClientActionID); found {
				if err := sess.agentExecutions.markTurnDelivery(operation.ID, "", agentTurnFailed,
					&agentExecutionError{Source: "reconnect", Code: "reconnect_failed", Message: message}); err != nil {
					return fmt.Errorf("settle terminal operation: %w", err)
				}
			}
		}
	}
	return nil
}

func (sess *session) rejectFailedReconnect(w http.ResponseWriter) bool {
	sess.stateMu.Lock()
	failed := sess.connectionStatusLocked() == "reconnect_failed"
	sess.stateMu.Unlock()
	if failed {
		httpapi.WriteError(w, http.StatusConflict, "reconnect_failed", "Codex cannot recover this thread")
	}
	return failed
}
