package sessions

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threads"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const codexNativeMissing = "codex_native_thread_missing"

func (sess *Runtime) codexMayBeZeroTurn() bool {
	return sess.session.stateOwner.codexZeroTurnEvidence("")
}

func (sess *State) codexZeroTurnEvidence(refusedAction string) bool {
	sess.stateMu.Lock()
	eligible := sess.codexZeroTurnEligible && !sess.closed
	sess.stateMu.Unlock()
	if !eligible {
		return false
	}
	var attempts []prompts.SendAttempt
	if sess.session.delivery.sendAttempts != nil {
		attempts = sess.session.delivery.sendAttempts.ListThread("codex", sess.ThreadID())
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
	return sess.session.view.projection == nil || len(sess.session.view.projection.turnList()) == 0
}

func (sess *Runtime) codexZeroTurnNativeMissing(actionID string, err error) bool {
	var rpcErr *protocol.RPCError
	return ProviderLifecycleCapabilities(sess.session.provider).ResumeWithoutTranscript && errors.As(err, &rpcErr) &&
		rpcErr.Message == "thread not found: "+sess.session.stateOwner.ThreadID() &&
		sess.session.stateOwner.codexZeroTurnRecoveryNeeded() && sess.session.stateOwner.codexZeroTurnEvidence(actionID)
}

func (sess *State) containCodexNativeMissing(message string) error {
	sess.stateMu.Lock()
	sess.codexZeroTurnEligible = false
	sess.codexZeroTurnResume = false
	sess.stateMu.Unlock()
	return sess.session.delivery.failCodexReconnect(message)
}

func (sess *Delivery) failCodexReconnect(message string) error {
	sess.sendMu.Lock()
	changed := false
	defer func() {
		sess.sendMu.Unlock()
		if !changed {
			return
		}
		sess.session.stateOwner.PublishProjectionRuntime()
		sess.session.stateOwner.NotifyRuntime()
		sess.session.stateOwner.updateHostedThread("reconnect_failed", message)
	}()
	closed, didChange := sess.session.stateOwner.markReconnectFailed(message)
	changed = didChange
	if closed {
		return nil
	}
	for _, queued := range sess.queuedPrompts(sess.session.stateOwner.ThreadID()) {
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

func (sess *State) rejectFailedReconnect(w http.ResponseWriter) bool {
	sess.stateMu.Lock()
	failed := sess.connectionStatusLocked() == "reconnect_failed"
	sess.stateMu.Unlock()
	if failed {
		httpapi.WriteError(w, http.StatusConflict, "reconnect_failed", "Codex cannot recover this thread")
	}
	return failed
}
