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

func (sess *Runtime) codexMayBeZeroTurn() bool {
	return sess.codexZeroTurnEvidence("")
}

func (sess *Runtime) codexZeroTurnEvidence(refusedAction string) bool {
	sess.session.stateOwner.stateMu.Lock()
	eligible := sess.session.stateOwner.codexZeroTurnEligible && !sess.session.stateOwner.closed
	sess.session.stateOwner.stateMu.Unlock()
	if !eligible {
		return false
	}
	var attempts []prompts.SendAttempt
	if sess.session.delivery.sendAttempts != nil {
		attempts = sess.session.delivery.sendAttempts.ListThread("codex", sess.session.stateOwner.ThreadID())
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
	if sess.session.agentExecutions != nil {
		for _, operation := range sess.session.agentExecutions.TurnsForHosted(sess.session.hostedThreadID) {
			if operation.ProviderTurnID != "" {
				return false
			}
		}
	}
	return sess.session.projection == nil || len(sess.session.projection.turnList()) == 0
}

func (sess *Runtime) codexZeroTurnNativeMissing(actionID string, err error) bool {
	var rpcErr *protocol.RPCError
	return ProviderLifecycleCapabilities(sess.session.provider).ResumeWithoutTranscript && errors.As(err, &rpcErr) &&
		rpcErr.Message == "thread not found: "+sess.session.stateOwner.ThreadID() &&
		sess.codexZeroTurnRecoveryNeeded() && sess.codexZeroTurnEvidence(actionID)
}

func (sess *Runtime) containCodexNativeMissing(message string) error {
	sess.session.stateOwner.stateMu.Lock()
	sess.session.stateOwner.codexZeroTurnEligible = false
	sess.session.stateOwner.codexZeroTurnResume = false
	sess.session.stateOwner.stateMu.Unlock()
	return sess.failCodexReconnect(message)
}

func (sess *Runtime) failCodexReconnect(message string) error {
	sess.session.delivery.sendMu.Lock()
	changed := false
	defer func() {
		sess.session.delivery.sendMu.Unlock()
		if !changed {
			return
		}
		sess.session.PublishProjectionRuntime()
		sess.session.stateOwner.NotifyRuntime()
		sess.session.stateOwner.updateHostedThread("reconnect_failed", message)
	}()
	sess.session.stateOwner.stateMu.Lock()
	closed := sess.session.stateOwner.closed || sess.session.stateOwner.connectionStatusLocked() == StatusExited
	if !closed {
		changed = true
		sess.session.stateOwner.transitionLifecycleLocked(lifecycle.EventNativeMissing)
		sess.session.stateOwner.spawnError = message
	}
	sess.session.stateOwner.stateMu.Unlock()
	if closed {
		return nil
	}
	for _, queued := range sess.session.delivery.queuedPrompts(sess.session.stateOwner.ThreadID()) {
		if queued.State != submissionQueued && queued.State != submissionParked {
			continue
		}
		attempt, ok := sess.session.delivery.sendAttempt(queued.ClientActionID)
		if queued.State == submissionParked {
			if !ok || (attempt.Reason != "reconnect_failed" && attempt.Reason != codexNativeMissing) {
				continue
			}
		} else if ok && attempt.Status != sendAttemptQueued && !(attempt.Status == sendAttemptRefused && attempt.Reason == "reconnect_failed") {
			continue
		}
		if sess.session.delivery.turns == nil {
			return fmt.Errorf("transactional turns store is unavailable")
		}
		var record prompts.Record
		var err error
		if queued.State == submissionParked {
			record, _, err = sess.session.delivery.turns.Get(context.Background(), queued.ClientActionID)
		} else {
			record, err = sess.session.delivery.turns.Cancel(context.Background(), queued.ClientActionID)
		}
		if err != nil {
			return fmt.Errorf("park terminal submission: %w", err)
		}
		sess.session.delivery.publishTransactionalOutbox(record.Outbox)
		if sess.session.agentExecutions != nil {
			if operation, found := sess.session.agentExecutions.TurnForTransportAction(queued.ClientActionID); found {
				if err := sess.session.agentExecutions.MarkTurnDelivery(operation.ID, "", agentTurnFailed,
					&threads.ExecutionError{Source: "reconnect", Code: "reconnect_failed", Message: message}); err != nil {
					return fmt.Errorf("settle terminal operation: %w", err)
				}
			}
		}
	}
	return nil
}

func (sess *Runtime) rejectFailedReconnect(w http.ResponseWriter) bool {
	sess.session.stateOwner.stateMu.Lock()
	failed := sess.session.stateOwner.connectionStatusLocked() == "reconnect_failed"
	sess.session.stateOwner.stateMu.Unlock()
	if failed {
		httpapi.WriteError(w, http.StatusConflict, "reconnect_failed", "Codex cannot recover this thread")
	}
	return failed
}
