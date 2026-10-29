package sessions

import "errors"

// VerifyClaudeAttachment prevents closing a different shim child than the saved record.
func (teardown *Teardown) VerifyClaudeAttachment(shimSessionID string) error {
	if c := teardown.state.claudeHandle(); c != nil {
		if tr, ok := c.tr.(*claudeShimTransport); ok && shimSessionID != tr.shimSessionID {
			return errors.New("Claude attachment changed; close refused")
		}
	}
	return nil
}
