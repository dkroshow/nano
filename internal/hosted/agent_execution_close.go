package hosted

import (
	"errors"
	"fmt"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// ResolveHostedRecord accepts the durable hosted-thread id, an optional
// client creation alias, the current Nano session id, or the live provider
// thread id. All references converge on the same durable record before an
// operation is created.
func (s *Controller) ResolveHostedRecord(ref string) (ownedthreads.SessionRecord, bool) {
	if record, ok := s.Executions.ResolveSession(ref); ok {
		return record, true
	}
	return s.Runtime.ResolveLiveRecord(ref, s.Executions.Session)
}

// closeHostedThreadForRecord ends a hosted thread's attachment whether or not
// a live session exists. Callers hold relationshipMu. The detached branch
// settles non-terminal operations explicitly — previously they dangled
// forever, because only a live session's close path ran settlement.
func (s *Controller) closeHostedThreadForRecord(record ownedthreads.SessionRecord) error {
	return s.CloseLifecycle(record.ID, func() error { return s.closeHostedThreadForRecordCore(record) })
}

// closeHostedThreadForRecordCore closes only the provider attachment while
// callers hold any feature-specific lifecycle locks.
func (s *Controller) closeHostedThreadForRecordCore(record ownedthreads.SessionRecord) error {
	if sess := s.Runtime.CloseSession(record.ID); sess != nil {
		return s.closeAgentSession(sess)
	}
	if s.Attachments != nil && record.NativeThreadID != "" {
		if attachment, ok := s.Attachments.Get(record.Provider, record.NativeThreadID); ok {
			if attachment.HostedThreadID != "" && attachment.HostedThreadID != record.ID {
				return errors.New("attachment belongs to another hosted thread; close refused")
			}
			if err := s.closeDetachedAttachment(attachment); err != nil {
				return err
			}
		}
	}
	if err := s.Executions.MarkSessionTurns(record.ID, ownedthreads.AgentTurnInterrupted, &ownedthreads.ExecutionError{
		Source: "client", Code: "session_closed",
		Message: "the hosted thread was closed while detached",
	}); err != nil {
		return err
	}
	// Persist the terminal state inside the heartbeat lifecycle lock. The
	// public close and bulk-close handlers repeat this update with their own
	// reason, but doing it here prevents a concurrent attachment from slipping
	// into the detached-close gap.
	return s.Executions.UpdateSession(record.ID, "", "", hostedstore.StatusExited, "closed")
}

// dropQueuedTurns removes undelivered queued prompts for the thread: an
// operation settled as interrupted must not redeliver its prompt on a later
// reattachment.
func (s *Controller) dropQueuedTurns(record ownedthreads.SessionRecord) error {
	if s.Queue == nil {
		return nil
	}
	for {
		rec, ok := s.Queue.First(record.Provider, record.NativeThreadID, record.ID)
		if !ok {
			return nil
		}
		if rec.ClientActionID == "" {
			return nil
		}
		if err := s.Runtime.CancelStoredPrompt(rec.ClientActionID); err != nil {
			return err
		}
	}
}

func (s *Controller) closeAgentSession(sess CloseSession) error {
	if sess == nil {
		return nil
	}
	var attachment hostedstore.Attachment
	var remembered bool
	if s.Attachments != nil {
		attachment, remembered = s.Attachments.Get(sess.Provider(), sess.ThreadID())
	}
	if remembered && sess.Provider() == "claude" {
		if err := sess.VerifyClaudeAttachment(attachment.ShimSessionID); err != nil {
			return err
		}
	}
	err := sess.CloseWithResult()
	if !sess.IsClosed() {
		return err
	}
	sess.RemoveCurrent()
	defer s.NotifyRuntime()
	if err != nil {
		return fmt.Errorf("Claude stop unconfirmed; attachment retained for retry: %w", err)
	}
	if remembered {
		if err := s.Attachments.ForgetClosed(attachment); err != nil {
			return err
		}
	}
	sess.ConfirmClaudeClose()
	return nil
}
