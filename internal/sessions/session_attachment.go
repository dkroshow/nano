package sessions

import "errors"

// VerifyClaudeAttachment prevents closing a different shim child than the saved record.
func (sess *Session) VerifyClaudeAttachment(shimSessionID string) error {
	if c := sess.claudeHandle(); c != nil {
		if tr, ok := c.tr.(*claudeShimTransport); ok && shimSessionID != tr.shimSessionID {
			return errors.New("Claude attachment changed; close refused")
		}
	}
	return nil
}
