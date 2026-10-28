package app

import "errors"

// verifyClaudeAttachment prevents closing a different shim child than the saved record.
func (sess *session) verifyClaudeAttachment(shimSessionID string) error {
	if c := sess.claudeHandle(); c != nil {
		if tr, ok := c.tr.(*claudeShimTransport); ok && shimSessionID != tr.shimSessionID {
			return errors.New("Claude attachment changed; close refused")
		}
	}
	return nil
}
