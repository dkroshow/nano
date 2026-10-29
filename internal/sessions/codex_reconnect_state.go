package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/threads"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const codexNativeMissing = "codex_native_thread_missing"

func (sess *Session) codexMayBeZeroTurn() bool {
	return sess.codexZeroTurnEvidence("")
}

func (sess *Session) codexZeroTurnEvidence(refusedAction string) bool {
	sess.stateMu.Lock()
	eligible := sess.codexZeroTurnEligible && !sess.closed
	sess.stateMu.Unlock()
	if !eligible {
		return false
	}
	var attempts []prompts.SendAttempt
	if sess.delivery.sendAttempts != nil {
		attempts = sess.delivery.sendAttempts.ListThread("codex", sess.ThreadID())
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
		for _, operation := range sess.agentExecutions.TurnsForHosted(sess.hostedThreadID) {
			if operation.ProviderTurnID != "" {
				return false
			}
		}
	}
	return sess.projection == nil || len(sess.projection.turnList()) == 0
}

func (sess *Session) codexZeroTurnNativeMissing(actionID string, err error) bool {
	var rpcErr *protocol.RPCError
	return ProviderLifecycleCapabilities(sess.provider).ResumeWithoutTranscript && errors.As(err, &rpcErr) &&
		rpcErr.Message == "thread not found: "+sess.ThreadID() &&
		sess.codexZeroTurnRecoveryNeeded() && sess.codexZeroTurnEvidence(actionID)
}

func (sess *Session) containCodexNativeMissing(message string) error {
	sess.stateMu.Lock()
	sess.codexZeroTurnEligible = false
	sess.codexZeroTurnResume = false
	sess.stateMu.Unlock()
	return sess.failCodexReconnect(message)
}

func (sess *Session) failCodexReconnect(message string) error {
	sess.delivery.sendMu.Lock()
	changed := false
	defer func() {
		sess.delivery.sendMu.Unlock()
		if !changed {
			return
		}
		sess.PublishProjectionRuntime()
		sess.NotifyRuntime()
		sess.updateHostedThread("reconnect_failed", message)
	}()
	sess.stateMu.Lock()
	closed := sess.closed || sess.connectionStatusLocked() == StatusExited
	if !closed {
		changed = true
		sess.transitionLifecycleLocked(lifecycle.EventNativeMissing)
		sess.spawnError = message
	}
	sess.stateMu.Unlock()
	if closed {
		return nil
	}
	for _, queued := range sess.delivery.queuedPrompts(sess.ThreadID()) {
		if queued.State != submissionQueued && queued.State != submissionParked {
			continue
		}
		attempt, ok := sess.delivery.sendAttempt(queued.ClientActionID)
		if queued.State == submissionParked {
			if !ok || (attempt.Reason != "reconnect_failed" && attempt.Reason != codexNativeMissing) {
				continue
			}
		} else if ok && attempt.Status != sendAttemptQueued && !(attempt.Status == sendAttemptRefused && attempt.Reason == "reconnect_failed") {
			continue
		}
		if sess.delivery.turns == nil {
			return fmt.Errorf("transactional turns store is unavailable")
		}
		var record prompts.Record
		var err error
		if queued.State == submissionParked {
			record, _, err = sess.delivery.turns.Get(context.Background(), queued.ClientActionID)
		} else {
			record, err = sess.delivery.turns.Cancel(context.Background(), queued.ClientActionID)
		}
		if err != nil {
			return fmt.Errorf("park terminal submission: %w", err)
		}
		sess.delivery.publishTransactionalOutbox(record.Outbox)
		if sess.agentExecutions != nil {
			if operation, found := sess.agentExecutions.TurnForTransportAction(queued.ClientActionID); found {
				if err := sess.agentExecutions.MarkTurnDelivery(operation.ID, "", agentTurnFailed,
					&threads.ExecutionError{Source: "reconnect", Code: "reconnect_failed", Message: message}); err != nil {
					return fmt.Errorf("settle terminal operation: %w", err)
				}
			}
		}
	}
	return nil
}

func (sess *Session) rejectFailedReconnect(w http.ResponseWriter) bool {
	sess.stateMu.Lock()
	failed := sess.connectionStatusLocked() == "reconnect_failed"
	sess.stateMu.Unlock()
	if failed {
		httpapi.WriteError(w, http.StatusConflict, "reconnect_failed", "Codex cannot recover this thread")
	}
	return failed
}
