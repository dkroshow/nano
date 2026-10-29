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
	return sess.stateOwner.codexZeroTurnEvidence("")
}

func (sess *State) codexZeroTurnEvidence(refusedAction string) bool {
	sess.stateMu.Lock()
	eligible := sess.codexZeroTurnEligible && !sess.closed
	sess.stateMu.Unlock()
	if !eligible {
		return false
	}
	var attempts []prompts.SendAttempt
	if sess.delivery.sendJournal() != nil {
		attempts = sess.delivery.sendJournal().ListThread("codex", sess.ThreadID())
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
	if sess.identity.agentExecutions != nil {
		for _, operation := range sess.identity.agentExecutions.TurnsForHosted(sess.identity.hostedThreadID) {
			if operation.ProviderTurnID != "" {
				return false
			}
		}
	}
	return sess.view.liveProjection() == nil || len(sess.view.liveProjection().turnList()) == 0
}

func (sess *Runtime) codexZeroTurnNativeMissing(actionID string, err error) bool {
	var rpcErr *protocol.RPCError
	return ProviderLifecycleCapabilities(sess.identity.provider).ResumeWithoutTranscript && errors.As(err, &rpcErr) &&
		rpcErr.Message == "thread not found: "+sess.stateOwner.ThreadID() &&
		sess.stateOwner.codexZeroTurnRecoveryNeeded() && sess.stateOwner.codexZeroTurnEvidence(actionID)
}

func (sess *State) containCodexNativeMissing(message string) error {
	sess.stateMu.Lock()
	sess.codexZeroTurnEligible = false
	sess.codexZeroTurnResume = false
	sess.stateMu.Unlock()
	return sess.delivery.failCodexReconnect(message)
}

func (sess *Delivery) failCodexReconnect(message string) error {
	sess.sendMu.Lock()
	changed := false
	defer func() {
		sess.sendMu.Unlock()
		if !changed {
			return
		}
		sess.stateOwner.PublishProjectionRuntime()
		sess.stateOwner.NotifyRuntime()
		sess.stateOwner.updateHostedThread("reconnect_failed", message)
	}()
	closed, didChange := sess.stateOwner.markReconnectFailed(message)
	changed = didChange
	if closed {
		return nil
	}
	for _, queued := range sess.queuedPrompts(sess.stateOwner.ThreadID()) {
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
		if sess.identity.agentExecutions != nil {
			if operation, found := sess.identity.agentExecutions.TurnForTransportAction(queued.ClientActionID); found {
				if err := sess.identity.agentExecutions.MarkTurnDelivery(operation.ID, "", agentTurnFailed,
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
