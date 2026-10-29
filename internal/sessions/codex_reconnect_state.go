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

func (providerRuntime *Runtime) codexMayBeZeroTurn() bool {
	return providerRuntime.state.codexZeroTurnEvidence("")
}

func (attachmentState *State) codexZeroTurnEvidence(refusedAction string) bool {
	attachmentState.stateMu.Lock()
	eligible := attachmentState.codexZeroTurnEligible && !attachmentState.closed
	attachmentState.stateMu.Unlock()
	if !eligible {
		return false
	}
	var attempts []prompts.SendAttempt
	if attachmentState.delivery.sendJournal() != nil {
		attempts = attachmentState.delivery.sendJournal().ListThread("codex", attachmentState.ThreadID())
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
	if attachmentState.identity.agentExecutions != nil {
		for _, operation := range attachmentState.identity.agentExecutions.TurnsForHosted(attachmentState.identity.hostedThreadID) {
			if operation.ProviderTurnID != "" {
				return false
			}
		}
	}
	return attachmentState.view.liveProjection() == nil || len(attachmentState.view.liveProjection().turnList()) == 0
}

func (providerRuntime *Runtime) codexZeroTurnNativeMissing(actionID string, err error) bool {
	var rpcErr *protocol.RPCError
	return ProviderLifecycleCapabilities(providerRuntime.identity.provider).ResumeWithoutTranscript && errors.As(err, &rpcErr) &&
		rpcErr.Message == "thread not found: "+providerRuntime.state.ThreadID() &&
		providerRuntime.state.codexZeroTurnRecoveryNeeded() && providerRuntime.state.codexZeroTurnEvidence(actionID)
}

func (attachmentState *State) containCodexNativeMissing(message string) error {
	attachmentState.stateMu.Lock()
	attachmentState.codexZeroTurnEligible = false
	attachmentState.codexZeroTurnResume = false
	attachmentState.stateMu.Unlock()
	return attachmentState.delivery.failCodexReconnect(message)
}

func (delivery *Delivery) failCodexReconnect(message string) error {
	delivery.sendMu.Lock()
	changed := false
	defer func() {
		delivery.sendMu.Unlock()
		if !changed {
			return
		}
		delivery.state.PublishProjectionRuntime()
		delivery.state.NotifyRuntime()
		delivery.state.updateHostedThread("reconnect_failed", message)
	}()
	closed, didChange := delivery.state.markReconnectFailed(message)
	changed = didChange
	if closed {
		return nil
	}
	for _, queued := range delivery.queuedPrompts(delivery.state.ThreadID()) {
		if queued.State != submissionQueued && queued.State != submissionParked {
			continue
		}
		attempt, ok := delivery.sendAttempt(queued.ClientActionID)
		if queued.State == submissionParked {
			if !ok || (attempt.Reason != "reconnect_failed" && attempt.Reason != codexNativeMissing) {
				continue
			}
		} else if ok && attempt.Status != sendAttemptQueued && !(attempt.Status == sendAttemptRefused && attempt.Reason == "reconnect_failed") {
			continue
		}
		if delivery.turns == nil {
			return fmt.Errorf("transactional turns store is unavailable")
		}
		var record prompts.Record
		var err error
		if queued.State == submissionParked {
			record, _, err = delivery.turns.Get(context.Background(), queued.ClientActionID)
		} else {
			record, err = delivery.turns.Cancel(context.Background(), queued.ClientActionID)
		}
		if err != nil {
			return fmt.Errorf("park terminal submission: %w", err)
		}
		delivery.publishTransactionalOutbox(record.Outbox)
		if delivery.identity.agentExecutions != nil {
			if operation, found := delivery.identity.agentExecutions.TurnForTransportAction(queued.ClientActionID); found {
				if err := delivery.identity.agentExecutions.MarkTurnDelivery(operation.ID, "", agentTurnFailed,
					&threads.ExecutionError{Source: "reconnect", Code: "reconnect_failed", Message: message}); err != nil {
					return fmt.Errorf("settle terminal operation: %w", err)
				}
			}
		}
	}
	return nil
}

func (attachmentState *State) rejectFailedReconnect(w http.ResponseWriter) bool {
	attachmentState.stateMu.Lock()
	failed := attachmentState.connectionStatusLocked() == "reconnect_failed"
	attachmentState.stateMu.Unlock()
	if failed {
		httpapi.WriteError(w, http.StatusConflict, "reconnect_failed", "Codex cannot recover this thread")
	}
	return failed
}
