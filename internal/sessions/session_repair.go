package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
)

const transcriptRepairWait = 10 * time.Second

// TranscriptRepairPrompt is a queued prompt recovered by a repair action.
type TranscriptRepairPrompt struct {
	ClientActionID string `json:"clientActionId"`
	Text           string `json:"text"`
}

// TranscriptRepairResponse reports the stopped attachment and recovered prompts.
type TranscriptRepairResponse struct {
	ClientActionID string                   `json:"clientActionId"`
	Interrupted    bool                     `json:"interrupted"`
	Discarded      []TranscriptRepairPrompt `json:"discarded"`
	Repaired       bool                     `json:"repaired"`
	Status         string                   `json:"status"`
	Detail         string                   `json:"detail,omitempty"`
	threadID       string
	attachmentGen  int64
}

// StopDiscardAndRepair is one fenced operator action. sendMu prevents the
// ordinary completion drain or a newly submitted prompt from crossing the
// stop/discard boundary. Repaired means this attachment's saved file is
// proven current and its prompts settled, with the attachment still the one
// stopped, idle and ready; panes then read that file. A timeout returns
// recovered prompt text but never claims the repair.
func (sess *Session) StopDiscardAndRepair(parent context.Context, actionID string) (TranscriptRepairResponse, error) {
	return sess.stopDiscardAndRepairWithin(parent, actionID, transcriptRepairWait)
}

func (sess *Session) stopDiscardAndRepairWithin(parent context.Context, actionID string, wait time.Duration) (TranscriptRepairResponse, error) {
	if sess == nil || sess.stateOwner.catalogSources == nil {
		return TranscriptRepairResponse{}, errors.New("transcript repair is unavailable")
	}
	sess.transcriptRepairMu.Lock()
	defer sess.transcriptRepairMu.Unlock()
	if cached, ok := sess.transcriptRepairResults[actionID]; ok {
		sess.stateOwner.stateMu.Lock()
		stale := sess.stateOwner.threadID != cached.threadID || sess.stateOwner.attachmentGeneration != cached.attachmentGen
		sess.stateOwner.stateMu.Unlock()
		if stale {
			return TranscriptRepairResponse{}, errors.New("transcript repair result belongs to a prior attachment")
		}
		return cached, nil
	}

	// Once accepted, finish the bounded action even if the browser disconnects.
	// Otherwise a lost response could cancel prompts without retaining the
	// idempotent result needed to restore their text on retry.
	_ = parent
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	sess.delivery.sendMu.Lock()
	defer sess.delivery.sendMu.Unlock()

	sess.stateOwner.stateMu.Lock()
	threadID := sess.stateOwner.threadID
	attachmentGeneration := sess.stateOwner.attachmentGeneration
	sess.stateOwner.stateMu.Unlock()
	if threadID == "" {
		return TranscriptRepairResponse{}, errors.New("provider thread is not attached")
	}

	queue := sess.delivery.queuedPrompts(threadID)
	discardable := make([]prompts.QueuedTurn, 0, len(queue))
	actionIDs := make([]string, 0, len(queue))
	for _, record := range queue {
		if record.State != submissionQueued && record.State != submissionParked {
			continue
		}
		discardable = append(discardable, record)
		actionIDs = append(actionIDs, record.ClientActionID)
	}

	interrupt, err := sess.delivery.interruptTurn(ctx)
	if err != nil {
		return TranscriptRepairResponse{}, fmt.Errorf("stop failed: %w", err)
	}
	if err := waitForTranscriptRepairIdle(ctx, sess); err != nil {
		return TranscriptRepairResponse{}, err
	}

	if len(actionIDs) > 0 {
		if sess.delivery.turns == nil {
			return TranscriptRepairResponse{}, errors.New("prompt store is unavailable")
		}
		canceled, cancelErr := sess.delivery.turns.CancelQueuedBatch(ctx, actionIDs)
		if cancelErr != nil {
			return TranscriptRepairResponse{}, fmt.Errorf("discard failed: %w", cancelErr)
		}
		for _, record := range canceled {
			sess.delivery.publishTransactionalOutbox(record.Outbox)
		}
	}

	response := TranscriptRepairResponse{
		ClientActionID: actionID,
		Discarded:      make([]TranscriptRepairPrompt, 0, len(discardable)),
		Status:         "saved_copy_not_ready",
		threadID:       threadID,
		attachmentGen:  attachmentGeneration,
	}
	if interrupted, _ := interrupt["interrupted"].(bool); interrupted {
		response.Interrupted = true
	}
	for _, record := range discardable {
		text := record.PresentationText
		if text == "" {
			text = record.Text
		}
		response.Discarded = append(response.Discarded, TranscriptRepairPrompt{ClientActionID: record.ClientActionID, Text: text})
	}

	// A fresh pass proves the file current after the stop. The wait follows
	// this session's hold, so the recheck after it, whatever the waits found,
	// names a replaced attachment as stale.
	caughtUp := sess.stateOwner.catalogSources.AwaitAttachedCurrent(sess.id, true, nil, ctx.Done())
	var settleErr error
	if caughtUp {
		settleErr = sess.delivery.settleSavedPrompts(ctx)
	}
	sess.stateOwner.stateMu.Lock()
	detail := sess.repairableLocked(threadID, attachmentGeneration)
	sess.stateOwner.stateMu.Unlock()
	switch {
	case detail == transcriptRepairStale:
		response.Status, response.Detail = "stale_attachment", detail
	case !caughtUp:
		response.Detail = "saved file is not proven current"
	case settleErr != nil:
		response.Detail = settleErr.Error()
	case detail != "":
		response.Detail = detail
	default:
		response.Repaired, response.Status = true, "repaired"
	}

	if sess.transcriptRepairResults == nil {
		sess.transcriptRepairResults = make(map[string]TranscriptRepairResponse)
	}
	// One attachment needs only a small replay window. The browser retains the
	// same action id across a transport retry.
	if len(sess.transcriptRepairResults) >= 8 {
		sess.transcriptRepairResults = make(map[string]TranscriptRepairResponse)
	}
	sess.transcriptRepairResults[actionID] = response
	return response, nil
}

func waitForTranscriptRepairIdle(ctx context.Context, sess *Session) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		sess.stateOwner.stateMu.Lock()
		active := sess.stateOwner.workActiveLocked() || sess.stateOwner.turnRestoreInProgress
		sess.stateOwner.stateMu.Unlock()
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("stop did not reach an idle provider boundary")
		case <-ticker.C:
		}
	}
}

const transcriptRepairStale = "attachment changed while repair was running"

// repairableLocked says why the repair cannot be reported now, or "" when the
// stopped attachment is unchanged, idle and ready. Callers hold stateMu.
func (sess *Session) repairableLocked(threadID string, attachmentGeneration int64) string {
	if sess.stateOwner.threadID != threadID || sess.stateOwner.attachmentGeneration != attachmentGeneration {
		return transcriptRepairStale
	}
	if sess.stateOwner.closed || sess.stateOwner.connectionStatusLocked() != "ready" || sess.stateOwner.workActiveLocked() || sess.stateOwner.turnRestoreInProgress {
		return "attachment is not idle and ready"
	}
	if len(sess.stateOwner.interactions) != 0 {
		return "a provider interaction is still pending"
	}
	return ""
}
