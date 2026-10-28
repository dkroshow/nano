package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const transcriptRepairWait = 10 * time.Second

type transcriptRepairPrompt struct {
	ClientActionID string `json:"clientActionId"`
	Text           string `json:"text"`
}

type transcriptRepairResponse struct {
	ClientActionID string                   `json:"clientActionId"`
	Interrupted    bool                     `json:"interrupted"`
	Discarded      []transcriptRepairPrompt `json:"discarded"`
	Repaired       bool                     `json:"repaired"`
	Status         string                   `json:"status"`
	Detail         string                   `json:"detail,omitempty"`
	threadID       string
	attachmentGen  int64
}

// stopDiscardAndRepair is one fenced operator action. sendMu prevents the
// ordinary completion drain or a newly submitted prompt from crossing the
// stop/discard boundary. Repaired means this attachment's saved file is
// proven current and its prompts settled, with the attachment still the one
// stopped, idle and ready; panes then read that file. A timeout returns
// recovered prompt text but never claims the repair.
func (sess *session) stopDiscardAndRepair(parent context.Context, actionID string) (transcriptRepairResponse, error) {
	return sess.stopDiscardAndRepairWithin(parent, actionID, transcriptRepairWait)
}

func (sess *session) stopDiscardAndRepairWithin(parent context.Context, actionID string, wait time.Duration) (transcriptRepairResponse, error) {
	if sess == nil || sess.catalogSources == nil {
		return transcriptRepairResponse{}, errors.New("transcript repair is unavailable")
	}
	sess.transcriptRepairMu.Lock()
	defer sess.transcriptRepairMu.Unlock()
	if cached, ok := sess.transcriptRepairResults[actionID]; ok {
		sess.stateMu.Lock()
		stale := sess.threadID != cached.threadID || sess.attachmentGeneration != cached.attachmentGen
		sess.stateMu.Unlock()
		if stale {
			return transcriptRepairResponse{}, errors.New("transcript repair result belongs to a prior attachment")
		}
		return cached, nil
	}

	// Once accepted, finish the bounded action even if the browser disconnects.
	// Otherwise a lost response could cancel prompts without retaining the
	// idempotent result needed to restore their text on retry.
	_ = parent
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()

	sess.stateMu.Lock()
	threadID := sess.threadID
	attachmentGeneration := sess.attachmentGeneration
	sess.stateMu.Unlock()
	if threadID == "" {
		return transcriptRepairResponse{}, errors.New("provider thread is not attached")
	}

	queue := sess.queuedPrompts(threadID)
	discardable := make([]queuedTurnRecord, 0, len(queue))
	actionIDs := make([]string, 0, len(queue))
	for _, record := range queue {
		if record.State != submissionQueued && record.State != submissionParked {
			continue
		}
		discardable = append(discardable, record)
		actionIDs = append(actionIDs, record.ClientActionID)
	}

	interrupt, err := sess.interruptTurn(ctx)
	if err != nil {
		return transcriptRepairResponse{}, fmt.Errorf("stop failed: %w", err)
	}
	if err := waitForTranscriptRepairIdle(ctx, sess); err != nil {
		return transcriptRepairResponse{}, err
	}

	if len(actionIDs) > 0 {
		if sess.turns == nil {
			return transcriptRepairResponse{}, errors.New("prompt store is unavailable")
		}
		canceled, cancelErr := sess.turns.CancelQueuedBatch(ctx, actionIDs)
		if cancelErr != nil {
			return transcriptRepairResponse{}, fmt.Errorf("discard failed: %w", cancelErr)
		}
		for _, record := range canceled {
			sess.publishTransactionalOutbox(record.Outbox)
		}
	}

	response := transcriptRepairResponse{
		ClientActionID: actionID,
		Discarded:      make([]transcriptRepairPrompt, 0, len(discardable)),
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
		response.Discarded = append(response.Discarded, transcriptRepairPrompt{ClientActionID: record.ClientActionID, Text: text})
	}

	// A fresh pass proves the file current after the stop. The wait follows
	// this session's hold, so the recheck after it, whatever the waits found,
	// names a replaced attachment as stale.
	caughtUp := sess.catalogSources.awaitAttachedCurrent(sess.id, true, nil, ctx.Done())
	var settleErr error
	if caughtUp {
		settleErr = sess.settleSavedPrompts(ctx)
	}
	sess.stateMu.Lock()
	detail := sess.repairableLocked(threadID, attachmentGeneration)
	sess.stateMu.Unlock()
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
		sess.transcriptRepairResults = make(map[string]transcriptRepairResponse)
	}
	// One attachment needs only a small replay window. The browser retains the
	// same action id across a transport retry.
	if len(sess.transcriptRepairResults) >= 8 {
		sess.transcriptRepairResults = make(map[string]transcriptRepairResponse)
	}
	sess.transcriptRepairResults[actionID] = response
	return response, nil
}

func waitForTranscriptRepairIdle(ctx context.Context, sess *session) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		sess.stateMu.Lock()
		active := sess.workActiveLocked() || sess.turnRestoreInProgress
		sess.stateMu.Unlock()
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
func (sess *session) repairableLocked(threadID string, attachmentGeneration int64) string {
	if sess.threadID != threadID || sess.attachmentGeneration != attachmentGeneration {
		return transcriptRepairStale
	}
	if sess.closed || sess.connectionStatusLocked() != "ready" || sess.workActiveLocked() || sess.turnRestoreInProgress {
		return "attachment is not idle and ready"
	}
	if len(sess.interactions) != 0 {
		return "a provider interaction is still pending"
	}
	return ""
}
