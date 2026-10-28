package app

import (
	"fmt"

	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

// Explicit operator escape: forget bookkeeping only, never claim termination
// or settle queued work. Refuse an attachment that has changed or resumed.
func (s *server) forgetUnconfirmedClaude(attachment attachmentRecord, shimID, attachedAt string) error {
	if attachment.ShimSessionID != shimID || attachment.AttachedAt != attachedAt {
		return fmt.Errorf("attachment changed; refresh before forgetting")
	}
	return s.ForgetDetachedClaude(attachment.NativeThreadID, func() error {
		return s.attachments.forgetClosed(attachment)
	})
}

// All helpers require relationshipMu. Legacy UI endpoints use the hosted
// lifecycle when possible, including heartbeat pause.
func (s *server) closeExplicitSession(sess *sessionruntime.Session) error {
	current := s.LookupSession(sess.ID())
	if current != sess {
		return fmt.Errorf("session attachment changed; refresh before closing")
	}
	if s.agentExecutions != nil && sess.HostedThreadID() != "" {
		if record, ok := s.resolveHostedThreadRecord(sess.HostedThreadID()); ok {
			if err := s.closeHostedThreadForRecord(record); err != nil {
				return err
			}
			return s.dropQueuedTurns(record)
		}
	}
	return s.closeAgentSession(sess)
}

func (s *server) closeExplicitAttachment(attachment attachmentRecord) error {
	if s.agentExecutions != nil && attachment.HostedThreadID != "" {
		if record, ok := s.resolveHostedThreadRecord(attachment.HostedThreadID); ok {
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
	live := s.AttachmentSession(attachment.Provider, attachment.NativeThreadID)
	if live != nil {
		return s.closeAgentSession(live)
	}
	return s.closeDetachedAttachment(attachment)
}

func (s *server) closeDetachedAttachment(attachment attachmentRecord) error {
	if attachment.Provider == "claude" {
		if err := sessionruntime.CloseDetachedClaude(s.defaults.claudeShimWS, attachment.ShimSessionID); err != nil {
			return fmt.Errorf("Claude stop unconfirmed; attachment retained (heartbeat may be paused): %w", err)
		}
	}
	return s.attachments.forgetClosed(attachment)
}
