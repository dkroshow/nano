package hosted

import (
	"fmt"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

// Explicit operator escape: forget bookkeeping only, never claim termination
// or settle queued work. Refuse an attachment that has changed or resumed.
func (s *Controller) forgetUnconfirmedClaude(attachment hostedstore.Attachment, shimID, attachedAt string) error {
	if attachment.ShimSessionID != shimID || attachment.AttachedAt != attachedAt {
		return fmt.Errorf("attachment changed; refresh before forgetting")
	}
	return s.Sessions.ForgetDetachedClaude(attachment.NativeThreadID, func() error {
		return s.Attachments.ForgetClosed(attachment)
	})
}

// All helpers require relationshipMu. Legacy UI endpoints use the hosted
// lifecycle when possible, including heartbeat pause.
func (s *Controller) closeExplicitSession(sess *sessions.Session) error {
	if s.Sessions.LookupSession(sess.ID()) != sess {
		return fmt.Errorf("session attachment changed; refresh before closing")
	}
	if s.Executions != nil && sess.HostedThreadID() != "" {
		if record, ok := s.ResolveHostedRecord(sess.HostedThreadID()); ok {
			if err := s.closeHostedThreadForRecord(record); err != nil {
				return err
			}
			return s.dropQueuedTurns(record)
		}
	}
	return s.closeAgentSession(sess)
}

func (s *Controller) closeExplicitAttachment(attachment hostedstore.Attachment) error {
	if s.Executions != nil && attachment.HostedThreadID != "" {
		if record, ok := s.ResolveHostedRecord(attachment.HostedThreadID); ok {
			if record.Provider != attachment.Provider || record.NativeThreadID != attachment.NativeThreadID {
				return fmt.Errorf("attachment identity conflicts with hosted thread; close refused")
			}
			if err := s.closeHostedThreadForRecord(record); err != nil {
				return err
			}
			return s.dropQueuedTurns(record)
		}
	}
	// A stale detached control must not forget a runtime that has since resumed.
	live := s.Sessions.AttachmentSession(attachment.Provider, attachment.NativeThreadID)
	if live != nil {
		return s.closeAgentSession(live)
	}
	return s.closeDetachedAttachment(attachment)
}

func (s *Controller) closeDetachedAttachment(attachment hostedstore.Attachment) error {
	if attachment.Provider == "claude" {
		if err := sessions.CloseDetachedClaude(s.ClaudeShimWS, attachment.ShimSessionID); err != nil {
			return fmt.Errorf("Claude stop unconfirmed; attachment retained (heartbeat may be paused): %w", err)
		}
	}
	return s.Attachments.ForgetClosed(attachment)
}
