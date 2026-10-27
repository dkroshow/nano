package app

import "fmt"

// Explicit operator escape: forget bookkeeping only, never claim termination
// or settle queued work. Refuse an attachment that has changed or resumed.
func (s *server) forgetUnconfirmedClaude(attachment attachmentRecord, shimID, attachedAt string) error {
	if attachment.ShimSessionID != shimID || attachment.AttachedAt != attachedAt {
		return fmt.Errorf("attachment changed; refresh before forgetting")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.provider == "claude" && sess.currentThreadID() == attachment.NativeThreadID && sess.summary().Status != sessionStatusExited {
			return fmt.Errorf("thread is attached; end its session before forgetting")
		}
	}
	return s.attachments.forgetClosed(attachment)
}

// All helpers require relationshipMu. Legacy UI endpoints use the hosted
// lifecycle when possible, including heartbeat pause.
func (s *server) closeExplicitSession(sess *session) error {
	s.mu.Lock()
	current := s.sessions[sess.id]
	s.mu.Unlock()
	if current != sess {
		return fmt.Errorf("session attachment changed; refresh before closing")
	}
	if s.agentExecutions != nil && sess.hostedThreadID != "" {
		if record, ok := s.resolveHostedThreadRecord(sess.hostedThreadID); ok {
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
	s.mu.Lock()
	var live *session
	for _, sess := range s.sessions {
		if sess.provider == attachment.Provider && sess.currentThreadID() == attachment.NativeThreadID {
			live = sess
			break
		}
	}
	s.mu.Unlock()
	if live != nil {
		return s.closeAgentSession(live)
	}
	return s.closeDetachedAttachment(attachment)
}

func (s *server) closeDetachedAttachment(attachment attachmentRecord) error {
	if attachment.Provider == "claude" {
		if err := closeDetachedClaude(s.defaults.claudeShimWS, attachment.ShimSessionID); err != nil {
			return fmt.Errorf("Claude stop unconfirmed; attachment retained (heartbeat may be paused): %w", err)
		}
	}
	return s.attachments.forgetClosed(attachment)
}
